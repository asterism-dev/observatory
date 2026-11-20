package store

import (
	"context"
	"errors"
	"net/netip"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/asterism-dev/observatory/internal/authz"
	"github.com/asterism-dev/observatory/internal/proto"
)

var (
	ErrLimitExceeded = errors.New("limit exceeded")
	ErrOwnsResources = errors.New("owns VMs or disks")
	ErrNotEnrolled   = errors.New("node not enrolled")
	ErrNotRunning    = errors.New("VM is not running")
	ErrLegacyRuntime = errors.New("created with the old guest runtime; recreate this VM")
)

type VM struct {
	ID, OrgID, NodeID  uuid.UUID
	UserID, TeamID     *uuid.UUID
	Name               string
	CPUs, MemoryMB     int
	DiskGB             int
	Power              proto.Power
	StopMode           proto.StopMode
	Generation         uint64
	Provisioned        bool
	State              proto.VMState
	Error, IP          string
	MgmtIP             netip.Addr
	CreatedBy          uuid.UUID
	CreatedAt          time.Time
	AutoSuspendMinutes int
	WakeSeq            uint64
	Image              string
	RestoreState       string
	LegacyRuntime      bool
	// ImageRef is the image reference the VM was created with, ImageDigest the version it runs; empty digest means the platform image.
	ImageRef, ImageDigest string
	// ImageID is nil for the platform image and once the image is deleted; ImageSource says how the reference was chosen.
	ImageID     *uuid.UUID
	ImageSource string
	// Kind is VMKindBuilder for the hidden VMs that run image builds.
	Kind     string
	DeleteAt *time.Time
}

const (
	VMKindVM      = "vm"
	VMKindBuilder = "builder"
)

const vmColumns = "id, org_id, node_id, user_id, team_id, name, cpus, memory_mb, disk_gb, power, coalesce(stop_mode, ''), generation, provisioned, state, error, ip, mgmt_ip, coalesce(created_by, '00000000-0000-0000-0000-000000000000'::uuid), created_at, auto_suspend_minutes, wake_seq, image, restore_state, legacy_runtime, coalesce(image_ref, ''), coalesce(image_digest, ''), image_id, coalesce(image_source, ''), kind, delete_at"

func scanVM(row pgx.Row) (v VM, err error) {
	err = row.Scan(&v.ID, &v.OrgID, &v.NodeID, &v.UserID, &v.TeamID, &v.Name, &v.CPUs, &v.MemoryMB, &v.DiskGB,
		&v.Power, &v.StopMode, &v.Generation, &v.Provisioned, &v.State, &v.Error, &v.IP, &v.MgmtIP, &v.CreatedBy, &v.CreatedAt, &v.AutoSuspendMinutes, &v.WakeSeq, &v.Image, &v.RestoreState, &v.LegacyRuntime, &v.ImageRef, &v.ImageDigest, &v.ImageID, &v.ImageSource, &v.Kind, &v.DeleteAt)
	if errors.Is(err, pgx.ErrNoRows) {
		err = ErrNotFound
	}
	return
}

func collectVMs(rows pgx.Rows, err error) ([]VM, error) {
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (VM, error) { return scanVM(r) })
}

// usage sums the VMs matching the filter, whatever their power state, plus live standalone disks.
func (s *Store) usage(ctx context.Context, orgID uuid.UUID, ownerCol string, owner *uuid.UUID) (u Limits, err error) {
	var count, cpus, mem, disk, snaps int
	filter := ""
	args := []any{orgID}
	if owner != nil {
		filter = " AND " + ownerCol + " = $2"
		args = append(args, *owner)
	}
	q := "SELECT count(*), coalesce(sum(cpus), 0), coalesce(sum(memory_mb), 0), coalesce(sum(disk_gb), 0) + " +
		"(SELECT coalesce(sum(size_gb), 0) FROM disks WHERE org_id = $1 AND deleted_at IS NULL" + filter + "), " +
		"(SELECT count(*) FROM vm_snapshots sn JOIN vms ON vms.id = sn.vm_id WHERE vms.org_id = $1" + filter + ") " +
		"FROM vms WHERE org_id = $1" + filter
	if err = s.q.QueryRow(ctx, q, args...).Scan(&count, &cpus, &mem, &disk, &snaps); err != nil {
		return
	}
	return Limits{VCPU: &cpus, RAMMB: &mem, DiskGB: &disk, VMCount: &count, SnapshotCount: &snaps}, nil
}

// plus leaves a dimension nil (unchecked) when the addend has none, so an operation is only held to the limits it uses.
func (u Limits) plus(a Limits) Limits {
	add := func(used, n *int) *int {
		if n == nil {
			return nil
		}
		return ptr(*used + *n)
	}
	return Limits{VCPU: add(u.VCPU, a.VCPU), RAMMB: add(u.RAMMB, a.RAMMB), DiskGB: add(u.DiskGB, a.DiskGB), VMCount: add(u.VMCount, a.VMCount),
		SnapshotCount: add(u.SnapshotCount, a.SnapshotCount)}
}

// checkLimits checks that the owner belongs to the org and that adding add stays within the org and owner limits.
func (s *Store) checkLimits(ctx context.Context, orgID uuid.UUID, userID, teamID *uuid.UUID, add Limits) error {
	ownerScope, ownerCol, owner := "", "team_id", teamID
	switch {
	case userID != nil && teamID == nil:
		ownerScope, ownerCol, owner = authz.UserSubject(*userID), "user_id", userID
	case teamID != nil && userID == nil:
		ownerScope = authz.TeamSubject(*teamID)
	default:
		return ErrNotFound
	}
	if err := s.checkScopeInOrg(ctx, orgID, ownerScope); err != nil {
		return err
	}
	limits, err := s.Limits(ctx, orgID)
	if err != nil {
		return err
	}
	for _, c := range []struct {
		scope string
		owner *uuid.UUID
	}{{ScopeOrg, nil}, {ownerScope, owner}} {
		l, ok := limits[c.scope]
		if !ok {
			continue
		}
		used, err := s.usage(ctx, orgID, ownerCol, c.owner)
		if err != nil {
			return err
		}
		if err := used.plus(add).exceeded(c.scope, l); err != nil {
			return err
		}
	}
	return nil
}

func ptr(n int) *int { return &n }

func (s *Store) CreateVM(ctx context.Context, v VM) (VM, error) {
	var out VM
	err := s.InTx(ctx, func(tx *Store) error {
		if err := tx.LockOrg(ctx, v.OrgID); err != nil {
			return err
		}
		var enrolled bool
		if err := tx.q.QueryRow(ctx, "SELECT enrolled_at IS NOT NULL FROM nodes WHERE id = $1 AND org_id = $2", v.NodeID, v.OrgID).Scan(&enrolled); errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		} else if err != nil {
			return err
		}
		if !enrolled {
			return ErrNotEnrolled
		}
		if err := tx.checkLimits(ctx, v.OrgID, v.UserID, v.TeamID, Limits{VCPU: &v.CPUs, RAMMB: &v.MemoryMB, DiskGB: &v.DiskGB, VMCount: ptr(1)}); err != nil {
			return err
		}
		mgmtIP, err := tx.nextMgmtIP(ctx, v.OrgID)
		if err != nil {
			return err
		}
		out, err = scanVM(tx.q.QueryRow(ctx, `
			INSERT INTO vms (org_id, node_id, user_id, team_id, name, cpus, memory_mb, disk_gb, created_by, mgmt_ip, image_ref, image_digest, image_id, image_source, kind, delete_at)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, nullif($11, ''), nullif($12, ''), $13, nullif($14, ''), coalesce(nullif($15, ''), 'vm'), $16) RETURNING `+vmColumns,
			v.OrgID, v.NodeID, v.UserID, v.TeamID, v.Name, v.CPUs, v.MemoryMB, v.DiskGB, v.CreatedBy, mgmtIP, v.ImageRef, v.ImageDigest, v.ImageID, v.ImageSource, v.Kind, v.DeleteAt))
		if isUniqueViolation(err) {
			return ErrConflict
		}
		return err
	})
	return out, err
}

// VMs leaves out builder VMs.
func (s *Store) VMs(ctx context.Context, org uuid.UUID) ([]VM, error) {
	return collectVMs(s.q.Query(ctx, "SELECT "+vmColumns+" FROM vms WHERE org_id = $1 AND kind = 'vm' ORDER BY name", org))
}

func (s *Store) BuilderVMs(ctx context.Context, org uuid.UUID) ([]VM, error) {
	return collectVMs(s.q.Query(ctx, "SELECT "+vmColumns+" FROM vms WHERE org_id = $1 AND kind = 'builder' ORDER BY created_at", org))
}

// OrphanBuilderVMs are builder VMs no queued or running build holds.
func (s *Store) OrphanBuilderVMs(ctx context.Context) ([]VM, error) {
	return collectVMs(s.q.Query(ctx, "SELECT "+vmColumns+` FROM vms v WHERE kind = 'builder'
		AND NOT EXISTS (SELECT 1 FROM image_builds b WHERE b.vm_id = v.id AND b.state IN ('queued', 'running'))`))
}

func (s *Store) VM(ctx context.Context, org, id uuid.UUID) (VM, error) {
	return scanVM(s.q.QueryRow(ctx, "SELECT "+vmColumns+" FROM vms WHERE id = $1 AND org_id = $2", id, org))
}

func (s *Store) VMsOnNode(ctx context.Context, node uuid.UUID) ([]VM, error) {
	return collectVMs(s.q.Query(ctx, "SELECT "+vmColumns+" FROM vms WHERE node_id = $1 ORDER BY name", node))
}

// SetVMPower refuses to pause or hibernate a stopped VM with ErrNotRunning, and to start a legacy-runtime VM with ErrLegacyRuntime.
func (s *Store) SetVMPower(ctx context.Context, org, id uuid.UUID, p proto.Power, m proto.StopMode) (VM, error) {
	v, err := scanVM(s.q.QueryRow(ctx, `
		UPDATE vms SET power = $3, stop_mode = nullif($4, ''), generation = generation + 1,
			wake_seq = wake_seq + CASE WHEN $3 = 'running' AND state = 'suspended' THEN 1 ELSE 0 END
		WHERE id = $1 AND org_id = $2 AND NOT ($3 IN ('paused', 'hibernated') AND power = 'stopped') AND NOT ($3 = 'running' AND legacy_runtime)
		RETURNING `+vmColumns, id, org, p, m))
	if errors.Is(err, ErrNotFound) {
		cur, verr := s.VM(ctx, org, id)
		if verr != nil {
			return VM{}, verr
		}
		if p == proto.PowerRunning && cur.LegacyRuntime {
			return VM{}, ErrLegacyRuntime
		}
		if p == proto.PowerPaused || p == proto.PowerHibernated {
			return VM{}, ErrNotRunning
		}
	}
	return v, err
}

// SetAutoSuspend does not bump the generation: nodes read the setting on every push.
func (s *Store) SetAutoSuspend(ctx context.Context, org, id uuid.UUID, minutes int) (VM, error) {
	return scanVM(s.q.QueryRow(ctx, `
		UPDATE vms SET auto_suspend_minutes = $3 WHERE id = $1 AND org_id = $2
		RETURNING `+vmColumns, id, org, minutes))
}

// DeleteVM marks the exports of the VM's disks releasing; the cascade drops the attachments.
// It records the VM in vm_history, except builder VMs; by is nil for automatic deletions.
func (s *Store) DeleteVM(ctx context.Context, org, id uuid.UUID, by *uuid.UUID, reason string) error {
	return s.InTx(ctx, func(tx *Store) error {
		// The node deletes the VM's snapshot files, which a fork not yet built still copies from.
		var forkPending bool
		err := tx.q.QueryRow(ctx, `
			SELECT EXISTS (SELECT 1 FROM vms f WHERE f.fork_source_vm = v.id AND NOT f.provisioned)
			FROM vms v WHERE v.id = $1 AND v.org_id = $2 FOR UPDATE`, id, org).Scan(&forkPending)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		} else if err != nil {
			return err
		}
		if forkPending {
			return ErrConflict
		}
		if _, err := tx.q.Exec(ctx, `
			UPDATE disk_exports SET releasing = true, release_seq = NULL
			WHERE disk_id IN (SELECT disk_id FROM vm_disks WHERE vm_id = $1)`, id); err != nil {
			return err
		}
		if _, err := tx.q.Exec(ctx, `
			INSERT INTO vm_history (id, org_id, user_id, team_id, node_id, name, cpus, memory_mb, disk_gb, image_ref, created_by, created_at, deleted_by, reason)
			SELECT id, org_id, user_id, team_id, node_id, name, cpus, memory_mb, disk_gb, coalesce(image_ref, ''), created_by, created_at, $2, $3
			FROM vms WHERE id = $1 AND kind = 'vm'`, id, by, reason); err != nil {
			return err
		}
		_, err = tx.q.Exec(ctx, "DELETE FROM vms WHERE id = $1", id)
		return err
	})
}

// SetDeleteAt does not bump the generation: only the hub acts on it.
func (s *Store) SetDeleteAt(ctx context.Context, org, id uuid.UUID, at *time.Time) (VM, error) {
	return scanVM(s.q.QueryRow(ctx, `
		UPDATE vms SET delete_at = $3 WHERE id = $1 AND org_id = $2
		RETURNING `+vmColumns, id, org, at))
}

func (s *Store) ExpiredVMs(ctx context.Context, now time.Time) ([]VM, error) {
	return collectVMs(s.q.Query(ctx, "SELECT "+vmColumns+" FROM vms WHERE kind = 'vm' AND delete_at <= $1 ORDER BY delete_at", now))
}

// LockExpiredVM returns ErrNotFound unless the VM is still due at now, so a cleared or extended expiry wins.
func (s *Store) LockExpiredVM(ctx context.Context, id uuid.UUID, now time.Time) error {
	err := s.q.QueryRow(ctx, "SELECT id FROM vms WHERE id = $1 AND delete_at <= $2 FOR UPDATE", id, now).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrNotFound
	}
	return err
}

// SetVMStatus ignores VMs that are not on the reporting node.
func (s *Store) SetVMStatus(ctx context.Context, node uuid.UUID, st proto.VMStatus) error {
	id, err := uuid.Parse(st.ID)
	if err != nil {
		return nil
	}
	_, err = s.q.Exec(ctx, `
		UPDATE vms SET state = $3, ip = $4, error = $5,
			image = CASE WHEN $6 <> '' THEN $6 ELSE image END,
			provisioned = provisioned OR $3 IN ('starting', 'running', 'stopped', 'paused', 'hibernated', 'suspended')
		WHERE id = $1 AND node_id = $2`, id, node, st.State, st.IP, st.Error, st.Image)
	return err
}

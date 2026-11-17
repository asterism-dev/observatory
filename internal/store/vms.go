package store

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/asterism-dev/observatory/internal/authz"
	"github.com/asterism-dev/observatory/internal/proto"
)

var (
	ErrLimitExceeded = errors.New("limit exceeded")
	ErrOwnsResources = errors.New("owns VMs")
)

type VM struct {
	ID, OrgID, NodeID uuid.UUID
	UserID, TeamID    *uuid.UUID
	Name              string
	CPUs, MemoryMB    int
	DiskGB            int
	Power             proto.Power
	StopMode          proto.StopMode
	Generation        uint64
	Provisioned       bool
	State             proto.VMState
	Error, IP         string
	CreatedBy         uuid.UUID
	CreatedAt         time.Time
}

const vmColumns = "id, org_id, node_id, user_id, team_id, name, cpus, memory_mb, disk_gb, power, coalesce(stop_mode, ''), generation, provisioned, state, error, ip, coalesce(created_by, '00000000-0000-0000-0000-000000000000'::uuid), created_at"

func scanVM(row pgx.Row) (v VM, err error) {
	err = row.Scan(&v.ID, &v.OrgID, &v.NodeID, &v.UserID, &v.TeamID, &v.Name, &v.CPUs, &v.MemoryMB, &v.DiskGB,
		&v.Power, &v.StopMode, &v.Generation, &v.Provisioned, &v.State, &v.Error, &v.IP, &v.CreatedBy, &v.CreatedAt)
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

// vmUsage sums the VMs matching the filter, whatever their power state.
func (s *Store) vmUsage(ctx context.Context, orgID uuid.UUID, ownerCol string, owner *uuid.UUID) (u Limits, err error) {
	var count, cpus, mem, disk int
	q := "SELECT count(*), coalesce(sum(cpus), 0), coalesce(sum(memory_mb), 0), coalesce(sum(disk_gb), 0) FROM vms WHERE org_id = $1"
	args := []any{orgID}
	if owner != nil {
		q += " AND " + ownerCol + " = $2"
		args = append(args, *owner)
	}
	if err = s.q.QueryRow(ctx, q, args...).Scan(&count, &cpus, &mem, &disk); err != nil {
		return
	}
	return Limits{VCPU: &cpus, RAMMB: &mem, DiskGB: &disk, VMCount: &count}, nil
}

func (u Limits) plus(v VM) Limits {
	return Limits{VCPU: ptr(*u.VCPU + v.CPUs), RAMMB: ptr(*u.RAMMB + v.MemoryMB), DiskGB: ptr(*u.DiskGB + v.DiskGB), VMCount: ptr(*u.VMCount + 1)}
}

func ptr(n int) *int { return &n }

func (s *Store) CreateVM(ctx context.Context, v VM) (VM, error) {
	var out VM
	err := s.InTx(ctx, func(tx *Store) error {
		if err := tx.LockOrg(ctx, v.OrgID); err != nil {
			return err
		}
		var one int
		if err := tx.q.QueryRow(ctx, "SELECT 1 FROM nodes WHERE id = $1 AND org_id = $2", v.NodeID, v.OrgID).Scan(&one); errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		} else if err != nil {
			return err
		}
		ownerScope, ownerCol, owner := "", "team_id", v.TeamID
		switch {
		case v.UserID != nil && v.TeamID == nil:
			ownerScope, ownerCol, owner = authz.UserSubject(*v.UserID), "user_id", v.UserID
		case v.TeamID != nil && v.UserID == nil:
			ownerScope = authz.TeamSubject(*v.TeamID)
		default:
			return ErrNotFound
		}
		if err := tx.checkScopeInOrg(ctx, v.OrgID, ownerScope); err != nil {
			return err
		}
		limits, err := tx.Limits(ctx, v.OrgID)
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
			used, err := tx.vmUsage(ctx, v.OrgID, ownerCol, c.owner)
			if err != nil {
				return err
			}
			if used.plus(v).exceeds(l) {
				return ErrLimitExceeded
			}
		}
		out, err = scanVM(tx.q.QueryRow(ctx, `
			INSERT INTO vms (org_id, node_id, user_id, team_id, name, cpus, memory_mb, disk_gb, created_by)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9) RETURNING `+vmColumns,
			v.OrgID, v.NodeID, v.UserID, v.TeamID, v.Name, v.CPUs, v.MemoryMB, v.DiskGB, v.CreatedBy))
		if isUniqueViolation(err) {
			return ErrConflict
		}
		return err
	})
	return out, err
}

func (s *Store) VMs(ctx context.Context, org uuid.UUID) ([]VM, error) {
	return collectVMs(s.q.Query(ctx, "SELECT "+vmColumns+" FROM vms WHERE org_id = $1 ORDER BY name", org))
}

func (s *Store) VM(ctx context.Context, org, id uuid.UUID) (VM, error) {
	return scanVM(s.q.QueryRow(ctx, "SELECT "+vmColumns+" FROM vms WHERE id = $1 AND org_id = $2", id, org))
}

func (s *Store) VMsOnNode(ctx context.Context, node uuid.UUID) ([]VM, error) {
	return collectVMs(s.q.Query(ctx, "SELECT "+vmColumns+" FROM vms WHERE node_id = $1 ORDER BY name", node))
}

func (s *Store) SetVMPower(ctx context.Context, org, id uuid.UUID, p proto.Power, m proto.StopMode) (VM, error) {
	return scanVM(s.q.QueryRow(ctx, `
		UPDATE vms SET power = $3, stop_mode = nullif($4, ''), generation = generation + 1
		WHERE id = $1 AND org_id = $2 RETURNING `+vmColumns, id, org, p, m))
}

func (s *Store) DeleteVM(ctx context.Context, org, id uuid.UUID) error {
	tag, err := s.q.Exec(ctx, "DELETE FROM vms WHERE id = $1 AND org_id = $2", id, org)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// SetVMStatus ignores VMs that are not on the reporting node.
func (s *Store) SetVMStatus(ctx context.Context, node uuid.UUID, st proto.VMStatus) error {
	id, err := uuid.Parse(st.ID)
	if err != nil {
		return nil
	}
	_, err = s.q.Exec(ctx, `
		UPDATE vms SET state = $3, ip = $4, error = $5,
			provisioned = provisioned OR $3 IN ('starting', 'running', 'stopped')
		WHERE id = $1 AND node_id = $2`, id, node, st.State, st.IP, st.Error)
	return err
}

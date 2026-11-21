package store

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/asterism-dev/observatory/internal/proto"
)

const MaxSnapshotsPerVM = 10

// SnapshotCopyTimeout bounds how long a reported snapshot may wait for its storage copies; nodes keep the VM paused a bit longer.
const SnapshotCopyTimeout = 120 * time.Second

var ErrShapeChanged = errors.New("VM changed since the snapshot; restore without memory")

type Snapshot struct {
	ID, OrgID, VMID uuid.UUID
	Name            string
	Memory          bool
	State, Error    string
	SizeBytes       int64
	CPUs, MemoryMB  int
	DiskGB          int
	Networks        []uuid.UUID
	Attachments     []SnapshotAttachment
	Disks           []SnapshotDisk
	ReportedAt      *time.Time
	CreatedBy       uuid.UUID
	CreatedAt       time.Time
}

type SnapshotAttachment struct {
	DiskID    uuid.UUID `json:"diskId"`
	MountPath string    `json:"mountPath"`
	Position  int       `json:"position"`
}

type SnapshotDisk struct {
	ID, SourceDiskID          uuid.UUID
	DiskName, MountPath       string
	Position, SizeGB, ReadyGB int
}

const snapshotColumns = `id, org_id, vm_id, name, memory, state, error, size_bytes, cpus, memory_mb, disk_gb, networks, attachments,
	reported_at, coalesce(created_by, '00000000-0000-0000-0000-000000000000'::uuid), created_at`

func (s *Store) scanSnapshot(ctx context.Context, row pgx.Row) (sn Snapshot, err error) {
	err = row.Scan(&sn.ID, &sn.OrgID, &sn.VMID, &sn.Name, &sn.Memory, &sn.State, &sn.Error, &sn.SizeBytes, &sn.CPUs, &sn.MemoryMB,
		&sn.DiskGB, &sn.Networks, &sn.Attachments, &sn.ReportedAt, &sn.CreatedBy, &sn.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return sn, ErrNotFound
	} else if err != nil {
		return sn, err
	}
	rows, err := s.q.Query(ctx, `
		SELECT id, source_disk_id, disk_name, mount_path, position, size_gb, ready_gb FROM snapshot_disks
		WHERE snapshot_id = $1 AND deleted_at IS NULL ORDER BY position`, sn.ID)
	if err != nil {
		return sn, err
	}
	sn.Disks, err = pgx.CollectRows(rows, pgx.RowToStructByPos[SnapshotDisk])
	return sn, err
}

func (s *Store) Snapshots(ctx context.Context, org, vm uuid.UUID) ([]Snapshot, error) {
	ids, err := collectIDs(s.q.Query(ctx, "SELECT id FROM vm_snapshots WHERE org_id = $1 AND vm_id = $2 ORDER BY created_at, name", org, vm))
	if err != nil {
		return nil, err
	}
	out := make([]Snapshot, 0, len(ids))
	for _, id := range ids {
		sn, err := s.Snapshot(ctx, org, vm, id)
		if err != nil {
			return nil, err
		}
		out = append(out, sn)
	}
	return out, nil
}

func (s *Store) Snapshot(ctx context.Context, org, vm, id uuid.UUID) (Snapshot, error) {
	return s.scanSnapshot(ctx, s.q.QueryRow(ctx, "SELECT "+snapshotColumns+" FROM vm_snapshots WHERE id = $1 AND vm_id = $2 AND org_id = $3", id, vm, org))
}

// rankedNetworks lists the VM's networks in NIC order (by network name, as the fleet ranks them).
func (s *Store) rankedNetworks(ctx context.Context, vm uuid.UUID) ([]uuid.UUID, error) {
	ids, err := collectIDs(s.q.Query(ctx, `
		SELECT vn.network_id FROM vm_networks vn JOIN networks n ON n.id = vn.network_id WHERE vn.vm_id = $1 ORDER BY n.name`, vm))
	if ids == nil {
		ids = []uuid.UUID{}
	}
	return ids, err
}

func (s *Store) snapshotAttachments(ctx context.Context, vm uuid.UUID) ([]SnapshotAttachment, error) {
	rows, err := s.q.Query(ctx, "SELECT disk_id, mount_path, position FROM vm_disks WHERE vm_id = $1 ORDER BY position", vm)
	if err != nil {
		return nil, err
	}
	atts, err := pgx.CollectRows(rows, pgx.RowToStructByPos[SnapshotAttachment])
	if atts == nil {
		atts = []SnapshotAttachment{}
	}
	return atts, err
}

// CreateSnapshot records the VM's shape now; a stopped VM has no memory to keep.
func (s *Store) CreateSnapshot(ctx context.Context, org, vm uuid.UUID, name string, memory bool, createdBy uuid.UUID) (Snapshot, error) {
	var out Snapshot
	err := s.InTx(ctx, func(tx *Store) error {
		if err := tx.LockHubOrg(ctx, org); err != nil {
			return err
		}
		var restoreState string
		err := tx.q.QueryRow(ctx, "SELECT restore_state FROM vms WHERE id = $1 AND org_id = $2 FOR UPDATE", vm, org).Scan(&restoreState)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		} else if err != nil {
			return err
		}
		// A restore rewrites the root disk and attached disks underneath the snapshot.
		if restoreState == "node" || restoreState == "storage" {
			return ErrConflict
		}
		v, err := tx.VM(ctx, org, vm)
		if err != nil {
			return err
		}
		var n int
		if err := tx.q.QueryRow(ctx, "SELECT count(*) FROM vm_snapshots WHERE vm_id = $1", vm).Scan(&n); err != nil {
			return err
		}
		if n >= MaxSnapshotsPerVM {
			return ErrLimitExceeded
		}
		if err := tx.checkLimits(ctx, org, v.UserID, v.TeamID, Limits{SnapshotCount: ptr(1)}); err != nil {
			return err
		}
		nets, err := tx.rankedNetworks(ctx, vm)
		if err != nil {
			return err
		}
		atts, err := tx.snapshotAttachments(ctx, vm)
		if err != nil {
			return err
		}
		var id uuid.UUID
		err = tx.q.QueryRow(ctx, `
			INSERT INTO vm_snapshots (org_id, vm_id, name, memory, cpus, memory_mb, disk_gb, networks, attachments, created_by)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10) RETURNING id`,
			org, vm, name, memory && v.Power != proto.PowerStopped, v.CPUs, v.MemoryMB, v.DiskGB, nets, atts, createdBy).Scan(&id)
		if isUniqueViolation(err) {
			return ErrConflict
		} else if err != nil {
			return err
		}
		out, err = tx.Snapshot(ctx, org, vm, id)
		return err
	})
	return out, err
}

// DeleteSnapshot refuses while a restore or fork still needs the snapshot's files on the node or storage server.
func (s *Store) DeleteSnapshot(ctx context.Context, org, vm, id uuid.UUID) error {
	return s.InTx(ctx, func(tx *Store) error {
		if err := tx.LockHubOrg(ctx, org); err != nil {
			return err
		}
		var busy bool
		err := tx.q.QueryRow(ctx, `
			SELECT EXISTS (SELECT 1 FROM disks d JOIN snapshot_disks sd ON sd.id IN (d.copy_from_snapshot, d.restore_from)
					WHERE sd.snapshot_id = s.id AND d.deleted_at IS NULL)
				OR EXISTS (SELECT 1 FROM vms v WHERE v.restore_snapshot_id = s.id AND v.restore_state IN ('node', 'storage'))
				OR EXISTS (SELECT 1 FROM vms v WHERE v.fork_snapshot_id = s.id AND NOT v.provisioned)
			FROM vm_snapshots s WHERE s.id = $1 AND s.vm_id = $2 AND s.org_id = $3`, id, vm, org).Scan(&busy)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		} else if err != nil {
			return err
		}
		if busy {
			return ErrConflict
		}
		if _, err := tx.q.Exec(ctx, "UPDATE snapshot_disks SET deleted_at = now() WHERE snapshot_id = $1 AND deleted_at IS NULL", id); err != nil {
			return err
		}
		_, err = tx.q.Exec(ctx, "DELETE FROM vm_snapshots WHERE id = $1", id)
		return err
	})
}

// ApplySnapshotStatus records the node's snapshot and restore progress; changed means the node's desired list changed.
func (s *Store) ApplySnapshotStatus(ctx context.Context, node uuid.UUID, st proto.VMStatus) (changed bool, err error) {
	vm, err := uuid.Parse(st.ID)
	if err != nil || (len(st.Snapshots) == 0 && st.RestoredSeq == 0) {
		return false, nil
	}
	err = s.InTx(ctx, func(tx *Store) error {
		var org uuid.UUID
		err := tx.q.QueryRow(ctx, "SELECT org_id FROM vms WHERE id = $1 AND node_id = $2", vm, node).Scan(&org)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		} else if err != nil {
			return err
		}
		if err := tx.LockHubOrg(ctx, org); err != nil {
			return err
		}
		var restoreSeq int64
		var restoreState string
		var restoreSnap *uuid.UUID
		err = tx.q.QueryRow(ctx, "SELECT restore_seq, restore_state, restore_snapshot_id FROM vms WHERE id = $1 AND node_id = $2 FOR UPDATE", vm, node).
			Scan(&restoreSeq, &restoreState, &restoreSnap)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		} else if err != nil {
			return err
		}
		for _, info := range st.Snapshots {
			id, err := uuid.Parse(info.ID)
			if err != nil {
				continue
			}
			const first = " WHERE id = $1 AND vm_id = $2 AND state = 'creating' AND reported_at IS NULL"
			if info.Error != "" {
				// Also after a report: a node that resumed the VM before completion fails the snapshot, whatever the copies did.
				var failed bool
				err := tx.q.QueryRow(ctx, `
					WITH failed AS (
						UPDATE vm_snapshots SET state = 'error', error = left($3, 500), reported_at = coalesce(reported_at, now())
						WHERE id = $1 AND vm_id = $2 AND state IN ('creating', 'ready') RETURNING id),
					copies AS (UPDATE snapshot_disks SET deleted_at = now() WHERE snapshot_id IN (SELECT id FROM failed) AND deleted_at IS NULL)
					SELECT EXISTS (SELECT 1 FROM failed)`, id, vm, info.Error).Scan(&failed)
				if err != nil {
					return err
				}
				changed = changed || failed
				continue
			}
			tag, err := tx.q.Exec(ctx, "UPDATE vm_snapshots SET reported_at = now(), size_bytes = greatest($3::bigint, 0), memory = memory AND $4"+first,
				id, vm, info.SizeBytes, info.Memory)
			if err != nil {
				return err
			}
			if tag.RowsAffected() == 0 {
				continue
			}
			changed = true
			// The guest is paused and flushed now, so copies of the attached disks are consistent.
			tag, err = tx.q.Exec(ctx, `
				INSERT INTO snapshot_disks (snapshot_id, source_disk_id, disk_name, mount_path, position, size_gb)
				SELECT $1, d.id, d.name, vd.mount_path, vd.position, d.ready_gb
				FROM vm_disks vd JOIN disks d ON d.id = vd.disk_id WHERE vd.vm_id = $2 AND d.ready_gb > 0`, id, vm)
			if err != nil {
				return err
			}
			if tag.RowsAffected() == 0 {
				if _, err := tx.q.Exec(ctx, "UPDATE vm_snapshots SET state = 'ready' WHERE id = $1", id); err != nil {
					return err
				}
			}
		}
		if st.RestoredSeq == 0 || int64(st.RestoredSeq) != restoreSeq || restoreState != "node" {
			return nil
		}
		changed = true
		tag, err := tx.q.Exec(ctx, `
			UPDATE disks d SET restore_from = sd.id, restore_token = gen_random_uuid()::text
			FROM snapshot_disks sd JOIN vm_disks vd ON vd.disk_id = sd.source_disk_id
			WHERE d.id = sd.source_disk_id AND vd.vm_id = $1 AND sd.snapshot_id = $2
				AND sd.deleted_at IS NULL AND sd.ready_gb > 0 AND d.restore_token IS NULL`, vm, restoreSnap)
		if err != nil {
			return err
		}
		if tag.RowsAffected() > 0 {
			_, err = tx.q.Exec(ctx, "UPDATE vms SET restore_state = 'storage' WHERE id = $1", vm)
		} else {
			// The node applies Ready only with a new generation.
			_, err = tx.q.Exec(ctx, "UPDATE vms SET restore_state = 'done', generation = generation + 1 WHERE id = $1", vm)
		}
		return err
	})
	return changed, err
}

// finishRestores marks restores of the orgs' VMs done once none of their disks waits for the storage server.
func (s *Store) finishRestores(ctx context.Context, orgs []uuid.UUID) ([]uuid.UUID, error) {
	return collectIDs(s.q.Query(ctx, `
		UPDATE vms v SET restore_state = 'done', generation = generation + 1
		WHERE v.org_id = ANY($1) AND v.restore_state = 'storage' AND NOT EXISTS (
			SELECT 1 FROM disks d JOIN snapshot_disks sd ON sd.id = d.restore_from WHERE sd.snapshot_id = v.restore_snapshot_id)
		RETURNING v.org_id`, orgs))
}

// ApplyStorageRestored clears the restores the storage server finished and returns the orgs whose VMs' restores are done.
func (s *Store) ApplyStorageRestored(ctx context.Context, tokens []string) ([]uuid.UUID, error) {
	if len(tokens) == 0 {
		return nil, nil
	}
	var orgs []uuid.UUID
	err := s.InTx(ctx, func(tx *Store) error {
		candidates, err := collectIDs(tx.q.Query(ctx, "SELECT DISTINCT org_id FROM disks WHERE restore_token = ANY($1) ORDER BY 1", tokens))
		if err != nil || len(candidates) == 0 {
			return err
		}
		for _, org := range candidates {
			if err := tx.LockHubOrg(ctx, org); err != nil {
				return err
			}
		}
		if _, err := tx.q.Exec(ctx, "UPDATE disks SET restore_from = NULL, restore_token = NULL WHERE restore_token = ANY($1)", tokens); err != nil {
			return err
		}
		orgs, err = tx.finishRestores(ctx, candidates)
		return err
	})
	return uniqueIDs(orgs), err
}

// FailStaleSnapshots fails snapshots whose storage copies were not ready within timeout, so their nodes resume the VMs, and those no node ever reported.
func (s *Store) FailStaleSnapshots(ctx context.Context, timeout time.Duration) ([]uuid.UUID, error) {
	return collectIDs(s.q.Query(ctx, `
		WITH failed AS (
			UPDATE vm_snapshots SET state = 'error',
				error = CASE WHEN reported_at IS NULL THEN 'snapshot never reached the node' ELSE 'storage copy timed out' END
			WHERE state = 'creating' AND (reported_at < now() - make_interval(secs => $1)
				OR reported_at IS NULL AND created_at < now() - interval '10 minutes') RETURNING id, org_id),
		copies AS (UPDATE snapshot_disks SET deleted_at = now() WHERE snapshot_id IN (SELECT id FROM failed) AND deleted_at IS NULL)
		SELECT DISTINCT org_id FROM failed ORDER BY 1`, timeout.Seconds()))
}

// FailNodeSnapshots fails the node's snapshots still waiting for copies: a node that went away may have resumed their VMs.
func (s *Store) FailNodeSnapshots(ctx context.Context, node uuid.UUID) ([]uuid.UUID, error) {
	return collectIDs(s.q.Query(ctx, `
		WITH failed AS (
			UPDATE vm_snapshots s SET state = 'error', error = 'node disconnected before the snapshot completed'
			FROM vms v WHERE v.id = s.vm_id AND v.node_id = $1 AND s.state = 'creating' AND s.reported_at IS NOT NULL
			RETURNING s.id, s.org_id),
		copies AS (UPDATE snapshot_disks SET deleted_at = now() WHERE snapshot_id IN (SELECT id FROM failed) AND deleted_at IS NULL)
		SELECT DISTINCT org_id FROM failed ORDER BY 1`, node))
}

// MemoryRestorable reports whether sn has memory and v still has the vCPUs, RAM, networks and attachments it was taken with.
func (s *Store) MemoryRestorable(ctx context.Context, v VM, sn Snapshot) (bool, error) {
	if !sn.Memory || v.CPUs != sn.CPUs || v.MemoryMB != sn.MemoryMB {
		return false, nil
	}
	nets, err := s.rankedNetworks(ctx, v.ID)
	if err != nil {
		return false, err
	}
	atts, err := s.snapshotAttachments(ctx, v.ID)
	if err != nil {
		return false, err
	}
	return slices.Equal(nets, sn.Networks) && slices.Equal(atts, sn.Attachments), nil
}

// RequestRestore asks the node to roll the VM back to snap; a memory restore needs the VM's shape unchanged.
func (s *Store) RequestRestore(ctx context.Context, org, vm, snap uuid.UUID, memory, restartAllowed bool) (VM, error) {
	var out VM
	err := s.InTx(ctx, func(tx *Store) error {
		if err := tx.LockHubOrg(ctx, org); err != nil {
			return err
		}
		var restoreState string
		var snapshotting bool
		// A restore hard-stops the VM, which would cut a snapshot in progress short.
		if err := tx.q.QueryRow(ctx, `
			SELECT restore_state, EXISTS (SELECT 1 FROM vm_snapshots WHERE vm_id = v.id AND state = 'creating')
			FROM vms v WHERE id = $1 AND org_id = $2 FOR UPDATE`, vm, org).Scan(&restoreState, &snapshotting); errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		} else if err != nil {
			return err
		}
		v, err := tx.VM(ctx, org, vm)
		if err != nil {
			return err
		}
		sn, err := tx.Snapshot(ctx, org, vm, snap)
		if err != nil {
			return err
		}
		// A started storage restore must finish first: its token can't be replaced before the storage server reports it.
		if sn.State != "ready" || restoreState == "storage" || snapshotting {
			return ErrConflict
		}
		if v.Power != proto.PowerStopped && !restartAllowed {
			return ErrRestartRequired
		}
		if memory {
			ok, err := tx.MemoryRestorable(ctx, v, sn)
			if err != nil {
				return err
			}
			if sn.Memory && !ok {
				return ErrShapeChanged
			}
			memory = ok
		}
		out, err = scanVM(tx.q.QueryRow(ctx, `
			UPDATE vms SET restore_snapshot_id = $2, restore_memory = $3, restore_seq = restore_seq + 1, restore_state = 'node',
				generation = generation + 1
			WHERE id = $1 RETURNING `+vmColumns, vm, snap, memory))
		return err
	})
	return out, err
}

// ForkVM creates a VM from snap on the source's node with copies of the snapshot's disks; the node builds it once the copies are ready.
func (s *Store) ForkVM(ctx context.Context, org uuid.UUID, snap Snapshot, name string, memory bool, createdBy uuid.UUID) (VM, error) {
	var out VM
	err := s.InTx(ctx, func(tx *Store) error {
		if err := tx.LockHubOrg(ctx, org); err != nil {
			return err
		}
		src, err := scanVM(tx.q.QueryRow(ctx, "SELECT "+vmColumns+" FROM vms WHERE id = $1 AND org_id = $2 FOR UPDATE", snap.VMID, org))
		if err != nil {
			return err
		}
		sn, err := tx.Snapshot(ctx, org, snap.VMID, snap.ID)
		if err != nil {
			return err
		}
		if sn.State != "ready" {
			return ErrConflict
		}
		memory = memory && sn.Memory
		nets, err := tx.rankedNetworks(ctx, src.ID)
		if err != nil {
			return err
		}
		if memory && len(nets) != len(sn.Networks) {
			return ErrShapeChanged
		}
		// Root disk and memory state are the snapshot's, so the fork takes its shape, not the source's current one.
		diskGB := sn.DiskGB
		for _, d := range sn.Disks {
			diskGB += d.SizeGB
		}
		if err := tx.checkLimits(ctx, org, src.UserID, src.TeamID, Limits{VCPU: &sn.CPUs, RAMMB: &sn.MemoryMB, DiskGB: &diskGB, VMCount: ptr(1)}); err != nil {
			return err
		}
		mgmtIP, err := tx.nextMgmtIP(ctx, org)
		if err != nil {
			return err
		}
		var id uuid.UUID
		err = tx.q.QueryRow(ctx, `
			INSERT INTO vms (org_id, node_id, user_id, team_id, name, cpus, memory_mb, disk_gb, created_by, mgmt_ip,
				fork_snapshot_id, fork_source_vm, fork_memory, image_ref, image_digest, image_id, image_source)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, nullif($14, ''), nullif($15, ''), $16, nullif($17, '')) RETURNING id`,
			org, src.NodeID, src.UserID, src.TeamID, name, sn.CPUs, sn.MemoryMB, sn.DiskGB, createdBy, mgmtIP, sn.ID, src.ID, memory,
			src.ImageRef, src.ImageDigest, src.ImageID, src.ImageSource).Scan(&id)
		if isUniqueViolation(err) {
			return ErrConflict
		} else if err != nil {
			return err
		}
		if len(nets) > 0 {
			if _, err := tx.SetVMNetworks(ctx, org, id, nets, true); err != nil {
				return err
			}
		}
		var attach []VMDisk
		for _, d := range sn.Disks {
			suffix := make([]byte, 2)
			if _, err := rand.Read(suffix); err != nil {
				return err
			}
			var disk uuid.UUID
			err := tx.q.QueryRow(ctx, `
				INSERT INTO disks (org_id, user_id, team_id, name, size_gb, copy_from_snapshot, created_by)
				VALUES ($1, $2, $3, $4, $5, $6, $7) RETURNING id`,
				org, src.UserID, src.TeamID, forkDiskName(d.DiskName, hex.EncodeToString(suffix)), d.SizeGB, d.ID, createdBy).Scan(&disk)
			if isUniqueViolation(err) {
				return ErrConflict
			} else if err != nil {
				return err
			}
			attach = append(attach, VMDisk{DiskID: disk, MountPath: d.MountPath})
		}
		if len(attach) > 0 {
			if _, _, err := tx.SetVMDisks(ctx, org, id, attach, true); err != nil {
				return err
			}
		}
		if err := tx.copyEgressCAs(ctx, org, src.ID, id); err != nil {
			return err
		}
		out, err = tx.VM(ctx, org, id)
		return err
	})
	return out, err
}

// NodeSnapshots lists the snapshots the node's VMs keep, oldest first; failed ones are left out so the node drops their files.
func (s *Store) NodeSnapshots(ctx context.Context, node uuid.UUID) (map[uuid.UUID][]proto.SnapshotRef, error) {
	rows, err := s.q.Query(ctx, `
		SELECT s.vm_id, s.id, s.memory, s.state = 'ready' FROM vm_snapshots s JOIN vms v ON v.id = s.vm_id
		WHERE v.node_id = $1 AND s.state <> 'error' ORDER BY s.created_at, s.name`, node)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[uuid.UUID][]proto.SnapshotRef{}
	for rows.Next() {
		var vm, id uuid.UUID
		var ref proto.SnapshotRef
		if err := rows.Scan(&vm, &id, &ref.Memory, &ref.Complete); err != nil {
			return nil, err
		}
		ref.ID = id.String()
		out[vm] = append(out[vm], ref)
	}
	return out, rows.Err()
}

// VMPlan is a VM's pending restore or fork; Hold keeps a fork off its node until its disk copies are ready.
type VMPlan struct {
	Restore *proto.Restore
	Fork    *proto.Fork
	Hold    bool
}

// NodeVMPlans returns the plans of the node's VMs that have a restore or an unbuilt fork.
func (s *Store) NodeVMPlans(ctx context.Context, node uuid.UUID) (map[uuid.UUID]VMPlan, error) {
	rows, err := s.q.Query(ctx, `
		SELECT v.id, v.restore_snapshot_id, v.restore_seq, v.restore_memory, v.restore_state = 'done',
			CASE WHEN NOT v.provisioned THEN v.fork_snapshot_id END, v.fork_source_vm, v.fork_memory,
			NOT v.provisioned AND EXISTS (SELECT 1 FROM vm_disks vd JOIN disks d ON d.id = vd.disk_id
				WHERE vd.vm_id = v.id AND (d.copy_from_snapshot IS NOT NULL OR d.ready_gb = 0))
		FROM vms v WHERE v.node_id = $1 AND ((v.restore_seq > 0 AND v.restore_snapshot_id IS NOT NULL)
			OR (v.fork_snapshot_id IS NOT NULL AND NOT v.provisioned))`, node)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[uuid.UUID]VMPlan{}
	for rows.Next() {
		var vm uuid.UUID
		var restoreSnap, forkSnap, forkSource *uuid.UUID
		var r proto.Restore
		var forkMemory, hold bool
		if err := rows.Scan(&vm, &restoreSnap, &r.Seq, &r.Memory, &r.Ready, &forkSnap, &forkSource, &forkMemory, &hold); err != nil {
			return nil, err
		}
		var p VMPlan
		if restoreSnap != nil {
			r.SnapshotID = restoreSnap.String()
			p.Restore = &r
		}
		if forkSnap != nil && forkSource != nil {
			p.Fork = &proto.Fork{SourceVMID: forkSource.String(), SnapshotID: forkSnap.String(), Memory: forkMemory}
			p.Hold = hold
		}
		out[vm] = p
	}
	return out, rows.Err()
}

func uniqueIDs(ids []uuid.UUID) []uuid.UUID {
	slices.SortFunc(ids, func(a, b uuid.UUID) int { return strings.Compare(a.String(), b.String()) })
	return slices.Compact(ids)
}

var forkSuffix = regexp.MustCompile(`-[0-9a-f]{4}$`)

// forkDiskName keeps "<name>-<suffix>" within the 63 characters of a disk name, replacing an earlier fork's suffix.
func forkDiskName(name, suffix string) string {
	name = forkSuffix.ReplaceAllString(name, "")
	return name[:min(len(name), 62-len(suffix))] + "-" + suffix
}

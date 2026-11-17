package store

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"slices"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/asterism-dev/observatory/internal/netaddr"
	"github.com/asterism-dev/observatory/internal/proto"
	"github.com/asterism-dev/observatory/internal/seal"
)

var (
	ErrOwnerMismatch = errors.New("disk has another owner")
	ErrNoStoragePort = errors.New("no free storage port")
)

type VMDisk struct {
	DiskID    uuid.UUID
	MountPath string
}

func (s *Store) VMDisks(ctx context.Context, vm uuid.UUID) ([]VMDisk, error) {
	rows, err := s.q.Query(ctx, "SELECT disk_id, mount_path FROM vm_disks WHERE vm_id = $1 ORDER BY position", vm)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowToStructByPos[VMDisk])
}

// OrgVMDisks returns every VM's disks of the org by position.
func (s *Store) OrgVMDisks(ctx context.Context, org uuid.UUID) (map[uuid.UUID][]VMDisk, error) {
	rows, err := s.q.Query(ctx, `
		SELECT vd.vm_id, vd.disk_id, vd.mount_path FROM vm_disks vd JOIN vms v ON v.id = vd.vm_id
		WHERE v.org_id = $1 ORDER BY vd.vm_id, vd.position`, org)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[uuid.UUID][]VMDisk{}
	for rows.Next() {
		var vm uuid.UUID
		var d VMDisk
		if err := rows.Scan(&vm, &d.DiskID, &d.MountPath); err != nil {
			return nil, err
		}
		out[vm] = append(out[vm], d)
	}
	return out, rows.Err()
}

func diskExportAAD(disk uuid.UUID) []byte { return []byte("disk-export:" + disk.String()) }

// SetVMDisks replaces the VM's disk list; every newly attached disk gets a fresh export (port and key) on the VM's node.
// changed is false when the list was already disks.
func (s *Store) SetVMDisks(ctx context.Context, org, vm uuid.UUID, disks []VMDisk, restartAllowed bool) (out VM, changed bool, err error) {
	err = s.InTx(ctx, func(tx *Store) error {
		// Org first, like DeleteDisk and GrowDisk, so a disk can't be deleted or grown while it is being attached.
		if err := tx.LockOrg(ctx, org); err != nil {
			return err
		}
		v, err := scanVM(tx.q.QueryRow(ctx, "SELECT "+vmColumns+" FROM vms WHERE id = $1 AND org_id = $2 FOR UPDATE", vm, org))
		if err != nil {
			return err
		}
		out = v
		rows, err := tx.q.Query(ctx, "SELECT disk_id, mount_path FROM vm_disks WHERE vm_id = $1 ORDER BY position", vm)
		if err != nil {
			return err
		}
		current, err := pgx.CollectRows(rows, pgx.RowToStructByPos[VMDisk])
		if err != nil {
			return err
		}
		if slices.Equal(current, disks) {
			return nil
		}
		// Never nil: a NULL array would make "<> ALL" below match nothing.
		ids := []uuid.UUID{}
		distinct := map[uuid.UUID]bool{}
		for _, d := range disks {
			ids = append(ids, d.DiskID)
			distinct[d.DiskID] = true
		}
		rows, err = tx.q.Query(ctx, `
			SELECT d.id, d.user_id IS NOT DISTINCT FROM $3 AND d.team_id IS NOT DISTINCT FROM $4,
				vd.vm_id IS NOT NULL AND vd.vm_id <> $5, e.disk_id IS NOT NULL AND (e.releasing OR vd.vm_id IS NULL)
			FROM disks d LEFT JOIN vm_disks vd ON vd.disk_id = d.id LEFT JOIN disk_exports e ON e.disk_id = d.id
			WHERE d.org_id = $1 AND d.id = ANY($2) AND d.deleted_at IS NULL FOR UPDATE OF d`,
			org, ids, v.UserID, v.TeamID, vm)
		if err != nil {
			return err
		}
		attachedHere := make(map[uuid.UUID]bool, len(current))
		for _, c := range current {
			attachedHere[c.DiskID] = true
		}
		var added []uuid.UUID
		found := 0
		for rows.Next() {
			var id uuid.UUID
			var sameOwner, elsewhere, exported bool
			if err := rows.Scan(&id, &sameOwner, &elsewhere, &exported); err != nil {
				rows.Close()
				return err
			}
			found++
			switch {
			case !sameOwner:
				err = ErrOwnerMismatch
			case elsewhere || exported:
				err = ErrConflict
			case !attachedHere[id]:
				added = append(added, id)
			}
			if err != nil {
				rows.Close()
				return err
			}
		}
		if err := rows.Err(); err != nil {
			return err
		}
		if found != len(distinct) {
			return ErrNotFound
		}
		if v.Power == proto.PowerRunning && !restartAllowed {
			return ErrRestartRequired
		}
		if _, err := tx.q.Exec(ctx, `
			UPDATE disk_exports SET releasing = true, release_seq = NULL
			WHERE disk_id IN (SELECT disk_id FROM vm_disks WHERE vm_id = $1) AND disk_id <> ALL($2)`, vm, ids); err != nil {
			return err
		}
		if _, err := tx.q.Exec(ctx, "DELETE FROM vm_disks WHERE vm_id = $1", vm); err != nil {
			return err
		}
		for i, d := range disks {
			_, err := tx.q.Exec(ctx, "INSERT INTO vm_disks (vm_id, disk_id, mount_path, position) VALUES ($1, $2, $3, $4)", vm, d.DiskID, d.MountPath, i)
			if isUniqueViolation(err) {
				return ErrConflict
			} else if err != nil {
				return err
			}
		}
		if len(added) > 0 {
			if err := tx.lockAddresses(ctx); err != nil {
				return err
			}
		}
		for _, id := range added {
			if err := tx.createExport(ctx, id, v.NodeID); err != nil {
				return err
			}
		}
		out, err = scanVM(tx.q.QueryRow(ctx, "UPDATE vms SET generation = generation + 1 WHERE id = $1 RETURNING "+vmColumns, vm))
		changed = err == nil
		return err
	})
	return out, changed, err
}

// createExport needs the address lock so two attaches can't pick the same port.
func (s *Store) createExport(ctx context.Context, disk, node uuid.UUID) error {
	if s.master == nil {
		return ErrNoMasterKey
	}
	var port *int
	if err := s.q.QueryRow(ctx, `
		SELECT min(p) FROM generate_series($1::int, $2::int) p WHERE NOT EXISTS (SELECT 1 FROM disk_exports WHERE port = p)`,
		netaddr.StoragePortFirst, netaddr.StoragePortLast).Scan(&port); err != nil {
		return err
	}
	if port == nil {
		return ErrNoStoragePort
	}
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		return err
	}
	nonce, sealed, err := seal.Seal(*s.master, key, diskExportAAD(disk))
	if err != nil {
		return err
	}
	_, err = s.q.Exec(ctx, "INSERT INTO disk_exports (disk_id, node_id, port, key_id, nonce, sealed) VALUES ($1, $2, $3, $4, $5, $6)",
		disk, node, *port, s.master.ID(), nonce, sealed)
	return err
}

func (s *Store) exportKey(disk uuid.UUID, keyID string, nonce, sealed []byte) (string, error) {
	if s.master == nil {
		return "", ErrNoMasterKey
	}
	if keyID != s.master.ID() {
		return "", ErrMasterKeyMismatch
	}
	key, err := seal.Open(*s.master, nonce, sealed, diskExportAAD(disk))
	return hex.EncodeToString(key), err
}

// NodeAttachments returns the node's VMs' disks by position; URL and key only once the storage server has the file.
func (s *Store) NodeAttachments(ctx context.Context, node uuid.UUID) (map[uuid.UUID][]proto.DiskAttachment, error) {
	rows, err := s.q.Query(ctx, `
		SELECT vd.vm_id, d.id, vd.mount_path, d.ready_gb, e.port, e.key_id, e.nonce, e.sealed
		FROM vms v JOIN vm_disks vd ON vd.vm_id = v.id JOIN disks d ON d.id = vd.disk_id
		LEFT JOIN disk_exports e ON e.disk_id = d.id AND e.node_id = v.node_id AND NOT e.releasing
		WHERE v.node_id = $1 ORDER BY vd.vm_id, vd.position`, node)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[uuid.UUID][]proto.DiskAttachment{}
	for rows.Next() {
		var vm, disk uuid.UUID
		var a proto.DiskAttachment
		var port *int
		var keyID *string
		var nonce, sealed []byte
		if err := rows.Scan(&vm, &disk, &a.MountPath, &a.SizeGB, &port, &keyID, &nonce, &sealed); err != nil {
			return nil, err
		}
		a.DiskID = disk.String()
		if a.SizeGB > 0 && port != nil {
			a.URL = netaddr.StorageURL(*port, a.DiskID)
			if a.PSK, err = s.exportKey(disk, *keyID, nonce, sealed); err != nil {
				return nil, err
			}
		}
		out[vm] = append(out[vm], a)
	}
	return out, rows.Err()
}

func collectIDs(rows pgx.Rows, err error) ([]uuid.UUID, error) {
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowTo[uuid.UUID])
}

// ReleasingDisks lists the node's releasing exports that no desired state has announced yet.
func (s *Store) ReleasingDisks(ctx context.Context, node uuid.UUID) ([]uuid.UUID, error) {
	return collectIDs(s.q.Query(ctx, "SELECT disk_id FROM disk_exports WHERE node_id = $1 AND releasing AND release_seq IS NULL ORDER BY disk_id", node))
}

// MarkReleaseSeq records the first desired-state seq sent after the detach; later sends keep it.
func (s *Store) MarkReleaseSeq(ctx context.Context, node uuid.UUID, disks []uuid.UUID, seq uint64) error {
	_, err := s.q.Exec(ctx, `
		UPDATE disk_exports SET release_seq = $3
		WHERE node_id = $1 AND disk_id = ANY($2) AND releasing AND release_seq IS NULL`, node, disks, seq)
	return err
}

// MaxReleaseSeq returns the highest recorded release_seq, 0 if none.
func (s *Store) MaxReleaseSeq(ctx context.Context) (uint64, error) {
	var seq int64
	err := s.q.QueryRow(ctx, "SELECT COALESCE(MAX(release_seq), 0) FROM disk_exports").Scan(&seq)
	return uint64(seq), err
}

// ConfirmReleases deletes the node's releasing exports it has applied (appliedSeq >= release_seq) and no longer uses.
func (s *Store) ConfirmReleases(ctx context.Context, node uuid.UUID, inUse []uuid.UUID, appliedSeq uint64) ([]uuid.UUID, error) {
	if inUse == nil {
		inUse = []uuid.UUID{}
	}
	return collectIDs(s.q.Query(ctx, `
		DELETE FROM disk_exports WHERE node_id = $1 AND releasing AND release_seq <= $3 AND disk_id <> ALL($2)
		RETURNING disk_id`, node, inUse, appliedSeq))
}

// ForceRelease drops a releasing export without the node's confirmation; the old key and port die with it.
func (s *Store) ForceRelease(ctx context.Context, org, disk uuid.UUID) error {
	tag, err := s.q.Exec(ctx, `
		DELETE FROM disk_exports e USING disks d
		WHERE e.disk_id = d.id AND d.id = $2 AND d.org_id = $1 AND e.releasing`, org, disk)
	if err != nil || tag.RowsAffected() > 0 {
		return err
	}
	var exists bool
	if err := s.q.QueryRow(ctx, "SELECT EXISTS (SELECT 1 FROM disks WHERE id = $1 AND org_id = $2 AND deleted_at IS NULL)", disk, org).Scan(&exists); err != nil {
		return err
	}
	if !exists {
		return ErrNotFound
	}
	return ErrConflict
}

package store

import (
	"context"
	"errors"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/asterism-dev/observatory/internal/proto"
)

var ErrInvalidSize = errors.New("size must be larger than the current size")

type Disk struct {
	ID, OrgID      uuid.UUID
	UserID, TeamID *uuid.UUID
	Name           string
	SizeGB         int
	ReadyGB        int
	CopyFrom       *uuid.UUID
	Attachment     *DiskAttachment
	Releasing      bool
	CreatedBy      uuid.UUID
	CreatedAt      time.Time
}

type DiskAttachment struct {
	VMID              uuid.UUID
	VMName, MountPath string
}

const diskSelect = `SELECT d.id, d.org_id, d.user_id, d.team_id, d.name, d.size_gb, d.ready_gb, d.copy_from,
	vd.vm_id, v.name, vd.mount_path, coalesce(e.releasing, false),
	coalesce(d.created_by, '00000000-0000-0000-0000-000000000000'::uuid), d.created_at
	FROM disks d LEFT JOIN vm_disks vd ON vd.disk_id = d.id LEFT JOIN vms v ON v.id = vd.vm_id
	LEFT JOIN disk_exports e ON e.disk_id = d.id
	WHERE d.deleted_at IS NULL AND d.org_id = $1`

func scanDisk(row pgx.Row) (d Disk, err error) {
	var vmID *uuid.UUID
	var vmName, mountPath *string
	err = row.Scan(&d.ID, &d.OrgID, &d.UserID, &d.TeamID, &d.Name, &d.SizeGB, &d.ReadyGB, &d.CopyFrom,
		&vmID, &vmName, &mountPath, &d.Releasing, &d.CreatedBy, &d.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		err = ErrNotFound
	}
	if vmID != nil {
		d.Attachment = &DiskAttachment{VMID: *vmID, VMName: *vmName, MountPath: *mountPath}
	}
	return
}

func (s *Store) Disks(ctx context.Context, org uuid.UUID) ([]Disk, error) {
	rows, err := s.q.Query(ctx, diskSelect+" ORDER BY d.name", org)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (Disk, error) { return scanDisk(r) })
}

func (s *Store) Disk(ctx context.Context, org, id uuid.UUID) (Disk, error) {
	return scanDisk(s.q.QueryRow(ctx, diskSelect+" AND d.id = $2", org, id))
}

// CreateDisk takes the size of the live source disk when d.CopyFrom is set.
func (s *Store) CreateDisk(ctx context.Context, d Disk) (Disk, error) {
	var out Disk
	err := s.InTx(ctx, func(tx *Store) error {
		if err := tx.LockOrg(ctx, d.OrgID); err != nil {
			return err
		}
		if d.CopyFrom != nil {
			err := tx.q.QueryRow(ctx, "SELECT size_gb FROM disks WHERE id = $1 AND org_id = $2 AND deleted_at IS NULL",
				*d.CopyFrom, d.OrgID).Scan(&d.SizeGB)
			if errors.Is(err, pgx.ErrNoRows) {
				return ErrNotFound
			} else if err != nil {
				return err
			}
		}
		if err := tx.checkLimits(ctx, d.OrgID, d.UserID, d.TeamID, Limits{DiskGB: &d.SizeGB}); err != nil {
			return err
		}
		var id uuid.UUID
		err := tx.q.QueryRow(ctx, `
			INSERT INTO disks (org_id, user_id, team_id, name, size_gb, copy_from, created_by)
			VALUES ($1, $2, $3, $4, $5, $6, $7) RETURNING id`,
			d.OrgID, d.UserID, d.TeamID, d.Name, d.SizeGB, d.CopyFrom, d.CreatedBy).Scan(&id)
		if isUniqueViolation(err) {
			return ErrConflict
		} else if err != nil {
			return err
		}
		out, err = tx.Disk(ctx, d.OrgID, id)
		return err
	})
	return out, err
}

func (s *Store) GrowDisk(ctx context.Context, org, id uuid.UUID, sizeGB int, restartAllowed bool) (Disk, error) {
	var out Disk
	err := s.InTx(ctx, func(tx *Store) error {
		if err := tx.LockOrg(ctx, org); err != nil {
			return err
		}
		d, err := tx.Disk(ctx, org, id)
		if err != nil {
			return err
		}
		if sizeGB <= d.SizeGB {
			return ErrInvalidSize
		}
		if d.Releasing {
			return ErrConflict
		}
		if d.Attachment != nil && !restartAllowed {
			var power proto.Power
			if err := tx.q.QueryRow(ctx, "SELECT power FROM vms WHERE id = $1", d.Attachment.VMID).Scan(&power); err != nil {
				return err
			}
			if power != proto.PowerStopped {
				return ErrRestartRequired
			}
		}
		delta := sizeGB - d.SizeGB
		if err := tx.checkLimits(ctx, org, d.UserID, d.TeamID, Limits{DiskGB: &delta}); err != nil {
			return err
		}
		if _, err := tx.q.Exec(ctx, "UPDATE disks SET size_gb = $2, grow_restart = grow_restart OR $3 WHERE id = $1", id, sizeGB, restartAllowed); err != nil {
			return err
		}
		out, err = tx.Disk(ctx, org, id)
		return err
	})
	return out, err
}

// DeleteDisk soft-deletes; the row is purged once the storage server reports the file gone.
func (s *Store) DeleteDisk(ctx context.Context, org, id uuid.UUID) error {
	return s.InTx(ctx, func(tx *Store) error {
		if err := tx.LockOrg(ctx, org); err != nil {
			return err
		}
		var busy bool
		err := tx.q.QueryRow(ctx, `
			SELECT EXISTS (SELECT 1 FROM vm_disks WHERE disk_id = d.id)
				OR EXISTS (SELECT 1 FROM disk_exports WHERE disk_id = d.id)
				OR EXISTS (SELECT 1 FROM disks c WHERE c.copy_from = d.id AND c.deleted_at IS NULL)
				OR EXISTS (SELECT 1 FROM snapshot_disks sd WHERE sd.source_disk_id = d.id AND sd.ready_gb = 0
					AND sd.deleted_at IS NULL AND sd.snapshot_id IS NOT NULL)
			FROM disks d WHERE d.id = $1 AND d.org_id = $2 AND d.deleted_at IS NULL`, id, org).Scan(&busy)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		} else if err != nil {
			return err
		}
		if busy {
			return ErrConflict
		}
		// A pending restore of a deleted disk is abandoned, so its VM need not wait for it.
		if _, err = tx.q.Exec(ctx, "UPDATE disks SET deleted_at = now(), restore_from = NULL, restore_token = NULL WHERE id = $1", id); err != nil {
			return err
		}
		_, err = tx.finishRestores(ctx, []uuid.UUID{org})
		return err
	})
}

// StorageState is the storage server's desired state; it carries unsealed export keys, so callers must never log it.
func (s *Store) StorageState(ctx context.Context) (proto.StorageState, error) {
	pool, ok := s.q.(*pgxpool.Pool)
	if !ok {
		return s.readStorageState(ctx, s.q)
	}
	// One snapshot, so an export never refers to a disk state the same response contradicts.
	tx, err := pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return proto.StorageState{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	return s.readStorageState(ctx, tx)
}

func (s *Store) readStorageState(ctx context.Context, q querier) (proto.StorageState, error) {
	st := proto.StorageState{Disks: []proto.StorageDisk{}, Deleted: []string{}, Exports: []proto.StorageExport{}}
	// Snapshot copies are files like disks; one still read by a live disk's copy or restore stays until that is done.
	rows, err := q.Query(ctx, `
		SELECT id, size_gb, ready_gb, coalesce(copy_from, copy_from_snapshot), restore_from, restore_token, deleted_at IS NOT NULL
		FROM disks
		UNION ALL
		SELECT sd.id, sd.size_gb, sd.ready_gb, CASE WHEN sd.ready_gb = 0 THEN sd.source_disk_id END, NULL, NULL,
			(sd.deleted_at IS NOT NULL OR sd.snapshot_id IS NULL) AND NOT EXISTS (SELECT 1 FROM disks d
				WHERE d.deleted_at IS NULL AND (d.copy_from_snapshot = sd.id OR d.restore_from = sd.id))
		FROM snapshot_disks sd
		ORDER BY 1`)
	if err != nil {
		return st, err
	}
	for rows.Next() {
		var id uuid.UUID
		var size, ready int
		var copyFrom, restoreFrom *uuid.UUID
		var restoreToken *string
		var deleted bool
		if err := rows.Scan(&id, &size, &ready, &copyFrom, &restoreFrom, &restoreToken, &deleted); err != nil {
			rows.Close()
			return st, err
		}
		if deleted {
			st.Deleted = append(st.Deleted, id.String())
			continue
		}
		d := proto.StorageDisk{ID: id.String(), SizeBytes: int64(size) << 30, Create: ready == 0}
		if copyFrom != nil {
			d.CopyFrom = copyFrom.String()
		}
		if restoreFrom != nil && restoreToken != nil {
			d.RestoreFrom, d.RestoreToken = proto.StorageFileName(restoreFrom.String()), *restoreToken
		}
		st.Disks = append(st.Disks, d)
	}
	if err := rows.Err(); err != nil {
		return st, err
	}
	rows, err = q.Query(ctx, `
		SELECT e.disk_id, e.port, e.key_id, e.nonce, e.sealed FROM disk_exports e JOIN disks d ON d.id = e.disk_id
		WHERE d.ready_gb > 0 ORDER BY e.port`)
	if err != nil {
		return st, err
	}
	defer rows.Close()
	for rows.Next() {
		var disk uuid.UUID
		var x proto.StorageExport
		var keyID string
		var nonce, sealed []byte
		if err := rows.Scan(&disk, &x.Port, &keyID, &nonce, &sealed); err != nil {
			return st, err
		}
		x.DiskID = disk.String()
		if x.PSK, err = s.exportKey(disk, keyID, nonce, sealed); err != nil {
			return st, err
		}
		st.Exports = append(st.Exports, x)
	}
	return st, rows.Err()
}

// ApplyStorageFiles records what the storage server has on disk and returns the orgs whose VMs got a new disk size.
func (s *Store) ApplyStorageFiles(ctx context.Context, files []proto.StorageFile) ([]uuid.UUID, error) {
	ids, sizes := []uuid.UUID{}, []int64{}
	for _, f := range files {
		name, ok := strings.CutSuffix(f.Name, ".img")
		if id, err := uuid.Parse(name); ok && err == nil && proto.StorageFileName(id.String()) == f.Name {
			ids, sizes = append(ids, id), append(sizes, f.SizeBytes)
		}
	}
	var orgs []uuid.UUID
	err := s.InTx(ctx, func(tx *Store) error {
		const grown = `FROM disks d JOIN unnest($1::uuid[], $2::bigint[]) f(id, size) ON f.id = d.id
			WHERE d.deleted_at IS NULL AND d.ready_gb <> d.size_gb AND f.size >= d.size_gb::bigint << 30`
		const copied = `FROM snapshot_disks sd JOIN unnest($1::uuid[], $2::bigint[]) f(id, size) ON f.id = sd.id
			JOIN vm_snapshots s ON s.id = sd.snapshot_id
			WHERE sd.deleted_at IS NULL AND sd.ready_gb = 0 AND f.size >= sd.size_gb::bigint << 30`
		candidates, err := collectIDs(tx.q.Query(ctx, "SELECT d.org_id "+grown+" UNION SELECT s.org_id "+copied+" ORDER BY 1", ids, sizes))
		if err != nil {
			return err
		}
		// Org locks first, in order, like GrowDisk and SetVMDisks, so the disk and VM updates below can't deadlock with them.
		for _, org := range candidates {
			if err := tx.LockOrg(ctx, org); err != nil {
				return err
			}
		}
		// A running VM restarts only for a first-time ready disk (it cannot have booted without it) or a grow with restart consent.
		if orgs, err = collectIDs(tx.q.Query(ctx, `
			WITH ready AS (
				UPDATE disks n SET ready_gb = n.size_gb, grow_restart = false FROM disks o
				WHERE o.id = n.id AND n.id IN (SELECT d.id `+grown+` AND d.org_id = ANY($3))
				RETURNING n.id, o.ready_gb = 0 OR o.grow_restart AS restart)
			UPDATE vms v SET generation = generation + 1 FROM vm_disks vd JOIN ready r ON r.id = vd.disk_id
			WHERE vd.vm_id = v.id AND (r.restart OR v.power = 'stopped') RETURNING v.org_id`, ids, sizes, candidates)); err != nil {
			return err
		}
		ready, err := collectIDs(tx.q.Query(ctx, `
			WITH copies AS (
				UPDATE snapshot_disks SET ready_gb = size_gb WHERE id IN (SELECT sd.id `+copied+` AND s.org_id = ANY($3))
				RETURNING id, snapshot_id)
			-- The statement still sees the copies as not ready, hence the exclusion.
			-- A copy finishing after the sweep timeout (e.g. right after a hub restart) may postdate the VM's resume.
			UPDATE vm_snapshots s SET state = 'ready' WHERE s.state = 'creating' AND s.id IN (SELECT snapshot_id FROM copies)
				AND s.reported_at > now() - make_interval(secs => $4)
				AND NOT EXISTS (SELECT 1 FROM snapshot_disks sd WHERE sd.snapshot_id = s.id AND sd.deleted_at IS NULL
					AND sd.ready_gb = 0 AND sd.id NOT IN (SELECT id FROM copies))
			RETURNING s.org_id`, ids, sizes, candidates, SnapshotCopyTimeout.Seconds()))
		if err != nil {
			return err
		}
		orgs = append(orgs, ready...)
		if _, err := tx.q.Exec(ctx, `
			UPDATE disks SET copy_from = NULL, copy_from_snapshot = NULL
			WHERE id = ANY($1) AND (copy_from IS NOT NULL OR copy_from_snapshot IS NOT NULL)`, ids); err != nil {
			return err
		}
		if _, err = tx.q.Exec(ctx, "DELETE FROM disks WHERE deleted_at IS NOT NULL AND id <> ALL($1)", ids); err != nil {
			return err
		}
		_, err = tx.q.Exec(ctx, `
			DELETE FROM snapshot_disks sd WHERE (sd.deleted_at IS NOT NULL OR sd.snapshot_id IS NULL) AND sd.id <> ALL($1)
				AND NOT EXISTS (SELECT 1 FROM disks d WHERE d.copy_from_snapshot = sd.id OR d.restore_from = sd.id)`, ids)
		return err
	})
	if err != nil {
		return nil, err
	}
	slices.SortFunc(orgs, func(a, b uuid.UUID) int { return strings.Compare(a.String(), b.String()) })
	return slices.Compact(orgs), nil
}

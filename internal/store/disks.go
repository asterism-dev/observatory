package store

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

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
		zero := 0
		if err := tx.checkLimits(ctx, d.OrgID, d.UserID, d.TeamID, Limits{VCPU: &zero, RAMMB: &zero, DiskGB: &d.SizeGB, VMCount: &zero}); err != nil {
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
			if power == proto.PowerRunning {
				return ErrRestartRequired
			}
		}
		zero, delta := 0, sizeGB-d.SizeGB
		if err := tx.checkLimits(ctx, org, d.UserID, d.TeamID, Limits{VCPU: &zero, RAMMB: &zero, DiskGB: &delta, VMCount: &zero}); err != nil {
			return err
		}
		if _, err := tx.q.Exec(ctx, "UPDATE disks SET size_gb = $2 WHERE id = $1", id, sizeGB); err != nil {
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
			FROM disks d WHERE d.id = $1 AND d.org_id = $2 AND d.deleted_at IS NULL`, id, org).Scan(&busy)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		} else if err != nil {
			return err
		}
		if busy {
			return ErrConflict
		}
		_, err = tx.q.Exec(ctx, "UPDATE disks SET deleted_at = now() WHERE id = $1", id)
		return err
	})
}

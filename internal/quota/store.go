package quota

import (
	"context"
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	eventsv1 "github.com/asterism-dev/observatory/gen/go/observatory/events/v1"
	"github.com/asterism-dev/observatory/internal/authz"
	"github.com/asterism-dev/observatory/internal/platform/outbox"
)

//go:embed migrations/*.sql
var migrationFS embed.FS

func migrations() (fs.FS, error) {
	own, err := fs.Sub(migrationFS, "migrations")
	if err != nil {
		return nil, err
	}
	return outbox.Migrations(own, 2), nil
}

var (
	errLimitExceedsOrg = errors.New("limit exceeds org limit")
	errNotFound        = errors.New("not found")
)

const scopeOrg = "org"

// Limits holds nil for unlimited.
type Limits struct{ VCPU, RAMMB, DiskGB, VMCount, SnapshotCount, ImageStorageGB *int32 }

func (l Limits) fields() [6]*int32 {
	return [6]*int32{l.VCPU, l.RAMMB, l.DiskGB, l.VMCount, l.SnapshotCount, l.ImageStorageGB}
}

// exceeds reports whether any non-null value of l is above the matching non-null value of org.
func (l Limits) exceeds(org Limits) bool {
	o := org.fields()
	for i, v := range l.fields() {
		if v != nil && o[i] != nil && *v > *o[i] {
			return true
		}
	}
	return false
}

type store struct {
	pool *pgxpool.Pool
}

// limits returns the stored rows by scope; scopes without a row are absent (all unlimited).
func (s *store) limits(ctx context.Context, org uuid.UUID) (map[string]Limits, error) {
	rows, err := s.pool.Query(ctx, "SELECT scope, vcpu, ram_mb, disk_gb, vm_count, snapshot_count, image_storage_gb FROM limits WHERE org = $1", org)
	if err != nil {
		return nil, fmt.Errorf("limits: %w", err)
	}
	defer rows.Close()
	out := map[string]Limits{}
	for rows.Next() {
		var scope string
		var l Limits
		if err := rows.Scan(&scope, &l.VCPU, &l.RAMMB, &l.DiskGB, &l.VMCount, &l.SnapshotCount, &l.ImageStorageGB); err != nil {
			return nil, fmt.Errorf("limits: %w", err)
		}
		out[scope] = l
	}
	return out, rows.Err()
}

func lockOrg(ctx context.Context, tx pgx.Tx, org uuid.UUID) error {
	if _, err := tx.Exec(ctx, "INSERT INTO orgs (org) VALUES ($1) ON CONFLICT DO NOTHING", org); err != nil {
		return fmt.Errorf("lock org: %w", err)
	}
	if _, err := tx.Exec(ctx, "SELECT 1 FROM orgs WHERE org = $1 FOR UPDATE", org); err != nil {
		return fmt.Errorf("lock org: %w", err)
	}
	return nil
}

// SetLimits replaces scope's limits under the org lock, so team/user ≤ org holds under concurrent changes; known reports whether a team or user scope names a team or member of the org.
// audit, if set, is written to the outbox in the same transaction.
func (s *store) SetLimits(ctx context.Context, org uuid.UUID, scope string, l Limits, known func(string) bool, audit *eventsv1.Event) error {
	if scope != scopeOrg {
		if err := checkScopeInOrg(scope, known); err != nil {
			return err
		}
	}
	return pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		if err := lockOrg(ctx, tx, org); err != nil {
			return err
		}
		if scope == scopeOrg {
			var below bool
			err := tx.QueryRow(ctx, `
				SELECT EXISTS (SELECT 1 FROM limits WHERE org = $1 AND scope <> 'org'
					AND (vcpu > $2::int OR ram_mb > $3::int OR disk_gb > $4::int OR vm_count > $5::int OR snapshot_count > $6::int OR image_storage_gb > $7::int))`,
				org, l.VCPU, l.RAMMB, l.DiskGB, l.VMCount, l.SnapshotCount, l.ImageStorageGB).Scan(&below)
			if err != nil {
				return fmt.Errorf("set limits: %w", err)
			}
			if below {
				return errLimitExceedsOrg
			}
		} else {
			var o Limits
			err := tx.QueryRow(ctx, "SELECT vcpu, ram_mb, disk_gb, vm_count, snapshot_count, image_storage_gb FROM limits WHERE org = $1 AND scope = 'org'", org).
				Scan(&o.VCPU, &o.RAMMB, &o.DiskGB, &o.VMCount, &o.SnapshotCount, &o.ImageStorageGB)
			if err != nil && !errors.Is(err, pgx.ErrNoRows) {
				return fmt.Errorf("set limits: %w", err)
			}
			if l.exceeds(o) {
				return errLimitExceedsOrg
			}
		}
		_, err := tx.Exec(ctx, `
			INSERT INTO limits (org, scope, vcpu, ram_mb, disk_gb, vm_count, snapshot_count, image_storage_gb) VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
			ON CONFLICT (org, scope) DO UPDATE SET vcpu = EXCLUDED.vcpu, ram_mb = EXCLUDED.ram_mb,
				disk_gb = EXCLUDED.disk_gb, vm_count = EXCLUDED.vm_count, snapshot_count = EXCLUDED.snapshot_count,
				image_storage_gb = EXCLUDED.image_storage_gb`,
			org, scope, l.VCPU, l.RAMMB, l.DiskGB, l.VMCount, l.SnapshotCount, l.ImageStorageGB)
		if err != nil {
			return fmt.Errorf("set limits: %w", err)
		}
		if audit == nil {
			return nil
		}
		return outbox.Write(ctx, tx, audit)
	})
}

// checkScopeInOrg wants a canonical team:<uuid> or user:<uuid> that known accepts; a nil known accepts nothing.
func checkScopeInOrg(scope string, known func(string) bool) error {
	kind, raw, _ := strings.Cut(scope, ":")
	id, err := uuid.Parse(raw)
	if err != nil || id.String() != raw || known == nil {
		return errNotFound
	}
	if (kind == "team" && scope == authz.TeamSubject(id) || kind == "user" && scope == authz.UserSubject(id)) && known(scope) {
		return nil
	}
	return errNotFound
}

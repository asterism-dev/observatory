package store

import (
	"context"
	"errors"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/asterism-dev/observatory/internal/authz"
)

var ErrLimitExceedsOrg = errors.New("limit exceeds org limit")

const ScopeOrg = "org"

// Limits holds nil for unlimited.
type Limits struct{ VCPU, RAMMB, DiskGB, VMCount *int }

func (l Limits) fields() [4]*int { return [4]*int{l.VCPU, l.RAMMB, l.DiskGB, l.VMCount} }

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

// Limits returns the stored rows by scope; scopes without a row are absent (all unlimited).
func (s *Store) Limits(ctx context.Context, orgID uuid.UUID) (map[string]Limits, error) {
	rows, err := s.q.Query(ctx, "SELECT scope, vcpu, ram_mb, disk_gb, vm_count FROM limits WHERE org_id = $1", orgID)
	if err != nil {
		return nil, err
	}
	out := map[string]Limits{}
	for rows.Next() {
		var scope string
		var l Limits
		if err := rows.Scan(&scope, &l.VCPU, &l.RAMMB, &l.DiskGB, &l.VMCount); err != nil {
			return nil, err
		}
		out[scope] = l
	}
	return out, rows.Err()
}

// SetLimits takes the org lock so team/user ≤ org holds under concurrent org, team and member changes.
func (s *Store) SetLimits(ctx context.Context, orgID uuid.UUID, scope string, l Limits) error {
	return s.InTx(ctx, func(tx *Store) error {
		if err := tx.LockOrg(ctx, orgID); err != nil {
			return err
		}
		if scope == ScopeOrg {
			var below bool
			err := tx.q.QueryRow(ctx, `
				SELECT EXISTS (SELECT 1 FROM limits WHERE org_id = $1 AND scope <> 'org'
					AND (vcpu > $2::int OR ram_mb > $3::int OR disk_gb > $4::int OR vm_count > $5::int))`,
				orgID, l.VCPU, l.RAMMB, l.DiskGB, l.VMCount).Scan(&below)
			if err != nil {
				return err
			}
			if below {
				return ErrLimitExceedsOrg
			}
		} else {
			if err := tx.checkScopeInOrg(ctx, orgID, scope); err != nil {
				return err
			}
			var org Limits
			err := tx.q.QueryRow(ctx, "SELECT vcpu, ram_mb, disk_gb, vm_count FROM limits WHERE org_id = $1 AND scope = 'org'", orgID).
				Scan(&org.VCPU, &org.RAMMB, &org.DiskGB, &org.VMCount)
			if err != nil && !errors.Is(err, pgx.ErrNoRows) {
				return err
			}
			if l.exceeds(org) {
				return ErrLimitExceedsOrg
			}
		}
		_, err := tx.q.Exec(ctx, `
			INSERT INTO limits (org_id, scope, vcpu, ram_mb, disk_gb, vm_count) VALUES ($1, $2, $3, $4, $5, $6)
			ON CONFLICT (org_id, scope) DO UPDATE SET vcpu = EXCLUDED.vcpu, ram_mb = EXCLUDED.ram_mb,
				disk_gb = EXCLUDED.disk_gb, vm_count = EXCLUDED.vm_count`,
			orgID, scope, l.VCPU, l.RAMMB, l.DiskGB, l.VMCount)
		return err
	})
}

// checkScopeInOrg wants a canonical team:<uuid> or user:<uuid> naming a team or member of the org.
func (s *Store) checkScopeInOrg(ctx context.Context, orgID uuid.UUID, scope string) error {
	kind, raw, _ := strings.Cut(scope, ":")
	id, err := uuid.Parse(raw)
	if err != nil || id.String() != raw {
		return ErrNotFound
	}
	var q string
	switch {
	case kind == "team" && scope == authz.TeamSubject(id):
		q = "SELECT 1 FROM teams WHERE org_id = $1 AND id = $2"
	case kind == "user" && scope == authz.UserSubject(id):
		q = "SELECT 1 FROM memberships WHERE org_id = $1 AND user_id = $2 FOR SHARE"
	default:
		return ErrNotFound
	}
	var one int
	if err := s.q.QueryRow(ctx, q, orgID, id).Scan(&one); errors.Is(err, pgx.ErrNoRows) {
		return ErrNotFound
	} else if err != nil {
		return err
	}
	return nil
}

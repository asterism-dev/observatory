package quota

import (
	"context"
	"fmt"
	"math"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	quotav1 "github.com/asterism-dev/observatory/gen/go/observatory/quota/v1"
	"github.com/asterism-dev/observatory/internal/platform/quotaclient"
)

// ponytail: a hold counts only for its TTL, so a consumer lagging more than 5 min behind a commit under-counts usage for the gap; holds must expire for crashed callers.
const reservationTTL = 5 * time.Minute

// Amounts are what an operation adds; nil dimensions are not checked.
type Amounts struct{ VCPU, RAMMB, DiskGB, VMCount, SnapshotCount *int32 }

func (a Amounts) fields() [5]*int32 {
	return [5]*int32{a.VCPU, a.RAMMB, a.DiskGB, a.VMCount, a.SnapshotCount}
}

// dimensions name Amounts' fields in check order, as 0.x's limitNames.
var dimensions = [5][2]string{{"vCPU", ""}, {"RAM", " MB"}, {"disk", " GB"}, {"VM count", ""}, {"snapshot count", ""}}

// limitError mirrors 0.x store.LimitError: Scope is "org" or the owner.
type limitError struct {
	Scope      string
	Name, Unit string
	Limit, Got int
}

func (e *limitError) Error() string { return quotaclient.Message(e.detail()) }

func (e *limitError) detail() *quotav1.LimitExceeded {
	return &quotav1.LimitExceeded{Scope: e.Scope, Name: e.Name, Unit: e.Unit, Limit: int32(e.Limit), Got: int32(e.Got)}
}

// exceeded returns the first requested dimension in which used + add is above max, else nil.
func exceeded(scope string, used [5]int64, add Amounts, max Limits) *limitError {
	m := max.fields()
	for i, a := range add.fields() {
		if a == nil || m[i] == nil {
			continue
		}
		if got := used[i] + int64(*a); got > int64(*m[i]) {
			return &limitError{Scope: scope, Name: dimensions[i][0], Unit: dimensions[i][1], Limit: int(*m[i]), Got: int(min(got, math.MaxInt32))}
		}
	}
	return nil
}

// Reserve holds add for owner until it expires, if it fits the org limits and then owner's; the org lock serializes it with other reserves and limit changes.
func (s *store) Reserve(ctx context.Context, org uuid.UUID, owner string, add Amounts) (uuid.UUID, time.Time, error) {
	id, now := uuid.New(), s.now()
	expires := now.Add(reservationTTL)
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		if err := lockOrg(ctx, tx, org); err != nil {
			return err
		}
		rows, err := tx.Query(ctx, "SELECT scope, vcpu, ram_mb, disk_gb, vm_count, snapshot_count FROM limits WHERE org = $1 AND scope = ANY($2)", org, []string{scopeOrg, owner})
		if err != nil {
			return fmt.Errorf("reserve: %w", err)
		}
		ls := map[string]Limits{}
		for rows.Next() {
			var scope string
			var l Limits
			if err := rows.Scan(&scope, &l.VCPU, &l.RAMMB, &l.DiskGB, &l.VMCount, &l.SnapshotCount); err != nil {
				rows.Close()
				return fmt.Errorf("reserve: %w", err)
			}
			ls[scope] = l
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return fmt.Errorf("reserve: %w", err)
		}
		for _, c := range []struct{ scope, owner string }{{scopeOrg, ""}, {owner, owner}} {
			l, ok := ls[c.scope]
			if !ok {
				continue
			}
			used, err := usage(ctx, tx, org, c.owner, now)
			if err != nil {
				return err
			}
			if e := exceeded(c.scope, used, add, l); e != nil {
				return e
			}
		}
		_, err = tx.Exec(ctx, "INSERT INTO reservations (id, org, owner, vcpu, ram_mb, disk_gb, vm_count, snapshot_count, expires_at) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)",
			id, org, owner, val(add.VCPU), val(add.RAMMB), val(add.DiskGB), val(add.VMCount), val(add.SnapshotCount), expires)
		if err != nil {
			return fmt.Errorf("reserve: %w", err)
		}
		return nil
	})
	if err != nil {
		return uuid.Nil, time.Time{}, err
	}
	return id, expires, nil
}

func val(p *int32) int32 {
	if p == nil {
		return 0
	}
	return *p
}

// usage sums org's live allocations and unexpired holds, only owner's when owner is set.
func usage(ctx context.Context, tx pgx.Tx, org uuid.UUID, owner string, now time.Time) (u [5]int64, err error) {
	err = tx.QueryRow(ctx, `
		SELECT coalesce(sum(vcpu), 0), coalesce(sum(ram_mb), 0), coalesce(sum(disk_gb), 0), coalesce(sum(vm_count), 0), coalesce(sum(snapshot_count), 0) FROM (
			SELECT vcpu, ram_mb, disk_gb, vm_count, snapshot_count FROM allocations WHERE org = $1 AND NOT deleted AND ($2 = '' OR owner = $2)
			UNION ALL
			SELECT vcpu, ram_mb, disk_gb, vm_count, snapshot_count FROM reservations WHERE org = $1 AND expires_at > $3 AND ($2 = '' OR owner = $2)
		) u`, org, owner, now).Scan(&u[0], &u[1], &u[2], &u[3], &u[4])
	if err != nil {
		return u, fmt.Errorf("usage: %w", err)
	}
	return u, nil
}

// Cancel drops holds; unknown ids are ignored.
func (s *store) Cancel(ctx context.Context, ids []uuid.UUID) error {
	if _, err := s.pool.Exec(ctx, "DELETE FROM reservations WHERE id = ANY($1)", ids); err != nil {
		return fmt.Errorf("cancel: %w", err)
	}
	return nil
}

// Release tombstones org's resources and their children, unseen ones too, so no later event can revive them.
func (s *store) Release(ctx context.Context, org uuid.UUID, resources []string) error {
	if len(resources) == 0 {
		return nil
	}
	now := s.now()
	return pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `
			INSERT INTO allocations (org, resource, owner, last_event_id, deleted, updated_at) SELECT $1, r, '', $3, true, $4 FROM unnest($2::text[]) r
			ON CONFLICT (org, resource) DO UPDATE SET deleted = true, updated_at = EXCLUDED.updated_at`, org, resources, uuid.Nil, now)
		if err != nil {
			return fmt.Errorf("release: %w", err)
		}
		if _, err := tx.Exec(ctx, "UPDATE allocations SET deleted = true, updated_at = $3 WHERE org = $1 AND parent = ANY($2)", org, resources, now); err != nil {
			return fmt.Errorf("release: %w", err)
		}
		return nil
	})
}

// setAllocation sets a resource's absolute usage from a lifecycle event: a tombstone is final, otherwise only a newer event id applies.
func (s *store) setAllocation(ctx context.Context, org uuid.UUID, resource, owner, parent string, a Amounts, eventID uuid.UUID) error {
	var p *string
	if parent != "" {
		p = &parent
	}
	_, err := s.pool.Exec(ctx, `
		INSERT INTO allocations (org, resource, owner, parent, vcpu, ram_mb, disk_gb, vm_count, snapshot_count, last_event_id, updated_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)
		ON CONFLICT (org, resource) DO UPDATE SET owner = EXCLUDED.owner, parent = EXCLUDED.parent, vcpu = EXCLUDED.vcpu, ram_mb = EXCLUDED.ram_mb,
			disk_gb = EXCLUDED.disk_gb, vm_count = EXCLUDED.vm_count, snapshot_count = EXCLUDED.snapshot_count,
			last_event_id = EXCLUDED.last_event_id, updated_at = EXCLUDED.updated_at
			WHERE NOT allocations.deleted AND allocations.last_event_id < EXCLUDED.last_event_id`,
		org, resource, owner, p, val(a.VCPU), val(a.RAMMB), val(a.DiskGB), val(a.VMCount), val(a.SnapshotCount), eventID, s.now())
	if err != nil {
		return fmt.Errorf("set allocation: %w", err)
	}
	return nil
}

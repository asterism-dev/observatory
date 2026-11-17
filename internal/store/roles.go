package store

import (
	"context"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/asterism-dev/observatory/internal/authz"
)

// PolicySnapshot reads the whole policy; inside a transaction it sees that transaction's uncommitted rows.
func (s *Store) PolicySnapshot(ctx context.Context) (authz.Policy, error) {
	pool, ok := s.q.(*pgxpool.Pool)
	if !ok {
		return readPolicy(ctx, s.q)
	}
	tx, err := pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return authz.Policy{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	return readPolicy(ctx, tx)
}

func readPolicy(ctx context.Context, q querier) (authz.Policy, error) {
	var p authz.Policy
	rows, err := q.Query(ctx, `SELECT org_id, user_id, role FROM memberships`)
	if err != nil {
		return p, err
	}
	p.Memberships, err = pgx.CollectRows(rows, func(r pgx.CollectableRow) (m authz.MemberRole, err error) {
		err = r.Scan(&m.OrgID, &m.UserID, &m.Role)
		return
	})
	if err != nil {
		return p, err
	}

	rows, err = q.Query(ctx, `SELECT t.org_id, tm.team_id, tm.user_id, tm.admin FROM team_members tm JOIN teams t ON t.id = tm.team_id`)
	if err != nil {
		return p, err
	}
	p.TeamMembers, err = pgx.CollectRows(rows, func(r pgx.CollectableRow) (m authz.TeamMember, err error) {
		err = r.Scan(&m.OrgID, &m.TeamID, &m.UserID, &m.Admin)
		return
	})
	if err != nil {
		return p, err
	}

	rows, err = q.Query(ctx, `SELECT id, org_id, name, description FROM roles`)
	if err != nil {
		return p, err
	}
	p.Roles, err = pgx.CollectRows(rows, func(r pgx.CollectableRow) (c authz.CustomRole, err error) {
		err = r.Scan(&c.ID, &c.OrgID, &c.Name, &c.Description)
		return
	})
	if err != nil {
		return p, err
	}
	byID := make(map[uuid.UUID]*authz.CustomRole, len(p.Roles))
	for i := range p.Roles {
		byID[p.Roles[i].ID] = &p.Roles[i]
	}

	rows, err = q.Query(ctx, `SELECT role_id, effect, object, action FROM role_rules ORDER BY role_id, position`)
	if err != nil {
		return p, err
	}
	defer rows.Close()
	for rows.Next() {
		var id uuid.UUID
		var r authz.Rule
		if err := rows.Scan(&id, &r.Effect, &r.Object, &r.Action); err != nil {
			return p, err
		}
		if c := byID[id]; c != nil {
			c.Rules = append(c.Rules, r)
		}
	}
	if err := rows.Err(); err != nil {
		return p, err
	}

	rows, err = q.Query(ctx, `SELECT role_id, coalesce(parent_builtin, parent_role::text) FROM role_parents ORDER BY role_id, 2`)
	if err != nil {
		return p, err
	}
	defer rows.Close()
	for rows.Next() {
		var id uuid.UUID
		var parent string
		if err := rows.Scan(&id, &parent); err != nil {
			return p, err
		}
		if c := byID[id]; c != nil {
			c.Parents = append(c.Parents, parent)
		}
	}
	if err := rows.Err(); err != nil {
		return p, err
	}

	rows, err = q.Query(ctx, `SELECT org_id, subject, role_id FROM role_bindings`)
	if err != nil {
		return p, err
	}
	p.Bindings, err = pgx.CollectRows(rows, func(r pgx.CollectableRow) (b authz.Binding, err error) {
		err = r.Scan(&b.OrgID, &b.Subject, &b.RoleID)
		return
	})
	return p, err
}

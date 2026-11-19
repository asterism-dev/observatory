package store

import (
	"context"
	"errors"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
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

	rows, err = q.Query(ctx, `SELECT org_id, id FROM teams`)
	if err != nil {
		return p, err
	}
	p.Teams, err = pgx.CollectRows(rows, func(r pgx.CollectableRow) (t authz.TeamRef, err error) {
		err = r.Scan(&t.OrgID, &t.ID)
		return
	})
	if err != nil {
		return p, err
	}

	rows, err = q.Query(ctx, `SELECT org_id, id, user_id, team_id FROM vms`)
	if err != nil {
		return p, err
	}
	p.VMs, err = pgx.CollectRows(rows, func(r pgx.CollectableRow) (v authz.VMRef, err error) {
		err = r.Scan(&v.OrgID, &v.ID, &v.UserID, &v.TeamID)
		return
	})
	if err != nil {
		return p, err
	}

	if p.Roles, err = readRoles(ctx, q, nil); err != nil {
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

// readRoles reads the custom roles of org, or of all orgs if org is nil, ordered by name.
func readRoles(ctx context.Context, q querier, org *uuid.UUID) ([]authz.CustomRole, error) {
	rows, err := q.Query(ctx, `SELECT id, org_id, name, description FROM roles WHERE $1::uuid IS NULL OR org_id = $1 ORDER BY name, id`, org)
	if err != nil {
		return nil, err
	}
	roles, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (c authz.CustomRole, err error) {
		err = r.Scan(&c.ID, &c.OrgID, &c.Name, &c.Description)
		return
	})
	if err != nil {
		return nil, err
	}
	byID := make(map[uuid.UUID]*authz.CustomRole, len(roles))
	for i := range roles {
		byID[roles[i].ID] = &roles[i]
	}

	rows, err = q.Query(ctx, `SELECT role_id, effect, object, action FROM role_rules WHERE role_id IN (SELECT id FROM roles WHERE $1::uuid IS NULL OR org_id = $1) ORDER BY role_id, position`, org)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var id uuid.UUID
		var r authz.Rule
		if err := rows.Scan(&id, &r.Effect, &r.Object, &r.Action); err != nil {
			return nil, err
		}
		if c := byID[id]; c != nil {
			c.Rules = append(c.Rules, r)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	rows, err = q.Query(ctx, `SELECT role_id, coalesce(parent_builtin, parent_role::text) FROM role_parents WHERE role_id IN (SELECT id FROM roles WHERE $1::uuid IS NULL OR org_id = $1) ORDER BY role_id, 2`, org)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var id uuid.UUID
		var parent string
		if err := rows.Scan(&id, &parent); err != nil {
			return nil, err
		}
		if c := byID[id]; c != nil {
			c.Parents = append(c.Parents, parent)
		}
	}
	return roles, rows.Err()
}

var ErrInvalidParent = errors.New("invalid parent role")

type BindingRow struct {
	ID        uuid.UUID
	Subject   string
	RoleID    uuid.UUID
	CreatedAt time.Time
}

func (s *Store) Roles(ctx context.Context, orgID uuid.UUID) ([]authz.CustomRole, error) {
	return readRoles(ctx, s.q, &orgID)
}

func (s *Store) Role(ctx context.Context, orgID, roleID uuid.UUID) (authz.CustomRole, error) {
	roles, err := s.Roles(ctx, orgID)
	if err != nil {
		return authz.CustomRole{}, err
	}
	i := slices.IndexFunc(roles, func(r authz.CustomRole) bool { return r.ID == roleID })
	if i < 0 {
		return authz.CustomRole{}, ErrNotFound
	}
	return roles[i], nil
}

// LockOrg serializes role graph and team membership changes in an org, so concurrent writes can't combine into a cycle or an over-deep chain; take it first (org → memberships → team_members).
func (s *Store) LockOrg(ctx context.Context, orgID uuid.UUID) error {
	var one int
	err := s.q.QueryRow(ctx, "SELECT 1 FROM orgs WHERE id = $1 FOR UPDATE", orgID).Scan(&one)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrNotFound
	}
	return err
}

func (s *Store) CreateRole(ctx context.Context, orgID uuid.UUID, r authz.CustomRole) (authz.CustomRole, error) {
	r.OrgID = orgID
	err := s.InTx(ctx, func(tx *Store) error {
		if err := tx.LockOrg(ctx, orgID); err != nil {
			return err
		}
		err := tx.q.QueryRow(ctx, "INSERT INTO roles (org_id, name, description) VALUES ($1, $2, $3) RETURNING id", orgID, r.Name, r.Description).Scan(&r.ID)
		if isUniqueViolation(err) {
			return ErrConflict
		} else if err != nil {
			return err
		}
		return tx.writeRoleBody(ctx, r)
	})
	if err != nil {
		return authz.CustomRole{}, err
	}
	return r, nil
}

// UpdateRole replaces name, description, rules and parents of r.ID.
func (s *Store) UpdateRole(ctx context.Context, orgID uuid.UUID, r authz.CustomRole) error {
	r.OrgID = orgID
	return s.InTx(ctx, func(tx *Store) error {
		if err := tx.LockOrg(ctx, orgID); err != nil {
			return err
		}
		tag, err := tx.q.Exec(ctx, "UPDATE roles SET name = $3, description = $4 WHERE org_id = $1 AND id = $2", orgID, r.ID, r.Name, r.Description)
		switch {
		case isUniqueViolation(err):
			return ErrConflict
		case err != nil:
			return err
		case tag.RowsAffected() == 0:
			return ErrNotFound
		}
		if _, err := tx.q.Exec(ctx, "DELETE FROM role_rules WHERE role_id = $1", r.ID); err != nil {
			return err
		}
		if _, err := tx.q.Exec(ctx, "DELETE FROM role_parents WHERE role_id = $1", r.ID); err != nil {
			return err
		}
		return tx.writeRoleBody(ctx, r)
	})
}

// writeRoleBody inserts r's rules and parents; a parent that is r or descends from r is a cycle (ErrConflict).
func (s *Store) writeRoleBody(ctx context.Context, r authz.CustomRole) error {
	for i, rule := range r.Rules {
		if _, err := s.q.Exec(ctx, "INSERT INTO role_rules (role_id, position, effect, object, action) VALUES ($1, $2, $3, $4, $5)",
			r.ID, i, rule.Effect, rule.Object, rule.Action); err != nil {
			return err
		}
	}
	var custom []uuid.UUID
	for _, parent := range r.Parents {
		var err error
		if parent == "member" || parent == "admin" {
			_, err = s.q.Exec(ctx, "INSERT INTO role_parents (role_id, parent_builtin) VALUES ($1, $2)", r.ID, parent)
		} else {
			id, perr := uuid.Parse(parent)
			if perr != nil || id.String() != parent {
				return ErrInvalidParent
			}
			var tag pgconn.CommandTag
			tag, err = s.q.Exec(ctx, `INSERT INTO role_parents (role_id, parent_role)
				SELECT $1, id FROM roles WHERE org_id = $2 AND id = $3`, r.ID, r.OrgID, id)
			if err == nil && tag.RowsAffected() == 0 {
				return ErrInvalidParent
			}
			custom = append(custom, id)
		}
		if isUniqueViolation(err) {
			return ErrConflict
		} else if err != nil {
			return err
		}
	}
	var cycle bool
	err := s.q.QueryRow(ctx, `
		WITH RECURSIVE ancestors(id) AS (
			SELECT unnest($1::uuid[])
			UNION SELECT rp.parent_role FROM role_parents rp JOIN ancestors a ON rp.role_id = a.id WHERE rp.parent_role IS NOT NULL)
		SELECT EXISTS (SELECT 1 FROM ancestors WHERE id = $2)`, custom, r.ID).Scan(&cycle)
	if err != nil {
		return err
	}
	if cycle {
		return ErrConflict
	}
	return nil
}

// DeleteRole also drops the role's bindings and its use as a parent (cascade).
func (s *Store) DeleteRole(ctx context.Context, orgID, roleID uuid.UUID) error {
	tag, err := s.q.Exec(ctx, "DELETE FROM roles WHERE org_id = $1 AND id = $2", orgID, roleID)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// Bind binds a custom role of the org to subject "user:<id>" (an org member) or "team:<id>" (an org team), else ErrNotMember.
func (s *Store) Bind(ctx context.Context, orgID uuid.UUID, subject string, roleID uuid.UUID) (BindingRow, error) {
	b := BindingRow{Subject: subject, RoleID: roleID}
	err := s.InTx(ctx, func(tx *Store) error {
		if err := tx.LockOrg(ctx, orgID); err != nil {
			return err
		}
		var one int
		err := tx.q.QueryRow(ctx, "SELECT 1 FROM roles WHERE org_id = $1 AND id = $2 FOR SHARE", orgID, roleID).Scan(&one)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		} else if err != nil {
			return err
		}
		if err := tx.LockSubject(ctx, orgID, subject); err != nil {
			return err
		}
		err = tx.q.QueryRow(ctx, "INSERT INTO role_bindings (org_id, subject, role_id) VALUES ($1, $2, $3) RETURNING id, created_at",
			orgID, subject, roleID).Scan(&b.ID, &b.CreatedAt)
		if isUniqueViolation(err) {
			return ErrConflict
		}
		return err
	})
	if err != nil {
		return BindingRow{}, err
	}
	return b, nil
}

// LockSubject share-locks the membership or team behind subject (role_bindings.subject has no FK), so it can't vanish before commit.
func (s *Store) LockSubject(ctx context.Context, orgID uuid.UUID, subject string) error {
	query := "SELECT 1 FROM memberships WHERE org_id = $1 AND user_id = $2 FOR SHARE"
	raw, ok := strings.CutPrefix(subject, "user:")
	if !ok {
		query = "SELECT 1 FROM teams WHERE org_id = $1 AND id = $2 FOR SHARE"
		raw, ok = strings.CutPrefix(subject, "team:")
	}
	id, err := uuid.Parse(raw)
	if !ok || err != nil || id.String() != raw {
		return ErrNotMember
	}
	var one int
	err = s.q.QueryRow(ctx, query, orgID, id).Scan(&one)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrNotMember
	}
	return err
}

// Unbind deletes the binding and returns it.
func (s *Store) Unbind(ctx context.Context, orgID, bindingID uuid.UUID) (BindingRow, error) {
	b := BindingRow{ID: bindingID}
	err := s.q.QueryRow(ctx, "DELETE FROM role_bindings WHERE org_id = $1 AND id = $2 RETURNING subject, role_id, created_at",
		orgID, bindingID).Scan(&b.Subject, &b.RoleID, &b.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return BindingRow{}, ErrNotFound
	}
	return b, err
}

func (s *Store) Bindings(ctx context.Context, orgID uuid.UUID) ([]BindingRow, error) {
	rows, err := s.q.Query(ctx, "SELECT id, subject, role_id, created_at FROM role_bindings WHERE org_id = $1 ORDER BY created_at, id", orgID)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (b BindingRow, err error) {
		err = r.Scan(&b.ID, &b.Subject, &b.RoleID, &b.CreatedAt)
		return
	})
}

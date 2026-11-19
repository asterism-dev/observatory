package store

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/asterism-dev/observatory/internal/authz"
)

var (
	ErrPersonalOrg = errors.New("not possible for a personal org")
	ErrLastOwner   = errors.New("an org needs at least one owner")
	ErrNotMember   = errors.New("not a member of the org")
	ErrConflict    = errors.New("already exists")
)

func (s *Store) CreateOrg(ctx context.Context, name string, ownerID uuid.UUID) (Org, error) {
	org := Org{Name: name}
	err := s.InTx(ctx, func(tx *Store) error {
		if err := tx.q.QueryRow(ctx, "INSERT INTO orgs (name) VALUES ($1) RETURNING id, created_at", name).Scan(&org.ID, &org.CreatedAt); err != nil {
			return err
		}
		_, err := tx.q.Exec(ctx, "INSERT INTO memberships (org_id, user_id, role) VALUES ($1, $2, $3)", org.ID, ownerID, RoleOwner)
		return err
	})
	if err != nil {
		return Org{}, err
	}
	return org, nil
}

func (s *Store) RenameOrg(ctx context.Context, orgID uuid.UUID, name string) error {
	var personal bool
	err := s.q.QueryRow(ctx, "SELECT personal_for IS NOT NULL FROM orgs WHERE id = $1", orgID).Scan(&personal)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return ErrNotFound
	case err != nil:
		return err
	case personal:
		return ErrPersonalOrg
	}
	_, err = s.q.Exec(ctx, "UPDATE orgs SET name = $2 WHERE id = $1", orgID, name)
	return err
}

func (s *Store) Membership(ctx context.Context, orgID, userID uuid.UUID) (Membership, error) {
	var m Membership
	err := s.q.QueryRow(ctx, `
		SELECT o.id, o.name, o.personal_for IS NOT NULL, o.created_at, m.role
		FROM memberships m JOIN orgs o ON o.id = m.org_id
		WHERE m.org_id = $1 AND m.user_id = $2`, orgID, userID).
		Scan(&m.Org.ID, &m.Org.Name, &m.Org.Personal, &m.Org.CreatedAt, &m.Role)
	if errors.Is(err, pgx.ErrNoRows) {
		return Membership{}, ErrNotFound
	}
	return m, err
}

type Member struct {
	User     User
	Role     Role
	JoinedAt time.Time
}

func (s *Store) Members(ctx context.Context, orgID uuid.UUID) ([]Member, error) {
	rows, err := s.q.Query(ctx, `
		SELECT u.id, u.subject, u.email, u.name, u.platform_admin, u.created_at, m.role, m.created_at
		FROM memberships m JOIN users u ON u.id = m.user_id
		WHERE m.org_id = $1
		ORDER BY m.created_at, u.id`, orgID)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (m Member, err error) {
		err = r.Scan(&m.User.ID, &m.User.Subject, &m.User.Email, &m.User.Name, &m.User.PlatformAdmin, &m.User.CreatedAt, &m.Role, &m.JoinedAt)
		return
	})
}

// LockMember locks the org row, serializing membership changes in the org, and returns the user's current role there.
func (s *Store) LockMember(ctx context.Context, orgID, userID uuid.UUID) (Role, error) {
	var personal bool
	err := s.q.QueryRow(ctx, "SELECT personal_for IS NOT NULL FROM orgs WHERE id = $1 FOR UPDATE", orgID).Scan(&personal)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return "", ErrNotFound
	case err != nil:
		return "", err
	case personal:
		return "", ErrPersonalOrg
	}
	var role Role
	err = s.q.QueryRow(ctx, "SELECT role FROM memberships WHERE org_id = $1 AND user_id = $2 FOR UPDATE", orgID, userID).Scan(&role)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", ErrNotFound
	}
	return role, err
}

// checkNotLastOwner must run after lockMember so the count can't change before commit.
func (s *Store) checkNotLastOwner(ctx context.Context, orgID uuid.UUID, current Role) error {
	if current != RoleOwner {
		return nil
	}
	var owners int
	if err := s.q.QueryRow(ctx, "SELECT count(*) FROM memberships WHERE org_id = $1 AND role = $2", orgID, RoleOwner).Scan(&owners); err != nil {
		return err
	}
	if owners <= 1 {
		return ErrLastOwner
	}
	return nil
}

func (s *Store) SetMemberRole(ctx context.Context, orgID, userID uuid.UUID, role Role) error {
	return s.InTx(ctx, func(tx *Store) error {
		current, err := tx.LockMember(ctx, orgID, userID)
		if err != nil {
			return err
		}
		if role != RoleOwner {
			if err := tx.checkNotLastOwner(ctx, orgID, current); err != nil {
				return err
			}
		}
		_, err = tx.q.Exec(ctx, "UPDATE memberships SET role = $3 WHERE org_id = $1 AND user_id = $2", orgID, userID, role)
		return err
	})
}

// RemoveMember also drops the user's team memberships, role bindings, limits, private variables and private integrations in the org; devices go by cascade.
func (s *Store) RemoveMember(ctx context.Context, orgID, userID uuid.UUID) error {
	return s.InTx(ctx, func(tx *Store) error {
		current, err := tx.LockMember(ctx, orgID, userID)
		if err != nil {
			return err
		}
		if err := tx.checkNotLastOwner(ctx, orgID, current); err != nil {
			return err
		}
		var owns bool
		if err := tx.q.QueryRow(ctx, `
			SELECT EXISTS (SELECT 1 FROM vms WHERE org_id = $1 AND user_id = $2)
				OR EXISTS (SELECT 1 FROM disks WHERE org_id = $1 AND user_id = $2 AND deleted_at IS NULL)`, orgID, userID).Scan(&owns); err != nil {
			return err
		}
		if owns {
			return ErrOwnsResources
		}
		if _, err := tx.q.Exec(ctx, `
			DELETE FROM team_members WHERE user_id = $2 AND team_id IN (SELECT id FROM teams WHERE org_id = $1)`, orgID, userID); err != nil {
			return err
		}
		if _, err := tx.q.Exec(ctx, "DELETE FROM role_bindings WHERE org_id = $1 AND subject = $2", orgID, authz.UserSubject(userID)); err != nil {
			return err
		}
		if _, err := tx.q.Exec(ctx, "DELETE FROM limits WHERE org_id = $1 AND scope = $2", orgID, authz.UserSubject(userID)); err != nil {
			return err
		}
		if _, err := tx.q.Exec(ctx, "DELETE FROM env_vars WHERE org_id = $1 AND scope = 'user' AND user_id = $2", orgID, userID); err != nil {
			return err
		}
		if _, err := tx.q.Exec(ctx, "DELETE FROM integrations WHERE org_id = $1 AND scope = 'user' AND user_id = $2", orgID, userID); err != nil {
			return err
		}
		if _, err := tx.q.Exec(ctx, "DELETE FROM images WHERE org_id = $1 AND scope = 'user' AND user_id = $2", orgID, userID); err != nil {
			return err
		}
		if _, err := tx.q.Exec(ctx, "DELETE FROM image_defaults WHERE org_id = $1 AND scope = 'user' AND user_id = $2", orgID, userID); err != nil {
			return err
		}
		_, err = tx.q.Exec(ctx, "DELETE FROM memberships WHERE org_id = $1 AND user_id = $2", orgID, userID)
		return err
	})
}

var ErrLastPlatformAdmin = errors.New("a platform needs at least one admin")

type OrgSummary struct {
	Org         Org
	MemberCount int
}

func (s *Store) Orgs(ctx context.Context) ([]OrgSummary, error) {
	rows, err := s.q.Query(ctx, `
		SELECT o.id, o.name, o.personal_for IS NOT NULL, o.created_at, (SELECT count(*) FROM memberships m WHERE m.org_id = o.id)
		FROM orgs o ORDER BY o.name, o.id`)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (o OrgSummary, err error) {
		err = r.Scan(&o.Org.ID, &o.Org.Name, &o.Org.Personal, &o.Org.CreatedAt, &o.MemberCount)
		return
	})
}

// Org returns the org without regard to membership; callers must be platform admins.
func (s *Store) Org(ctx context.Context, orgID uuid.UUID) (Org, error) {
	var o Org
	err := s.q.QueryRow(ctx, "SELECT id, name, personal_for IS NOT NULL, created_at FROM orgs WHERE id = $1", orgID).
		Scan(&o.ID, &o.Name, &o.Personal, &o.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return Org{}, ErrNotFound
	}
	return o, err
}

func (s *Store) Users(ctx context.Context) ([]User, error) {
	rows, err := s.q.Query(ctx, "SELECT "+userColumns+" FROM users ORDER BY name, id")
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (User, error) { return scanUser(r) })
}

// SetPlatformAdmin locks every current admin row, so concurrent revocations can't leave none.
func (s *Store) SetPlatformAdmin(ctx context.Context, userID uuid.UUID, admin bool) error {
	return s.InTx(ctx, func(tx *Store) error {
		rows, err := tx.q.Query(ctx, "SELECT id FROM users WHERE platform_admin ORDER BY id FOR UPDATE")
		if err != nil {
			return err
		}
		admins, err := pgx.CollectRows(rows, pgx.RowTo[uuid.UUID])
		if err != nil {
			return err
		}
		if !admin && len(admins) == 1 && admins[0] == userID {
			return ErrLastPlatformAdmin
		}
		tag, err := tx.q.Exec(ctx, "UPDATE users SET platform_admin = $2 WHERE id = $1", userID, admin)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			return ErrNotFound
		}
		return nil
	})
}

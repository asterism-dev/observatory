package store

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

type Role string

const (
	RoleOwner  Role = "owner"
	RoleAdmin  Role = "admin"
	RoleMember Role = "member"
)

type Identity struct{ Subject, Email, Name string }

type User struct {
	ID            uuid.UUID
	Subject       string
	Email         string
	Name          string
	PlatformAdmin bool
	CreatedAt     time.Time
}

type Org struct {
	ID        uuid.UUID
	Name      string
	Personal  bool
	CreatedAt time.Time
}

type Membership struct {
	Org  Org
	Role Role
}

type LoginResult struct {
	User                User
	Created             bool
	PersonalOrg         *Org
	BecamePlatformAdmin bool
}

const firstUserLockKey = 0x6f627376_01 // arbitrary constant, unique per lock purpose

const userColumns = "id, subject, email, name, platform_admin, created_at"

func scanUser(row pgx.Row) (User, error) {
	var u User
	err := row.Scan(&u.ID, &u.Subject, &u.Email, &u.Name, &u.PlatformAdmin, &u.CreatedAt)
	return u, err
}

func (s *Store) Login(ctx context.Context, id Identity) (LoginResult, error) {
	var res LoginResult
	err := s.InTx(ctx, func(tx *Store) error {
		if _, err := tx.q.Exec(ctx, "SELECT pg_advisory_xact_lock($1)", firstUserLockKey); err != nil {
			return err
		}
		var empty bool
		if err := tx.q.QueryRow(ctx, "SELECT NOT EXISTS (SELECT 1 FROM users)").Scan(&empty); err != nil {
			return err
		}
		// xmax = 0 only for a freshly inserted row, not for the ON CONFLICT update path.
		row := tx.q.QueryRow(ctx, `
			INSERT INTO users (subject, email, name, platform_admin) VALUES ($1, $2, $3, $4)
			ON CONFLICT (subject) DO UPDATE SET email = EXCLUDED.email, name = EXCLUDED.name
			RETURNING `+userColumns+`, (xmax = 0)`, id.Subject, id.Email, id.Name, empty)
		var err error
		if err = row.Scan(&res.User.ID, &res.User.Subject, &res.User.Email, &res.User.Name,
			&res.User.PlatformAdmin, &res.User.CreatedAt, &res.Created); err != nil {
			return err
		}
		if !res.Created {
			return nil
		}
		res.BecamePlatformAdmin = empty
		orgName := id.Name
		if orgName == "" {
			orgName = id.Email
		}
		org := Org{Name: orgName, Personal: true}
		if err := tx.q.QueryRow(ctx,
			"INSERT INTO orgs (name, personal_for) VALUES ($1, $2) RETURNING id, created_at",
			orgName, res.User.ID).Scan(&org.ID, &org.CreatedAt); err != nil {
			return err
		}
		if _, err := tx.q.Exec(ctx,
			"INSERT INTO memberships (org_id, user_id, role) VALUES ($1, $2, $3)",
			org.ID, res.User.ID, RoleOwner); err != nil {
			return err
		}
		res.PersonalOrg = &org
		return nil
	})
	if err != nil {
		return LoginResult{}, err
	}
	return res, nil
}

func (s *Store) User(ctx context.Context, id uuid.UUID) (User, error) {
	u, err := scanUser(s.q.QueryRow(ctx, "SELECT "+userColumns+" FROM users WHERE id = $1", id))
	if errors.Is(err, pgx.ErrNoRows) {
		return User{}, ErrNotFound
	}
	return u, err
}

func (s *Store) Memberships(ctx context.Context, userID uuid.UUID) ([]Membership, error) {
	rows, err := s.q.Query(ctx, `
		SELECT o.id, o.name, o.personal_for IS NOT NULL, o.created_at, m.role
		FROM memberships m JOIN orgs o ON o.id = m.org_id
		WHERE m.user_id = $1
		ORDER BY (o.personal_for IS NULL), o.name`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Membership
	for rows.Next() {
		var m Membership
		if err := rows.Scan(&m.Org.ID, &m.Org.Name, &m.Org.Personal, &m.Org.CreatedAt, &m.Role); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

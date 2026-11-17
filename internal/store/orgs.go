package store

import (
	"context"
	"errors"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
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

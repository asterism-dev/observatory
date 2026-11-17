package store

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

var (
	ErrInviteInvalid = errors.New("invalid invite")
	ErrAlreadyMember = errors.New("already a member")
)

type Invite struct {
	ID, OrgID, CreatedBy uuid.UUID
	Role                 Role
	CreatedAt, ExpiresAt time.Time
}

const inviteColumns = "id, org_id, created_by, role, created_at, expires_at"

func scanInvite(row pgx.Row) (Invite, error) {
	var i Invite
	err := row.Scan(&i.ID, &i.OrgID, &i.CreatedBy, &i.Role, &i.CreatedAt, &i.ExpiresAt)
	return i, err
}

func (s *Store) CreateInvite(ctx context.Context, orgID uuid.UUID, role Role, by uuid.UUID, ttl time.Duration) (string, Invite, error) {
	var personal bool
	err := s.q.QueryRow(ctx, "SELECT personal_for IS NOT NULL FROM orgs WHERE id = $1", orgID).Scan(&personal)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return "", Invite{}, ErrNotFound
	case err != nil:
		return "", Invite{}, err
	case personal:
		return "", Invite{}, ErrPersonalOrg
	}
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", Invite{}, err
	}
	token := "obsinv_" + base64.RawURLEncoding.EncodeToString(raw)
	inv, err := scanInvite(s.q.QueryRow(ctx, `
		INSERT INTO invites (org_id, token_hash, role, created_by, expires_at)
		VALUES ($1, $2, $3, $4, now() + make_interval(secs => $5))
		RETURNING `+inviteColumns, orgID, hashToken(token), role, by, ttl.Seconds()))
	if err != nil {
		return "", Invite{}, err
	}
	return token, inv, nil
}

// Invites lists the org's invites that can still be accepted.
func (s *Store) Invites(ctx context.Context, orgID uuid.UUID) ([]Invite, error) {
	rows, err := s.q.Query(ctx, `
		SELECT `+inviteColumns+` FROM invites
		WHERE org_id = $1 AND used_at IS NULL AND revoked_at IS NULL AND expires_at > now()
		ORDER BY created_at DESC, id`, orgID)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (Invite, error) { return scanInvite(r) })
}

func (s *Store) RevokeInvite(ctx context.Context, orgID, id uuid.UUID) error {
	tag, err := s.q.Exec(ctx, `
		UPDATE invites SET revoked_at = now()
		WHERE id = $1 AND org_id = $2 AND used_at IS NULL AND revoked_at IS NULL AND expires_at > now()`, id, orgID)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// AcceptInvite consumes the invite and adds the user; on ErrAlreadyMember the invite stays usable.
func (s *Store) AcceptInvite(ctx context.Context, token string, userID uuid.UUID) (Org, Role, error) {
	var (
		org  Org
		role Role
	)
	err := s.InTx(ctx, func(tx *Store) error {
		err := tx.q.QueryRow(ctx, `
			UPDATE invites SET used_at = now(), used_by = $2
			WHERE token_hash = $1 AND used_at IS NULL AND revoked_at IS NULL AND expires_at > now()
			RETURNING org_id, role`, hashToken(token), userID).Scan(&org.ID, &role)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrInviteInvalid
		}
		if err != nil {
			return err
		}
		tag, err := tx.q.Exec(ctx, `
			INSERT INTO memberships (org_id, user_id, role) VALUES ($1, $2, $3)
			ON CONFLICT (org_id, user_id) DO NOTHING`, org.ID, userID, role)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			return ErrAlreadyMember
		}
		return tx.q.QueryRow(ctx, "SELECT name, personal_for IS NOT NULL, created_at FROM orgs WHERE id = $1", org.ID).
			Scan(&org.Name, &org.Personal, &org.CreatedAt)
	})
	if err != nil {
		return Org{}, "", err
	}
	return org, role, nil
}

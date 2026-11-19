package store

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// CreateRegistryCredential issues a registry secret bound to the session, so revoking the session revokes it too.
func (s *Store) CreateRegistryCredential(ctx context.Context, session uuid.UUID, ttl time.Duration) (string, time.Time, error) {
	secret, err := randomSecret()
	if err != nil {
		return "", time.Time{}, err
	}
	secret = "obr_" + secret
	if _, err := s.q.Exec(ctx, "DELETE FROM registry_credentials WHERE expires_at < now()"); err != nil {
		return "", time.Time{}, err
	}
	var expires time.Time
	err = s.q.QueryRow(ctx, `
		INSERT INTO registry_credentials (session_id, secret_hash, expires_at) VALUES ($1, $2, now() + make_interval(secs => $3))
		RETURNING expires_at`, session, hashToken(secret), ttl.Seconds()).Scan(&expires)
	return secret, expires, err
}

// RegistryCredentialUser returns the user of a valid credential whose session is still valid.
func (s *Store) RegistryCredentialUser(ctx context.Context, secret string) (uuid.UUID, error) {
	var user uuid.UUID
	err := s.q.QueryRow(ctx, `
		SELECT s.user_id FROM registry_credentials c JOIN sessions s ON s.id = c.session_id
		WHERE c.secret_hash = $1 AND c.expires_at > now() AND s.revoked_at IS NULL AND s.expires_at > now()`, hashToken(secret)).Scan(&user)
	if errors.Is(err, pgx.ErrNoRows) {
		return uuid.Nil, ErrNotFound
	}
	return user, err
}

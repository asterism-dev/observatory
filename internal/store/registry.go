package store

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// ErrSessionRevoked is returned for a session whose end the hub has recorded.
var ErrSessionRevoked = errors.New("session revoked")

// revokedSessionRetention outlives every session (the CLI's 30 days), so a revoked one can never mint again.
const revokedSessionRetention = 30 * 24 * time.Hour

// CreateRegistryCredential issues a registry secret that ends with the session; DeleteSessionCredentials revokes it earlier.
func (s *Store) CreateRegistryCredential(ctx context.Context, user, session uuid.UUID, sessionExpires time.Time, ttl time.Duration) (string, time.Time, error) {
	secret, err := randomSecret()
	if err != nil {
		return "", time.Time{}, err
	}
	secret = "obr_" + secret
	if _, err := s.q.Exec(ctx, "DELETE FROM registry_credentials WHERE expires_at < now() OR session_expires_at < now()"); err != nil {
		return "", time.Time{}, err
	}
	if _, err := s.q.Exec(ctx, "DELETE FROM registry_revoked_sessions WHERE expires_at < now()"); err != nil {
		return "", time.Time{}, err
	}
	var expires time.Time
	err = s.q.QueryRow(ctx, `
		INSERT INTO registry_credentials (user_id, session_id, session_expires_at, secret_hash, expires_at)
		SELECT $1, $2, $3, $4, now() + make_interval(secs => $5)
		WHERE NOT EXISTS (SELECT 1 FROM registry_revoked_sessions WHERE session_id = $2)
		RETURNING expires_at`, user, session, sessionExpires, hashToken(secret), ttl.Seconds()).Scan(&expires)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", time.Time{}, ErrSessionRevoked
	}
	return secret, expires, err
}

// RegistryCredentialUser returns the user of a credential that hasn't expired and whose session hasn't either.
func (s *Store) RegistryCredentialUser(ctx context.Context, secret string) (uuid.UUID, error) {
	var user uuid.UUID
	err := s.q.QueryRow(ctx, `
		SELECT user_id FROM registry_credentials c
		WHERE secret_hash = $1 AND expires_at > now() AND session_expires_at > now()
			AND NOT EXISTS (SELECT 1 FROM registry_revoked_sessions r WHERE r.session_id = c.session_id)`, hashToken(secret)).Scan(&user)
	if errors.Is(err, pgx.ErrNoRows) {
		return uuid.Nil, ErrNotFound
	}
	return user, err
}

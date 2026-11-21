package store

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// CreateRegistryCredential issues a registry secret that ends with the session; every use checks the session with iam.
func (s *Store) CreateRegistryCredential(ctx context.Context, user, session uuid.UUID, sessionExpires time.Time, ttl time.Duration) (string, time.Time, error) {
	secret, err := randomSecret()
	if err != nil {
		return "", time.Time{}, err
	}
	secret = "obr_" + secret
	if _, err := s.q.Exec(ctx, "DELETE FROM registry_credentials WHERE expires_at < now() OR session_expires_at < now()"); err != nil {
		return "", time.Time{}, err
	}
	var expires time.Time
	err = s.q.QueryRow(ctx, `
		INSERT INTO registry_credentials (user_id, session_id, session_expires_at, secret_hash, expires_at)
		VALUES ($1, $2, $3, $4, now() + make_interval(secs => $5))
		RETURNING expires_at`, user, session, sessionExpires, hashToken(secret), ttl.Seconds()).Scan(&expires)
	return secret, expires, err
}

// RegistryCredentialUser returns the user and session of a credential that hasn't expired and whose session hasn't either; the caller asks iam whether the session was revoked.
func (s *Store) RegistryCredentialUser(ctx context.Context, secret string) (user, session uuid.UUID, err error) {
	err = s.q.QueryRow(ctx, `
		SELECT user_id, session_id FROM registry_credentials
		WHERE secret_hash = $1 AND expires_at > now() AND session_expires_at > now()`, hashToken(secret)).Scan(&user, &session)
	if errors.Is(err, pgx.ErrNoRows) {
		return uuid.Nil, uuid.Nil, ErrNotFound
	}
	return user, session, err
}

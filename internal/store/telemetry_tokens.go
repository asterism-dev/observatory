package store

import (
	"context"
	"crypto/rand"
	"encoding/base32"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

const (
	telemetryTokenPrefix  = "obs_grafana_"
	maxTelemetryTokens    = 20
	telemetryTouchMinimum = 60 * time.Second
)

type TelemetryToken struct {
	ID, OrgID, CreatedBy  uuid.UUID
	Name                  string
	CreatedAt             time.Time
	LastUsedAt, RevokedAt *time.Time
}

const telemetryTokenColumns = "id, org_id, name, created_by, created_at, last_used_at, revoked_at"

func scanTelemetryToken(row pgx.Row) (t TelemetryToken, err error) {
	err = row.Scan(&t.ID, &t.OrgID, &t.Name, &t.CreatedBy, &t.CreatedAt, &t.LastUsedAt, &t.RevokedAt)
	if err == pgx.ErrNoRows {
		err = ErrNotFound
	}
	return
}

// CreateTelemetryToken returns the plaintext secret, which is never stored.
func (s *Store) CreateTelemetryToken(ctx context.Context, org, createdBy uuid.UUID, name string) (TelemetryToken, string, error) {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return TelemetryToken{}, "", err
	}
	secret := telemetryTokenPrefix + base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(raw)
	var tok TelemetryToken
	err := s.InTx(ctx, func(tx *Store) error {
		if err := tx.LockHubOrg(ctx, org); err != nil {
			return err
		}
		var n int
		if err := tx.q.QueryRow(ctx, "SELECT count(*) FROM telemetry_tokens WHERE org_id = $1 AND revoked_at IS NULL", org).Scan(&n); err != nil {
			return err
		}
		if n >= maxTelemetryTokens {
			return ErrLimitExceeded
		}
		var err error
		tok, err = scanTelemetryToken(tx.q.QueryRow(ctx, `
			INSERT INTO telemetry_tokens (org_id, name, token_hash, created_by) VALUES ($1, $2, $3, $4)
			RETURNING `+telemetryTokenColumns, org, name, hashToken(secret), createdBy))
		if isUniqueViolation(err) {
			return ErrConflict
		}
		return err
	})
	if err != nil {
		return TelemetryToken{}, "", err
	}
	return tok, secret, nil
}

func (s *Store) TelemetryTokens(ctx context.Context, org uuid.UUID) ([]TelemetryToken, error) {
	rows, err := s.q.Query(ctx, "SELECT "+telemetryTokenColumns+" FROM telemetry_tokens WHERE org_id = $1 AND revoked_at IS NULL ORDER BY name, id", org)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []TelemetryToken
	for rows.Next() {
		t, err := scanTelemetryToken(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// RevokeTelemetryToken returns the revoked token's name.
func (s *Store) RevokeTelemetryToken(ctx context.Context, org, id uuid.UUID) (string, error) {
	var name string
	err := s.q.QueryRow(ctx, "UPDATE telemetry_tokens SET revoked_at = now() WHERE org_id = $1 AND id = $2 AND revoked_at IS NULL RETURNING name", org, id).Scan(&name)
	if err == pgx.ErrNoRows {
		err = ErrNotFound
	}
	return name, err
}

// RevokeTelemetryTokensBy revokes the active tokens user created in org and returns them.
func (s *Store) RevokeTelemetryTokensBy(ctx context.Context, org, user uuid.UUID) ([]TelemetryToken, error) {
	rows, err := s.q.Query(ctx, "UPDATE telemetry_tokens SET revoked_at = now() WHERE org_id = $1 AND created_by = $2 AND revoked_at IS NULL RETURNING "+telemetryTokenColumns, org, user)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (TelemetryToken, error) { return scanTelemetryToken(row) })
}

func (s *Store) TelemetryTokenBySecret(ctx context.Context, secret string) (TelemetryToken, error) {
	return scanTelemetryToken(s.q.QueryRow(ctx, "SELECT "+telemetryTokenColumns+" FROM telemetry_tokens WHERE token_hash = $1 AND revoked_at IS NULL", hashToken(secret)))
}

func (s *Store) TouchTelemetryToken(ctx context.Context, id uuid.UUID) error {
	_, err := s.q.Exec(ctx, "UPDATE telemetry_tokens SET last_used_at = now() WHERE id = $1 AND (last_used_at IS NULL OR last_used_at < now() - make_interval(secs => $2))", id, telemetryTouchMinimum.Seconds())
	return err
}

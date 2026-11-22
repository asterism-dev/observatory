package store

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/asterism-dev/observatory/internal/seal"
)

// gcmNonceSize is the nonce length seal.Seal uses, stored ahead of the ciphertext.
const gcmNonceSize = 12

// Secret returns the secret name, sealed under key; on first use it stores generate's value, and concurrent first uses agree on one.
func (s *Store) Secret(ctx context.Context, key seal.Key, name string, generate func() ([]byte, error)) ([]byte, error) {
	aad := []byte("iam.secrets/" + name)
	load := func() ([]byte, error) {
		var sealed []byte
		if err := s.q.QueryRow(ctx, "SELECT sealed FROM secrets WHERE name = $1", name).Scan(&sealed); err != nil {
			return nil, err
		}
		if len(sealed) < gcmNonceSize {
			return nil, fmt.Errorf("secret %s: truncated", name)
		}
		plain, err := seal.Open(key, sealed[:gcmNonceSize], sealed[gcmNonceSize:], aad)
		if err != nil {
			return nil, fmt.Errorf("secret %s: not sealed with this OBSERVATORY_SECRETS_KEY: %w", name, err)
		}
		return plain, nil
	}
	v, err := load()
	if !errors.Is(err, pgx.ErrNoRows) {
		return v, err
	}
	plain, err := generate()
	if err != nil {
		return nil, err
	}
	nonce, ct, err := seal.Seal(key, plain, aad)
	if err != nil {
		return nil, err
	}
	if _, err := s.q.Exec(ctx, "INSERT INTO secrets (name, sealed) VALUES ($1, $2) ON CONFLICT DO NOTHING", name, append(nonce, ct...)); err != nil {
		return nil, err
	}
	return load()
}

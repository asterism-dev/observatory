package store

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"golang.org/x/crypto/ssh"
)

var ErrInvalidKey = errors.New("invalid ssh public key")

type SSHKey struct {
	ID, UserID                   uuid.UUID
	Name, PublicKey, Fingerprint string
	CreatedAt                    time.Time
}

const sshKeyColumns = "id, user_id, name, public_key, fingerprint, created_at"

func scanSSHKey(row pgx.Row) (k SSHKey, err error) {
	err = row.Scan(&k.ID, &k.UserID, &k.Name, &k.PublicKey, &k.Fingerprint, &k.CreatedAt)
	return
}

func (s *Store) AddSSHKey(ctx context.Context, k SSHKey) (SSHKey, error) {
	pub, _, _, _, err := ssh.ParseAuthorizedKey([]byte(k.PublicKey))
	if err != nil {
		return SSHKey{}, ErrInvalidKey
	}
	out, err := scanSSHKey(s.q.QueryRow(ctx, `
		INSERT INTO ssh_keys (user_id, name, public_key, fingerprint) VALUES ($1, $2, $3, $4)
		RETURNING `+sshKeyColumns,
		k.UserID, k.Name, strings.TrimSpace(string(ssh.MarshalAuthorizedKey(pub))), ssh.FingerprintSHA256(pub)))
	if isUniqueViolation(err) {
		return SSHKey{}, ErrConflict
	}
	return out, err
}

func (s *Store) SSHKeys(ctx context.Context, user uuid.UUID) ([]SSHKey, error) {
	rows, err := s.q.Query(ctx, "SELECT "+sshKeyColumns+" FROM ssh_keys WHERE user_id = $1 ORDER BY created_at, id", user)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (SSHKey, error) { return scanSSHKey(r) })
}

func (s *Store) DeleteSSHKey(ctx context.Context, user, id uuid.UUID) error {
	tag, err := s.q.Exec(ctx, "DELETE FROM ssh_keys WHERE id = $1 AND user_id = $2", id, user)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

func (s *Store) SSHKeysOf(ctx context.Context, users []uuid.UUID) ([]string, error) {
	rows, err := s.q.Query(ctx, "SELECT DISTINCT public_key FROM ssh_keys WHERE user_id = ANY($1) ORDER BY public_key", users)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowTo[string])
}

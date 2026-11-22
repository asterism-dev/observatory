// Package store is iam's data in schema iam: users, orgs, memberships, sessions, invites, teams, roles, bindings, SSH keys and secrets.
package store

import (
	"context"
	"crypto/sha256"
	"errors"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/asterism-dev/observatory/internal/iam/migrations"
	"github.com/asterism-dev/observatory/internal/platform/outbox"
	"github.com/asterism-dev/observatory/internal/platform/pgsvc"
)

const Schema = "iam"

var (
	ErrNotFound      = errors.New("not found")
	ErrOwnsResources = errors.New("owns VMs or disks")
)

// Open migrates schema iam at url and returns a pool on it.
func Open(ctx context.Context, url string) (*pgxpool.Pool, error) {
	return pgsvc.Open(ctx, url, Schema, outbox.Migrations(migrations.FS, 2))
}

type querier interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
	Begin(ctx context.Context) (pgx.Tx, error)
}

type Store struct {
	q querier
}

func New(pool *pgxpool.Pool) *Store { return &Store{q: pool} }

// InTx runs fn in a transaction; on a Store already in one it becomes a savepoint.
func (s *Store) InTx(ctx context.Context, fn func(tx *Store) error) error {
	tx, err := s.q.Begin(ctx)
	if err != nil {
		return err
	}
	// Rollback after a successful Commit is a no-op.
	defer func() { _ = tx.Rollback(ctx) }()
	if err := fn(&Store{q: tx}); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func isUniqueViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505"
}

func hashToken(token string) []byte {
	h := sha256.Sum256([]byte(token))
	return h[:]
}

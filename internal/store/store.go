package store

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

var ErrNotFound = errors.New("not found")

type querier interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
	Begin(ctx context.Context) (pgx.Tx, error)
}

type Store struct{ q querier }

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

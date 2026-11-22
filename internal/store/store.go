package store

import (
	"context"
	"crypto/sha256"
	"errors"
	"slices"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/asterism-dev/observatory/internal/seal"
)

var (
	ErrNotFound = errors.New("not found")
	ErrConflict = errors.New("already exists")
)

type querier interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
	Begin(ctx context.Context) (pgx.Tx, error)
}

type Store struct {
	q      querier
	master *seal.Key
	quota  Quota
	// pending is the outermost transaction's quota work, shared by its savepoints; nil outside a transaction.
	pending *pendingQuota
}

func New(pool *pgxpool.Pool, quota Quota) *Store { return &Store{q: pool, quota: quota} }

// WithMasterKey returns a copy of s that wraps org keys with k.
func (s *Store) WithMasterKey(k seal.Key) *Store { return &Store{q: s.q, master: &k, quota: s.quota} }

// InTx runs fn in a transaction, or a savepoint on a Store already in one; the outermost commits its quota holds with it, cancels them on failure and releases deleted resources after.
func (s *Store) InTx(ctx context.Context, fn func(tx *Store) error) error {
	tx, err := s.q.Begin(ctx)
	if err != nil {
		return err
	}
	// Rollback after a successful Commit is a no-op.
	defer func() { _ = tx.Rollback(ctx) }()
	inner := &Store{q: tx, master: s.master, quota: s.quota, pending: s.pending}
	if s.pending != nil {
		reserved, released := len(s.pending.reserved), len(s.pending.released)
		err := fn(inner)
		if err == nil {
			err = tx.Commit(ctx)
		}
		if err != nil {
			// The savepoint's creates and deletes roll back, so its holds go at once and its releases never happen.
			failed := &pendingQuota{reserved: slices.Clone(s.pending.reserved[reserved:])}
			s.pending.reserved, s.pending.released = s.pending.reserved[:reserved], s.pending.released[:released]
			failed.cancel(ctx, s.quota)
		}
		return err
	}
	p := &pendingQuota{}
	inner.pending = p
	err = fn(inner)
	if err == nil {
		err = p.writeCommitted(ctx, tx)
	}
	if err == nil {
		err = tx.Commit(ctx)
	}
	if err != nil {
		p.cancel(ctx, s.quota)
		return err
	}
	p.release(ctx, s.quota)
	return nil
}

func isUniqueViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505"
}

func hashToken(token string) []byte {
	h := sha256.Sum256([]byte(token))
	return h[:]
}

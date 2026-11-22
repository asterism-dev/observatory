package store

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/asterism-dev/observatory/internal/db/dbtest"
)

// NewTestDB returns a pool on a throwaway database with schema iam migrated.
func NewTestDB(t testing.TB) *pgxpool.Pool {
	t.Helper()
	pool, err := Open(context.Background(), dbtest.Empty(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	return pool
}

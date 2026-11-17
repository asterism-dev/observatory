package dbtest

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"net/url"
	"os"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/asterism-dev/observatory/internal/db"
)

const envURL = "OBSERVATORY_TEST_DATABASE_URL"

// New creates a migrated throwaway database and drops it when the test ends.
func New(t testing.TB) *pgxpool.Pool {
	t.Helper()
	ctx := context.Background()

	adminURL := os.Getenv(envURL)
	if adminURL == "" {
		t.Fatalf("%s is not set; run `make dev-up` and use `make test`", envURL)
	}
	suffix := make([]byte, 8)
	if _, err := rand.Read(suffix); err != nil {
		t.Fatal(err)
	}
	name := "obs_test_" + hex.EncodeToString(suffix)

	admin, err := pgx.Connect(ctx, adminURL)
	if err != nil {
		t.Fatalf("connect to %s: %v", envURL, err)
	}
	defer admin.Close(ctx)
	if _, err := admin.Exec(ctx, "CREATE DATABASE "+name); err != nil {
		t.Fatalf("create database: %v", err)
	}

	u, err := url.Parse(adminURL)
	if err != nil {
		t.Fatal(err)
	}
	u.Path = "/" + name
	testURL := u.String()

	var pool *pgxpool.Pool
	t.Cleanup(func() {
		if pool != nil {
			pool.Close()
		}
		conn, err := pgx.Connect(ctx, adminURL)
		if err != nil {
			t.Errorf("connect for cleanup: %v", err)
			return
		}
		defer conn.Close(ctx)
		if _, err := conn.Exec(ctx, "DROP DATABASE "+name+" WITH (FORCE)"); err != nil {
			t.Errorf("drop database: %v", err)
		}
	})

	if err := db.Migrate(ctx, testURL); err != nil {
		t.Fatal(err)
	}
	pool, err = db.Open(ctx, testURL)
	if err != nil {
		t.Fatal(err)
	}
	return pool
}

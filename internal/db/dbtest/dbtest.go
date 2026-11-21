package dbtest

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"net/url"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/asterism-dev/observatory/internal/db"
)

const envURL = "OBSERVATORY_TEST_DATABASE_URL"

// New creates a migrated throwaway database and drops it when the test ends.
func New(t testing.TB) *pgxpool.Pool {
	t.Helper()
	testURL := Empty(t)
	if err := db.Migrate(context.Background(), testURL); err != nil {
		t.Fatal(err)
	}
	pool, err := db.Open(context.Background(), testURL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	return pool
}

// Empty creates an unmigrated throwaway database, drops it when the test ends and returns its URL.
func Empty(t testing.TB) string {
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

	t.Cleanup(func() {
		conn, err := pgx.Connect(ctx, adminURL)
		if err != nil {
			t.Errorf("connect for cleanup: %v", err)
			return
		}
		defer conn.Close(ctx)
		// Closed pool backends or an autovacuum worker may linger briefly; FORCE can't terminate the latter without superuser.
		for range 5 {
			if _, err := conn.Exec(ctx, "DROP DATABASE "+name); err == nil {
				return
			}
			time.Sleep(100 * time.Millisecond)
		}
		if _, err := conn.Exec(ctx, "DROP DATABASE "+name+" WITH (FORCE)"); err != nil {
			t.Logf("drop database %s (left behind): %v", name, err)
		}
	})

	return testURL
}

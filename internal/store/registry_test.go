package store_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/asterism-dev/observatory/internal/db/dbtest"
	"github.com/asterism-dev/observatory/internal/store"
)

func TestRegistryCredentialExpiresWithSession(t *testing.T) {
	ctx := context.Background()
	pool := dbtest.New(t)
	s := store.New(pool)
	user, session := uuid.New(), uuid.New()
	live, _, err := s.CreateRegistryCredential(ctx, user, session, time.Now().Add(time.Hour), 12*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if got, err := s.RegistryCredentialUser(ctx, live); err != nil || got != user {
		t.Fatalf("live: %v %v, want %v", got, err, user)
	}
	ended, _, err := s.CreateRegistryCredential(ctx, user, session, time.Now().Add(-time.Second), 12*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.RegistryCredentialUser(ctx, ended); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("after the session expired: %v", err)
	}
	if _, _, err := s.CreateRegistryCredential(ctx, user, uuid.New(), time.Now().Add(time.Hour), 12*time.Hour); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := pool.QueryRow(ctx, "SELECT count(*) FROM registry_credentials WHERE session_id = $1", session).Scan(&n); err != nil || n != 1 {
		t.Fatalf("credentials of the session after cleanup: %d %v, want the live one", n, err)
	}
}

func TestRevokedSessionGetsNoRegistryCredential(t *testing.T) {
	ctx := context.Background()
	pool := dbtest.New(t)
	s := store.New(pool)
	user, session := uuid.New(), uuid.New()
	expires := time.Now().Add(time.Hour)
	for range 2 {
		if err := s.DeleteSessionCredentials(ctx, session); err != nil {
			t.Fatal(err)
		}
	}
	if _, _, err := s.CreateRegistryCredential(ctx, user, session, expires, 12*time.Hour); !errors.Is(err, store.ErrSessionRevoked) {
		t.Fatalf("create for a revoked session: %v", err)
	}
	// A credential whose insert raced the revocation must not work either.
	execSQL(t, pool, `INSERT INTO registry_credentials (user_id, session_id, session_expires_at, secret_hash, expires_at)
		VALUES ($1, $2, $3, sha256('raced'), now() + interval '1 hour')`, user, session, expires)
	if _, err := s.RegistryCredentialUser(ctx, "raced"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("raced credential: %v", err)
	}
	execSQL(t, pool, `UPDATE registry_revoked_sessions SET expires_at = now() - interval '1 second'`)
	if _, _, err := s.CreateRegistryCredential(ctx, user, uuid.New(), expires, 12*time.Hour); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := pool.QueryRow(ctx, "SELECT count(*) FROM registry_revoked_sessions").Scan(&n); err != nil || n != 0 {
		t.Fatalf("expired revocation records: %d %v", n, err)
	}
}

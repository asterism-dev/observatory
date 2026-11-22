package store_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/asterism-dev/observatory/internal/db/dbtest"
	"github.com/asterism-dev/observatory/internal/store"
	"github.com/asterism-dev/observatory/internal/store/storetest"
)

func TestRegistryCredentialExpiresWithSession(t *testing.T) {
	ctx := context.Background()
	pool := dbtest.New(t)
	s := storetest.New(t, pool)
	user, session := uuid.New(), uuid.New()
	live, _, err := s.CreateRegistryCredential(ctx, user, session, time.Now().Add(time.Hour), 12*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if got, sess, err := s.RegistryCredentialUser(ctx, live); err != nil || got != user || sess != session {
		t.Fatalf("live: %v %v %v, want %v %v", got, sess, err, user, session)
	}
	ended, _, err := s.CreateRegistryCredential(ctx, user, session, time.Now().Add(-time.Second), 12*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.RegistryCredentialUser(ctx, ended); !errors.Is(err, store.ErrNotFound) {
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

func TestDeleteSessionCredentials(t *testing.T) {
	ctx := context.Background()
	pool := dbtest.New(t)
	s := storetest.New(t, pool)
	user, session := uuid.New(), uuid.New()
	cred, _, err := s.CreateRegistryCredential(ctx, user, session, time.Now().Add(time.Hour), 12*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err := s.DeleteSessionCredentials(ctx, session); err != nil {
			t.Fatal(err)
		}
	}
	if _, _, err := s.RegistryCredentialUser(ctx, cred); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("deleted credential: %v", err)
	}
}

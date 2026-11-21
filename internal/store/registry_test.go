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
	s := store.New(dbtest.New(t))
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
}

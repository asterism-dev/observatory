package store_test

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/asterism-dev/observatory/internal/iam/store"
)

func newSessionUser(t *testing.T, s *store.Store, sub string) uuid.UUID {
	t.Helper()
	res, err := s.Login(context.Background(), store.Identity{Subject: sub, Email: sub + "@x.io", Name: sub})
	if err != nil {
		t.Fatal(err)
	}
	return res.User.ID
}

func TestSessionRoundTrip(t *testing.T) {
	ctx := context.Background()
	pool := store.NewTestDB(t)
	s := store.New(pool)
	uid := newSessionUser(t, s, "a")

	token, sess, err := s.CreateSession(ctx, uid, store.SessionBrowser, time.Hour, "ua", "1.2.3.4")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(token, "obs_") {
		t.Fatalf("token %q lacks prefix", token)
	}
	got, err := s.SessionByToken(ctx, token)
	if err != nil {
		t.Fatal(err)
	}
	if got.ID != sess.ID || got.UserID != uid || got.Kind != store.SessionBrowser || got.UserAgent != "ua" || got.IP != "1.2.3.4" {
		t.Fatalf("unexpected session: %+v", got)
	}
	var hash []byte
	if err := pool.QueryRow(ctx, "SELECT token_hash FROM sessions WHERE id = $1", sess.ID).Scan(&hash); err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(hash, []byte(token)) {
		t.Fatal("token stored in plaintext")
	}
	if _, err := s.SessionByToken(ctx, "obs_unknown"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("unknown token: got %v", err)
	}
}

func TestExpiredSessionIsNotFound(t *testing.T) {
	ctx := context.Background()
	s := store.New(store.NewTestDB(t))
	uid := newSessionUser(t, s, "a")

	token, _, err := s.CreateSession(ctx, uid, store.SessionCLI, -time.Second, "", "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.SessionByToken(ctx, token); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("got %v", err)
	}
}

func TestRevokedSessionIsNotFound(t *testing.T) {
	ctx := context.Background()
	s := store.New(store.NewTestDB(t))
	uid := newSessionUser(t, s, "a")

	token, sess, err := s.CreateSession(ctx, uid, store.SessionBrowser, time.Hour, "", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.RevokeSession(ctx, uid, sess.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SessionByToken(ctx, token); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("got %v", err)
	}
	list, err := s.Sessions(ctx, uid)
	if err != nil || len(list) != 0 {
		t.Fatalf("sessions: %v %+v", err, list)
	}
	if err := s.RevokeSession(ctx, uid, sess.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("second revoke: got %v", err)
	}
}

func TestCannotRevokeOtherUsersSession(t *testing.T) {
	ctx := context.Background()
	s := store.New(store.NewTestDB(t))
	a := newSessionUser(t, s, "a")
	b := newSessionUser(t, s, "b")

	token, sess, err := s.CreateSession(ctx, a, store.SessionBrowser, time.Hour, "", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.RevokeSession(ctx, b, sess.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("got %v", err)
	}
	if _, err := s.SessionByToken(ctx, token); err != nil {
		t.Fatalf("session should still be valid: %v", err)
	}
}

func TestSessionLastUsedBumpedOnlyWhenStale(t *testing.T) {
	ctx := context.Background()
	pool := store.NewTestDB(t)
	s := store.New(pool)
	uid := newSessionUser(t, s, "a")

	token, sess, err := s.CreateSession(ctx, uid, store.SessionBrowser, time.Hour, "", "")
	if err != nil {
		t.Fatal(err)
	}
	got, err := s.SessionByToken(ctx, token)
	if err != nil || !got.LastUsedAt.Equal(sess.LastUsedAt) {
		t.Fatalf("fresh session bumped: %v %v vs %v", err, got.LastUsedAt, sess.LastUsedAt)
	}
	if _, err := pool.Exec(ctx, "UPDATE sessions SET last_used_at = now() - interval '5 minutes' WHERE id = $1", sess.ID); err != nil {
		t.Fatal(err)
	}
	got, err = s.SessionByToken(ctx, token)
	if err != nil || time.Since(got.LastUsedAt) > time.Minute {
		t.Fatalf("stale session not bumped: %v %v", err, got.LastUsedAt)
	}
}

package store_test

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"

	"github.com/asterism-dev/observatory/internal/db/dbtest"
	"github.com/asterism-dev/observatory/internal/store"
)

const (
	testKey1 = "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIO+6yIP+G69X+7mX0h8gSHrNNG4s+hDeXmzGTGYeH6nA laptop"
	testKey2 = "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIEJpmT82ezqhgaQWfjAbLfbMs82g7JPP/E1drrSals/R other"
)

func TestSSHKeys(t *testing.T) {
	ctx := context.Background()
	s := store.New(dbtest.New(t))
	a, b := login(t, s, "a"), login(t, s, "b")

	if _, err := s.AddSSHKey(ctx, store.SSHKey{UserID: a.User.ID, Name: "x", PublicKey: "not a key"}); !errors.Is(err, store.ErrInvalidKey) {
		t.Fatalf("invalid: %v", err)
	}
	k, err := s.AddSSHKey(ctx, store.SSHKey{UserID: a.User.ID, Name: "laptop", PublicKey: testKey1})
	if err != nil || k.Fingerprint == "" {
		t.Fatalf("add: %+v %v", k, err)
	}
	if _, err := s.AddSSHKey(ctx, store.SSHKey{UserID: a.User.ID, Name: "again", PublicKey: testKey1}); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("dup: %v", err)
	}
	if _, err := s.AddSSHKey(ctx, store.SSHKey{UserID: b.User.ID, Name: "same", PublicKey: testKey1}); err != nil {
		t.Fatalf("other user same key: %v", err)
	}
	if _, err := s.AddSSHKey(ctx, store.SSHKey{UserID: b.User.ID, Name: "o", PublicKey: testKey2}); err != nil {
		t.Fatal(err)
	}
	if list, err := s.SSHKeys(ctx, a.User.ID); err != nil || len(list) != 1 {
		t.Fatalf("list: %+v %v", list, err)
	}
	if keys, err := s.SSHKeysOf(ctx, []uuid.UUID{a.User.ID, b.User.ID}); err != nil || len(keys) != 3 {
		t.Fatalf("of: %v %v", keys, err)
	}
	if err := s.DeleteSSHKey(ctx, b.User.ID, k.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("other's delete: %v", err)
	}
	if err := s.DeleteSSHKey(ctx, a.User.ID, k.ID); err != nil {
		t.Fatal(err)
	}
}

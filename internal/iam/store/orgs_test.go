package store_test

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"

	"github.com/asterism-dev/observatory/internal/iam/store"
)

func TestCreateAndRenameOrg(t *testing.T) {
	ctx := context.Background()
	s := store.New(store.NewTestDB(t))
	res, err := s.Login(ctx, store.Identity{Subject: "a", Email: "a@x.io", Name: "A"})
	if err != nil {
		t.Fatal(err)
	}
	org, err := s.CreateOrg(ctx, "Acme", res.User.ID)
	if err != nil {
		t.Fatal(err)
	}
	if org.Name != "Acme" || org.Personal || org.ID == uuid.Nil {
		t.Fatalf("org: %+v", org)
	}
	m, err := s.Membership(ctx, org.ID, res.User.ID)
	if err != nil || m.Role != store.RoleOwner || m.Org.ID != org.ID || m.Org.Name != "Acme" {
		t.Fatalf("membership: %+v %v", m, err)
	}

	if err := s.RenameOrg(ctx, org.ID, "Acme Inc"); err != nil {
		t.Fatal(err)
	}
	if m, _ := s.Membership(ctx, org.ID, res.User.ID); m.Org.Name != "Acme Inc" {
		t.Fatalf("rename: %+v", m)
	}
	if err := s.RenameOrg(ctx, res.PersonalOrg.ID, "x"); !errors.Is(err, store.ErrPersonalOrg) {
		t.Fatalf("personal rename: %v", err)
	}
	if err := s.RenameOrg(ctx, uuid.New(), "x"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("missing org rename: %v", err)
	}
	if _, err := s.Membership(ctx, org.ID, uuid.New()); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("non-member: %v", err)
	}
}

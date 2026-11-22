package store_test

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"

	"github.com/asterism-dev/observatory/internal/iam/store"
)

func TestOrgsAndUsers(t *testing.T) {
	ctx := context.Background()
	s := store.New(store.NewTestDB(t))
	a, b := login(t, s, "a"), login(t, s, "b")
	org, _ := s.CreateOrg(ctx, "Acme", a.User.ID)
	if err := s.SetMemberRole(ctx, org.ID, a.User.ID, store.RoleOwner); err != nil {
		t.Fatal(err)
	}
	orgs, err := s.Orgs(ctx)
	if err != nil || len(orgs) != 3 {
		t.Fatalf("orgs: %+v %v", orgs, err)
	}
	for _, o := range orgs {
		if o.Org.ID == org.ID && o.MemberCount != 1 {
			t.Fatalf("member count: %+v", o)
		}
	}
	if us, err := s.Users(ctx); err != nil || len(us) != 2 {
		t.Fatalf("users: %+v %v", us, err)
	}
	if got, err := s.Org(ctx, org.ID); err != nil || got.Name != "Acme" {
		t.Fatalf("org: %+v %v", got, err)
	}
	if _, err := s.Org(ctx, b.User.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("unknown org: %v", err)
	}
}

func TestLastPlatformAdminStore(t *testing.T) {
	ctx := context.Background()
	s := store.New(store.NewTestDB(t))
	a, b := login(t, s, "a"), login(t, s, "b")
	if !a.User.PlatformAdmin || b.User.PlatformAdmin {
		t.Fatal("first user should be the only admin")
	}
	if err := s.SetPlatformAdmin(ctx, a.User.ID, false); !errors.Is(err, store.ErrLastPlatformAdmin) {
		t.Fatalf("revoke last: %v", err)
	}
	if err := s.SetPlatformAdmin(ctx, uuid.New(), true); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("unknown: %v", err)
	}
	if err := s.SetPlatformAdmin(ctx, b.User.ID, true); err != nil {
		t.Fatal(err)
	}
	if err := s.SetPlatformAdmin(ctx, b.User.ID, true); err != nil {
		t.Fatalf("grant twice: %v", err)
	}

	errs := make(chan error, 2)
	go func() { errs <- s.SetPlatformAdmin(ctx, a.User.ID, false) }()
	go func() { errs <- s.SetPlatformAdmin(ctx, b.User.ID, false) }()
	var last int
	for range 2 {
		if errors.Is(<-errs, store.ErrLastPlatformAdmin) {
			last++
		}
	}
	us, _ := s.Users(ctx)
	admins := 0
	for _, u := range us {
		if u.PlatformAdmin {
			admins++
		}
	}
	if last != 1 || admins != 1 {
		t.Fatalf("concurrent revoke: last=%d admins=%d", last, admins)
	}
}

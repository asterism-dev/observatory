package store_test

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"

	"github.com/asterism-dev/observatory/internal/db/dbtest"
	"github.com/asterism-dev/observatory/internal/store"
)

func TestLoginCreatesUserWithPersonalOrg(t *testing.T) {
	ctx := context.Background()
	s := store.New(dbtest.New(t))

	res, err := s.Login(ctx, store.Identity{Subject: "sub-1", Email: "a@x.io", Name: "Ada"})
	if err != nil {
		t.Fatal(err)
	}
	if !res.Created || res.PersonalOrg == nil || res.PersonalOrg.Name != "Ada" {
		t.Fatalf("unexpected result: %+v", res)
	}
	ms, err := s.Memberships(ctx, res.User.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(ms) != 1 || ms[0].Role != store.RoleOwner || !ms[0].Org.Personal {
		t.Fatalf("unexpected memberships: %+v", ms)
	}
}

func TestFirstUserBecomesPlatformAdmin(t *testing.T) {
	ctx := context.Background()
	s := store.New(dbtest.New(t))

	first, err := s.Login(ctx, store.Identity{Subject: "sub-1", Email: "a@x.io", Name: "Ada"})
	if err != nil {
		t.Fatal(err)
	}
	if !first.BecamePlatformAdmin || !first.User.PlatformAdmin {
		t.Fatalf("first user not admin: %+v", first)
	}
	second, err := s.Login(ctx, store.Identity{Subject: "sub-2", Email: "b@x.io", Name: "Bob"})
	if err != nil {
		t.Fatal(err)
	}
	if second.BecamePlatformAdmin || second.User.PlatformAdmin {
		t.Fatalf("second user is admin: %+v", second)
	}
}

func TestLoginIsIdempotentPerSubject(t *testing.T) {
	ctx := context.Background()
	s := store.New(dbtest.New(t))

	first, err := s.Login(ctx, store.Identity{Subject: "sub-1", Email: "a@x.io", Name: "Ada"})
	if err != nil {
		t.Fatal(err)
	}
	second, err := s.Login(ctx, store.Identity{Subject: "sub-1", Email: "new@x.io", Name: "Ada"})
	if err != nil {
		t.Fatal(err)
	}
	if second.Created || second.PersonalOrg != nil || second.BecamePlatformAdmin {
		t.Fatalf("second login should not create: %+v", second)
	}
	if second.User.ID != first.User.ID || second.User.Email != "new@x.io" {
		t.Fatalf("unexpected user: %+v", second.User)
	}
	if !second.User.PlatformAdmin {
		t.Fatal("admin flag must survive a re-login")
	}
	ms, err := s.Memberships(ctx, first.User.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(ms) != 1 {
		t.Fatalf("want 1 membership, got %d", len(ms))
	}
}

func TestConcurrentFirstLoginsYieldOneAdmin(t *testing.T) {
	ctx := context.Background()
	pool := dbtest.New(t)
	s := store.New(pool)

	const n = 10
	results := make([]store.LoginResult, n)
	errs := make([]error, n)
	var wg sync.WaitGroup
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			results[i], errs[i] = s.Login(ctx, store.Identity{
				Subject: fmt.Sprintf("sub-%d", i), Email: fmt.Sprintf("u%d@x.io", i), Name: fmt.Sprintf("U%d", i),
			})
		}()
	}
	wg.Wait()

	admins := 0
	for i := range n {
		if errs[i] != nil {
			t.Fatal(errs[i])
		}
		if results[i].BecamePlatformAdmin {
			admins++
		}
	}
	if admins != 1 {
		t.Fatalf("want 1 BecamePlatformAdmin, got %d", admins)
	}
	var count int
	if err := pool.QueryRow(ctx, "SELECT count(*) FROM users WHERE platform_admin").Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("want 1 platform admin row, got %d", count)
	}
}

func TestPersonalOrgFallsBackToEmail(t *testing.T) {
	s := store.New(dbtest.New(t))

	res, err := s.Login(context.Background(), store.Identity{Subject: "sub-1", Email: "a@x.io"})
	if err != nil {
		t.Fatal(err)
	}
	if res.PersonalOrg == nil || res.PersonalOrg.Name != "a@x.io" {
		t.Fatalf("unexpected org: %+v", res.PersonalOrg)
	}
}

func TestInTxRollsBack(t *testing.T) {
	ctx := context.Background()
	pool := dbtest.New(t)
	s := store.New(pool)

	boom := errors.New("boom")
	err := s.InTx(ctx, func(tx *store.Store) error {
		if _, err := tx.Login(ctx, store.Identity{Subject: "sub-1", Email: "a@x.io", Name: "Ada"}); err != nil {
			return err
		}
		return boom
	})
	if !errors.Is(err, boom) {
		t.Fatalf("want boom, got %v", err)
	}
	var count int
	if err := pool.QueryRow(ctx, "SELECT count(*) FROM users").Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatalf("want no users after rollback, got %d", count)
	}
}

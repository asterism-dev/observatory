package store_test

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"

	"github.com/asterism-dev/observatory/internal/authz"
	"github.com/asterism-dev/observatory/internal/db/dbtest"
	"github.com/asterism-dev/observatory/internal/store"
)

func ip(n int) *int { return &n }

// policyKnown answers SetLimits' scope check from the current policy, as the API does.
func policyKnown(t *testing.T, s *store.Store, org uuid.UUID) func(string) bool {
	t.Helper()
	p, err := s.PolicySnapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	e, err := authz.New(p)
	if err != nil {
		t.Fatal(err)
	}
	return func(scope string) bool {
		kind, raw, _ := strings.Cut(scope, ":")
		id, err := uuid.Parse(raw)
		if err != nil {
			return false
		}
		if kind == "team" {
			return e.HasTeam(org, id)
		}
		_, ok := e.Role(org, id)
		return ok
	}
}

func TestLimitsStack(t *testing.T) {
	ctx := context.Background()
	s := store.New(dbtest.New(t))
	a, b := login(t, s, "a"), login(t, s, "b")
	org, _ := s.CreateOrg(ctx, "A", a.User.ID)
	other, _ := s.CreateOrg(ctx, "B", b.User.ID)
	team, _ := s.CreateTeam(ctx, org.ID, "core")
	foreign, _ := s.CreateTeam(ctx, other.ID, "x")
	teamScope, userScope := "team:"+team.ID.String(), "user:"+a.User.ID.String()

	if err := s.SetLimits(ctx, org.ID, "org", store.Limits{VCPU: ip(10), DiskGB: ip(100)}, nil); err != nil {
		t.Fatal(err)
	}
	if err := s.SetLimits(ctx, org.ID, teamScope, store.Limits{VCPU: ip(11)}, policyKnown(t, s, org.ID)); !errors.Is(err, store.ErrLimitExceedsOrg) {
		t.Fatalf("team above org: %v", err)
	}
	if err := s.SetLimits(ctx, org.ID, userScope, store.Limits{DiskGB: ip(101)}, policyKnown(t, s, org.ID)); !errors.Is(err, store.ErrLimitExceedsOrg) {
		t.Fatalf("user above org: %v", err)
	}
	// nulls are unlimited and RAM is unlimited at org level, so any team RAM is fine.
	if err := s.SetLimits(ctx, org.ID, teamScope, store.Limits{VCPU: ip(10), RAMMB: ip(1 << 20)}, policyKnown(t, s, org.ID)); err != nil {
		t.Fatalf("team at org: %v", err)
	}
	if err := s.SetLimits(ctx, org.ID, userScope, store.Limits{}, policyKnown(t, s, org.ID)); err != nil {
		t.Fatalf("all null: %v", err)
	}
	if err := s.SetLimits(ctx, org.ID, "org", store.Limits{VCPU: ip(9), DiskGB: ip(100)}, nil); !errors.Is(err, store.ErrLimitExceedsOrg) {
		t.Fatalf("org below team: %v", err)
	}
	if err := s.SetLimits(ctx, org.ID, "org", store.Limits{DiskGB: ip(100)}, nil); err != nil {
		t.Fatalf("org unlimited vcpu: %v", err)
	}
	if err := s.SetLimits(ctx, org.ID, "org", store.Limits{VCPU: ip(10), RAMMB: ip(5)}, nil); !errors.Is(err, store.ErrLimitExceedsOrg) {
		t.Fatalf("org ram below team: %v", err)
	}
	ls, err := s.Limits(ctx, org.ID)
	if err != nil || ls["org"].VCPU != nil || *ls["org"].DiskGB != 100 || *ls[teamScope].VCPU != 10 || ls[userScope].VCPU != nil {
		t.Fatalf("limits: %+v %v", ls, err)
	}

	for _, scope := range []string{"team:" + foreign.ID.String(), "user:" + b.User.ID.String(), "team:" + uuid.NewString(),
		"team:nope", "user:" + uuid.NewString(), "bogus", "", "TEAM:" + team.ID.String(), "team:" + uuid.Nil.String()} {
		if err := s.SetLimits(ctx, org.ID, scope, store.Limits{}, policyKnown(t, s, org.ID)); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("scope %q: %v", scope, err)
		}
	}
	if ls, _ := s.Limits(ctx, other.ID); len(ls) != 0 {
		t.Fatalf("foreign org touched: %+v", ls)
	}
}

func TestLimitsConcurrentOrgLowerVsTeamRaise(t *testing.T) {
	ctx := context.Background()
	s := store.New(dbtest.New(t))
	a := login(t, s, "a")
	org, _ := s.CreateOrg(ctx, "A", a.User.ID)
	team, _ := s.CreateTeam(ctx, org.ID, "core")
	if err := s.SetLimits(ctx, org.ID, "org", store.Limits{VCPU: ip(10)}, nil); err != nil {
		t.Fatal(err)
	}
	known := policyKnown(t, s, org.ID)
	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); _ = s.SetLimits(ctx, org.ID, "org", store.Limits{VCPU: ip(5)}, nil) }()
	go func() {
		defer wg.Done()
		_ = s.SetLimits(ctx, org.ID, "team:"+team.ID.String(), store.Limits{VCPU: ip(8)}, known)
	}()
	wg.Wait()
	ls, _ := s.Limits(ctx, org.ID)
	if tl, ok := ls["team:"+team.ID.String()]; ok && tl.VCPU != nil && *tl.VCPU > *ls["org"].VCPU {
		t.Fatalf("team above org: %+v", ls)
	}
}

func TestOrgsAndUsers(t *testing.T) {
	ctx := context.Background()
	s := store.New(dbtest.New(t))
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
	s := store.New(dbtest.New(t))
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

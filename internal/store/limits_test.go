package store_test

import (
	"context"
	"errors"
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
	p, err := policySnapshot(s, context.Background())
	if err != nil {
		t.Fatal(err)
	}
	e, err := authz.New(p)
	if err != nil {
		t.Fatal(err)
	}
	return func(scope string) bool { return e.HasSubject(org, scope) }
}

func TestLimitsStack(t *testing.T) {
	ctx := context.Background()
	s := store.New(dbtest.New(t))
	a, b := login(t, s, "a"), login(t, s, "b")
	org, _ := createOrg(s, ctx, "A", a.User.ID)
	other, _ := createOrg(s, ctx, "B", b.User.ID)
	team, _ := createTeam(s, ctx, org.ID, "core")
	foreign, _ := createTeam(s, ctx, other.ID, "x")
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
	org, _ := createOrg(s, ctx, "A", a.User.ID)
	team, _ := createTeam(s, ctx, org.ID, "core")
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

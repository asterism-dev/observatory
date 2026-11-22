package authz

import (
	"context"
	"errors"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
)

func memberPolicy(org, user uuid.UUID, role string) Policy {
	return Policy{Memberships: []MemberRole{{OrgID: org, UserID: user, Role: role}}}
}

func TestServiceReloadPicksUpChanges(t *testing.T) {
	org, user := uuid.New(), uuid.New()
	var calls atomic.Int32
	svc, err := NewService(context.Background(), func(context.Context) (Policy, error) {
		if calls.Add(1) == 1 {
			return memberPolicy(org, user, "member"), nil
		}
		return memberPolicy(org, user, "owner"), nil
	})
	if err != nil {
		t.Fatal(err)
	}
	sub := UserSubject(user)
	if svc.Allowed(sub, org, ObjMembers, "iam.members.setRole") {
		t.Fatal("member must not set roles")
	}
	if err := svc.Reload(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !svc.Allowed(sub, org, ObjMembers, "iam.members.setRole") {
		t.Fatal("owner should set roles after reload")
	}
}

func TestServiceReloadFailureKeepsOldEnforcer(t *testing.T) {
	org, user := uuid.New(), uuid.New()
	var fail atomic.Bool
	svc, err := NewService(context.Background(), func(context.Context) (Policy, error) {
		if fail.Load() {
			return Policy{}, errors.New("db down")
		}
		return memberPolicy(org, user, "owner"), nil
	})
	if err != nil {
		t.Fatal(err)
	}
	old := svc.Enforcer()
	fail.Store(true)
	if err := svc.Reload(context.Background()); err == nil {
		t.Fatal("want error")
	}
	if svc.Enforcer() != old || !svc.Allowed(UserSubject(user), org, ObjMembers, "iam.members.setRole") {
		t.Fatal("old enforcer must stay in place")
	}
}

func TestServiceInvalidPolicyKeepsOldEnforcer(t *testing.T) {
	org, user := uuid.New(), uuid.New()
	var bad atomic.Bool
	svc, err := NewService(context.Background(), func(context.Context) (Policy, error) {
		if bad.Load() {
			return memberPolicy(org, user, "bogus"), nil
		}
		return memberPolicy(org, user, "owner"), nil
	})
	if err != nil {
		t.Fatal(err)
	}
	old := svc.Enforcer()
	bad.Store(true)
	if err := svc.Reload(context.Background()); err == nil || svc.Enforcer() != old {
		t.Fatal("invalid policy must fail reload and keep the old enforcer")
	}
}

func TestNewServiceFailsOnInitialLoadError(t *testing.T) {
	if _, err := NewService(context.Background(), func(context.Context) (Policy, error) {
		return Policy{}, errors.New("boom")
	}); err == nil {
		t.Fatal("want error")
	}
}

func TestServiceConcurrentReloads(t *testing.T) {
	org, user := uuid.New(), uuid.New()
	svc, err := NewService(context.Background(), func(context.Context) (Policy, error) {
		return memberPolicy(org, user, "owner"), nil
	})
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for range 20 {
		wg.Add(2)
		go func() {
			defer wg.Done()
			if err := svc.Reload(context.Background()); err != nil {
				t.Error(err)
			}
		}()
		go func() {
			defer wg.Done()
			for range 50 {
				if !svc.Allowed(UserSubject(user), org, ObjMembers, "iam.members.setRole") {
					t.Error("denied during reload")
				}
			}
		}()
	}
	wg.Wait()
}

func TestServiceCurrent(t *testing.T) {
	org, user := uuid.New(), uuid.New()
	svc, err := NewService(context.Background(), func(context.Context) (Policy, error) { return memberPolicy(org, user, "owner"), nil })
	if err != nil {
		t.Fatal(err)
	}
	e, ok := svc.Current()
	if !ok || e != svc.Enforcer() {
		t.Fatalf("Current = %v, %v; want the current enforcer", e, ok)
	}
}

func TestServiceFailsClosedWhenNotFresh(t *testing.T) {
	org, user := uuid.New(), uuid.New()
	svc, err := NewService(context.Background(), func(context.Context) (Policy, error) { return memberPolicy(org, user, "owner"), nil })
	if err != nil {
		t.Fatal(err)
	}
	sub := UserSubject(user)
	var fresh atomic.Bool
	svc.WithFreshness(fresh.Load)
	if _, ok := svc.Current(); ok {
		t.Fatal("current while not fresh")
	}
	if svc.Allowed(sub, org, ObjMembers, "iam.members.setRole") || len(svc.Enforcer().OrgsOf(user)) != 0 {
		t.Fatal("a stale policy still grants")
	}
	if _, ok := svc.LastKnown().Role(org, user); !ok {
		t.Fatal("LastKnown dropped the stale policy")
	}
	fresh.Store(true)
	if e, ok := svc.Current(); !ok || e != svc.Enforcer() || !svc.Allowed(sub, org, ObjMembers, "iam.members.setRole") {
		t.Fatal("not current while fresh")
	}
}

func TestServiceOnReloadPassesOldAndNew(t *testing.T) {
	org, user := uuid.New(), uuid.New()
	var role atomic.Value
	role.Store("member")
	svc, err := NewService(context.Background(), func(context.Context) (Policy, error) {
		return memberPolicy(org, user, role.Load().(string)), nil
	})
	if err != nil {
		t.Fatal(err)
	}
	var got [][2]string
	svc.OnReload(func(old, new Policy) {
		got = append(got, [2]string{old.Memberships[0].Role, new.Memberships[0].Role})
	})
	role.Store("owner")
	if err := svc.Reload(context.Background()); err != nil {
		t.Fatal(err)
	}
	if want := [][2]string{{"member", "member"}, {"member", "owner"}}; !slices.Equal(got, want) {
		t.Fatalf("OnReload calls %v, want %v", got, want)
	}
}

// roleLoad loads memberPolicy with the current role; while gate is set, each load captures the role, then waits for gate.
type roleLoad struct {
	org, user uuid.UUID
	role      atomic.Value
	gate      atomic.Pointer[chan struct{}]
	started   chan string
}

func newRoleLoad(role string) *roleLoad {
	l := &roleLoad{org: uuid.New(), user: uuid.New(), started: make(chan string, 16)}
	l.role.Store(role)
	return l
}

func (l *roleLoad) load(ctx context.Context) (Policy, error) {
	role := l.role.Load().(string)
	if g := l.gate.Load(); g != nil {
		l.started <- role
		select {
		case <-*g:
		case <-ctx.Done():
			return Policy{}, ctx.Err()
		}
	}
	return memberPolicy(l.org, l.user, role), nil
}

func (l *roleLoad) roleIn(s *Service) string {
	r, _ := s.LastKnown().Role(l.org, l.user)
	return r
}

func TestReloadLoadsOutsideTheHold(t *testing.T) {
	l := newRoleLoad("member")
	svc, err := NewService(context.Background(), l.load)
	if err != nil {
		t.Fatal(err)
	}
	gate := make(chan struct{})
	l.gate.Store(&gate)
	release := svc.HoldReloads()
	done := make(chan error, 1)
	go func() { done <- svc.Reload(context.Background()) }()
	// A load waits for a database connection; under the hold that wait can starve the holders' transactions.
	select {
	case <-l.started:
	case <-time.After(2 * time.Second):
		release()
		t.Fatal("reload waited for the hold before loading")
	}
	l.gate.Store(nil)
	close(gate)
	release()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestReloadAcrossAHoldSeesTheHeldWrite(t *testing.T) {
	l := newRoleLoad("member")
	svc, err := NewService(context.Background(), l.load)
	if err != nil {
		t.Fatal(err)
	}
	gate := make(chan struct{})
	l.gate.Store(&gate)
	done := make(chan error, 1)
	go func() { done <- svc.Reload(context.Background()) }()
	if role := <-l.started; role != "member" {
		t.Fatalf("first load saw %q", role)
	}
	release := svc.HoldReloads()
	l.role.Store("owner")
	release()
	l.gate.Store(nil)
	close(gate)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if r := l.roleIn(svc); r != "owner" {
		t.Fatalf("a load started before the hold ended was published: role %q", r)
	}
}

func TestReloadKeepsLoadOrder(t *testing.T) {
	l := newRoleLoad("member")
	svc, err := NewService(context.Background(), l.load)
	if err != nil {
		t.Fatal(err)
	}
	gate := make(chan struct{})
	l.gate.Store(&gate)
	done := make(chan error, 1)
	go func() { done <- svc.Reload(context.Background()) }()
	<-l.started
	l.gate.Store(nil)
	l.role.Store("owner")
	if err := svc.Reload(context.Background()); err != nil {
		t.Fatal(err)
	}
	close(gate)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if r := l.roleIn(svc); r != "owner" {
		t.Fatalf("an older load replaced a newer one: role %q", r)
	}
}

func TestReloadCommittedRetriesUntilCaughtUp(t *testing.T) {
	old := commitRetryWait
	commitRetryWait = 10 * time.Millisecond
	t.Cleanup(func() { commitRetryWait = old })
	l := newRoleLoad("member")
	var fail atomic.Bool
	svc, err := NewService(context.Background(), func(ctx context.Context) (Policy, error) {
		if fail.Load() {
			return Policy{}, errors.New("db down")
		}
		return l.load(ctx)
	})
	if err != nil {
		t.Fatal(err)
	}
	if svc.Behind() {
		t.Fatal("behind before any failure")
	}
	fail.Store(true)
	l.role.Store("owner")
	if err := svc.ReloadCommitted(context.Background()); err == nil {
		t.Fatal("want the reload error")
	}
	if !svc.Behind() {
		t.Fatal("not behind after a failed reload of a commit")
	}
	time.Sleep(50 * time.Millisecond)
	if !svc.Behind() || l.roleIn(svc) != "member" {
		t.Fatal("caught up while every load fails")
	}
	fail.Store(false)
	deadline := time.Now().Add(5 * time.Second)
	for svc.Behind() {
		if time.Now().After(deadline) {
			t.Fatal("the background retry never caught up")
		}
		time.Sleep(5 * time.Millisecond)
	}
	if r := l.roleIn(svc); r != "owner" {
		t.Fatalf("caught up without the commit: role %q", r)
	}
}

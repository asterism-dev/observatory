package authz

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"

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
	if svc.Allowed(sub, org, ObjMembers, "member:set-role") {
		t.Fatal("member must not set roles")
	}
	if err := svc.Reload(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !svc.Allowed(sub, org, ObjMembers, "member:set-role") {
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
	if svc.Enforcer() != old || !svc.Allowed(UserSubject(user), org, ObjMembers, "member:set-role") {
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
				if !svc.Allowed(UserSubject(user), org, ObjMembers, "member:set-role") {
					t.Error("denied during reload")
				}
			}
		}()
	}
	wg.Wait()
}

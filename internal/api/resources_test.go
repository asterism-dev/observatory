package api

import (
	"context"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"connectrpc.com/connect"
	"github.com/google/uuid"

	hubv1 "github.com/asterism-dev/observatory/gen/go/observatory/hub/v1"
	"github.com/asterism-dev/observatory/internal/authz"
	"github.com/asterism-dev/observatory/internal/authz/authztest"
)

// revisionPolicy serves e once its revision reaches the requested one.
type revisionPolicy struct {
	e   *authz.Enforcer
	rev atomic.Uint64
}

func (p *revisionPolicy) Current() (*authz.Enforcer, bool) { return p.e, true }

func (p *revisionPolicy) WaitRevision(ctx context.Context, rev uint64) error {
	for p.rev.Load() < rev {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(5 * time.Millisecond):
		}
	}
	return nil
}

func TestCanHoldWaitsForRevision(t *testing.T) {
	old := canHoldWait
	canHoldWait = 100 * time.Millisecond
	t.Cleanup(func() { canHoldWait = old })
	org, user, owned := uuid.New(), uuid.New(), uuid.New()
	e, err := authz.New(authz.Policy{
		Memberships: []authz.MemberRole{{OrgID: org, UserID: user, Role: "member"}},
		Owned:       []authz.OwnedRef{{Kind: "vms", OrgID: org, ID: owned, UserID: &user}},
		Catalogs:    authztest.Legacy(),
	})
	if err != nil {
		t.Fatal(err)
	}
	p := &revisionPolicy{e: e}
	p.rev.Store(6)
	srv := ResourceServer{Policy: p}
	canHold := func(vm uuid.UUID) error {
		_, err := srv.CanHold(t.Context(), connect.NewRequest(&hubv1.CanHoldRequest{OrgId: org.String(), Actor: authz.UserSubject(user), MinRevision: 7,
			Rules: []*hubv1.Rule{{Effect: "allow", Object: authz.VMObject(vm), Action: "compute.instances.get"}}}))
		return err
	}

	start := time.Now()
	if err := canHold(owned); connect.CodeOf(err) != connect.CodeUnavailable {
		t.Fatalf("below min_revision: %v", err)
	}
	if time.Since(start) < canHoldWait {
		t.Fatal("answered before waiting for the revision")
	}
	p.rev.Store(7)
	if err := canHold(owned); err != nil {
		t.Fatalf("owned VM at min_revision: %v", err)
	}
	err = canHold(uuid.New())
	if connect.CodeOf(err) != connect.CodePermissionDenied || !strings.Contains(err.Error(), authz.ErrEscalation.Error()) {
		t.Fatalf("foreign VM: %v", err)
	}
	if _, err := srv.CanHold(t.Context(), connect.NewRequest(&hubv1.CanHoldRequest{OrgId: "nope", Actor: authz.UserSubject(user)})); connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Fatalf("bad org: %v", err)
	}
}

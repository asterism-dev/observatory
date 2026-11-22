package iamtest

import (
	"context"
	"errors"
	"testing"
	"time"

	coreauthz "github.com/asterism-dev/observatory/internal/authz"
	"github.com/asterism-dev/observatory/internal/platform/authz"
)

// OwnerRefs loads the resource owners a service adds to iam's policy.
type OwnerRefs func(context.Context) ([]coreauthz.OwnedRef, error)

// Policy is an enforcer built like the hub's: the policy watched from the fake plus the owner refs.
type Policy struct {
	*coreauthz.Service
	s *Server
	w *authz.Watcher
}

// Policy watches s until t ends; it reloads only on Sync, so tests decide when a change lands.
func (s *Server) Policy(t testing.TB, owners OwnerRefs) *Policy {
	t.Helper()
	// The hub registers its catalog before it builds this enforcer.
	s.change(func() { s.putCatalog(legacy("hub")) })
	ctx, cancel := context.WithCancel(context.Background())
	w, err := authz.NewWatcher(s.URL(), s.token, nil)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() { w.Run(ctx); close(done) }()
	t.Cleanup(func() { cancel(); <-done })
	wctx, wcancel := context.WithTimeout(ctx, 10*time.Second)
	defer wcancel()
	if err := w.WaitRevision(wctx, s.Revision()); err != nil {
		t.Fatalf("iamtest: first policy: %v", err)
	}
	z, err := coreauthz.NewService(ctx, func(ctx context.Context) (coreauthz.Policy, error) {
		p, _, ok := w.Policy()
		if !ok {
			return p, errors.New("iamtest: no policy yet")
		}
		var err error
		p.Owned, err = owners(ctx)
		return p, err
	})
	if err != nil {
		t.Fatal(err)
	}
	z.WithFreshness(func() bool { _, ok := w.Current(); return ok })
	return &Policy{Service: z, s: s, w: w}
}

// Sync waits until the watcher has the fake's latest revision, then reloads, as the hub does on a stream message.
func (p *Policy) Sync(t testing.TB) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := p.w.WaitRevision(ctx, p.s.Revision()); err != nil {
		t.Fatalf("iamtest: revision %d: %v", p.s.Revision(), err)
	}
	if err := p.Reload(ctx); err != nil {
		t.Fatal(err)
	}
}

package authz

import (
	"context"
	"sync"
	"sync/atomic"

	"github.com/google/uuid"
)

// Service holds the current Enforcer and swaps it atomically on Reload.
type Service struct {
	load     func(context.Context) (Policy, error)
	mu       sync.Mutex
	cur      atomic.Pointer[Enforcer]
	onReload func(old, new Policy)
	fresh    atomic.Pointer[func() bool]
}

func NewService(ctx context.Context, load func(context.Context) (Policy, error)) (*Service, error) {
	s := &Service{load: load}
	if err := s.Reload(ctx); err != nil {
		return nil, err
	}
	return s, nil
}

// Reload rebuilds the enforcer from a fresh load; on any error the old one stays.
func (s *Service) Reload(ctx context.Context) error {
	// The lock spans load and swap so swaps happen in load order.
	s.mu.Lock()
	defer s.mu.Unlock()
	p, err := s.load(ctx)
	if err != nil {
		return err
	}
	e, err := New(p)
	if err != nil {
		return err
	}
	old := s.cur.Load()
	s.cur.Store(e)
	if s.onReload != nil {
		s.onReload(old.policy, p)
	}
	return nil
}

// OnReload calls fn with the previous and the new policy after every successful Reload, in reload order, and now with the current one as both; fn runs under the reload lock and must not block.
func (s *Service) OnReload(fn func(old, new Policy)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.onReload = fn
	cur := s.cur.Load().policy
	fn(cur, cur)
}

// WithFreshness makes Current report false while fresh does.
func (s *Service) WithFreshness(fresh func() bool) *Service {
	s.fresh.Store(&fresh)
	return s
}

func (s *Service) Enforcer() *Enforcer { return s.cur.Load() }

// Current makes the policy a platform authz source; it is loaded at construction, so only freshness can withhold it.
func (s *Service) Current() (*Enforcer, bool) {
	if fresh := s.fresh.Load(); fresh != nil && !(*fresh)() {
		return nil, false
	}
	return s.cur.Load(), true
}

func (s *Service) Allowed(sub string, org uuid.UUID, obj, act string) bool {
	return s.Enforcer().Allowed(sub, org, obj, act)
}

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
	onReload func(Policy)
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
	s.cur.Store(e)
	if s.onReload != nil {
		s.onReload(p)
	}
	return nil
}

// OnReload calls fn with the current policy now and after every successful Reload, in reload order; fn runs under the reload lock and must not block.
func (s *Service) OnReload(fn func(Policy)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.onReload = fn
	fn(s.cur.Load().policy)
}

func (s *Service) Enforcer() *Enforcer { return s.cur.Load() }

func (s *Service) Allowed(sub string, org uuid.UUID, obj, act string) bool {
	return s.Enforcer().Allowed(sub, org, obj, act)
}

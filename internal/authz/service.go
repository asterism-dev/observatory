package authz

import (
	"context"
	"sync"
	"sync/atomic"

	"github.com/google/uuid"
)

// Service holds the current Enforcer and swaps it atomically on Reload.
type Service struct {
	load func(context.Context) (Policy, error)
	mu   sync.Mutex
	cur  atomic.Pointer[Enforcer]
	hook func(context.Context, Policy)
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
	if s.hook != nil {
		s.hook(ctx, p)
	}
	return nil
}

// OnReload runs fn with every policy that becomes current, starting with the one loaded now; fn runs under the reload lock.
func (s *Service) OnReload(fn func(context.Context, Policy)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.hook = fn
	if e := s.cur.Load(); e != nil {
		fn(context.Background(), e.policy)
	}
}

func (s *Service) Enforcer() *Enforcer { return s.cur.Load() }

func (s *Service) Allowed(sub string, org uuid.UUID, obj, act string) bool {
	return s.Enforcer().Allowed(sub, org, obj, act)
}

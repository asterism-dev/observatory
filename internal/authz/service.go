package authz

import (
	"context"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
)

var (
	// committedReloadWait bounds each reload of ReloadCommitted, so a starved pool cannot stall the committing request.
	committedReloadWait = 5 * time.Second
	commitRetryWait     = time.Second
)

// Service holds the current Enforcer and swaps it atomically on Reload.
type Service struct {
	load func(context.Context) (Policy, error)
	// loads numbers loads by start; swapped (the load behind cur) and heldUntil (the last load started before a hold ended) change under mu; behindUntil is the latest load failed in ReloadCommitted.
	loads       atomic.Uint64
	mu          sync.Mutex
	heldUntil   uint64
	swapped     atomic.Uint64
	behindUntil atomic.Uint64
	retrying    atomic.Bool
	cur         atomic.Pointer[Enforcer]
	onReload    func(old, new Policy)
	fresh       atomic.Pointer[func() bool]
	// denyAll stands in for the policy while it is not fresh.
	denyAll *Enforcer
}

func NewService(ctx context.Context, load func(context.Context) (Policy, error)) (*Service, error) {
	denyAll, err := New(Policy{})
	if err != nil {
		return nil, err
	}
	s := &Service{load: load, denyAll: denyAll}
	if err := s.Reload(ctx); err != nil {
		return nil, err
	}
	return s, nil
}

// Reload rebuilds the enforcer from a fresh load; on any error the old one stays.
func (s *Service) Reload(ctx context.Context) error {
	_, err := s.reload(ctx)
	return err
}

// reload is Reload that also returns the number of its last load.
func (s *Service) reload(ctx context.Context) (uint64, error) {
	for {
		// The load runs outside mu: holders of mu sit in transactions, and a load waiting for their connections under mu would deadlock the pool.
		n := s.loads.Add(1)
		p, err := s.load(ctx)
		if err != nil {
			return n, err
		}
		e, err := New(p)
		if err != nil {
			return n, err
		}
		if s.swap(n, p, e) {
			return n, nil
		}
	}
}

// swap publishes load n unless a later load already did; it reports false when n started before a hold ended and may miss the held write.
func (s *Service) swap(n uint64, p Policy, e *Enforcer) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if n <= s.heldUntil {
		return false
	}
	if n < s.swapped.Load() {
		return true
	}
	s.swapped.Store(n)
	old := s.cur.Load()
	s.cur.Store(e)
	if s.onReload != nil {
		s.onReload(old.policy, p)
	}
	return true
}

// ReloadCommitted reloads after a committed write, bounded by committedReloadWait; on failure Behind reports true and the reload is retried in the background until a load that started later succeeds.
func (s *Service) ReloadCommitted(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), committedReloadWait)
	defer cancel()
	n, err := s.reload(ctx)
	if err == nil {
		return nil
	}
	for {
		cur := s.behindUntil.Load()
		if cur >= n || s.behindUntil.CompareAndSwap(cur, n) {
			break
		}
	}
	if s.retrying.CompareAndSwap(false, true) {
		go s.retryCommitted()
	}
	return err
}

// ponytail: the retry runs until it catches up, also past shutdown; give Service a lifetime if that matters.
func (s *Service) retryCommitted() {
	for wait := commitRetryWait; ; wait = min(2*wait, 30*time.Second) {
		if !s.Behind() {
			s.retrying.Store(false)
			// A failure between the check and the store found retrying set and left the retry to this loop.
			if !s.Behind() || !s.retrying.CompareAndSwap(false, true) {
				return
			}
			continue
		}
		time.Sleep(wait)
		ctx, cancel := context.WithTimeout(context.Background(), committedReloadWait)
		_, err := s.reload(ctx)
		cancel()
		if err != nil {
			slog.Error("policy reload after a commit still fails, the policy misses that commit", "err", err, "retry_in", min(2*wait, 30*time.Second))
		}
	}
}

// Behind reports whether a commit may be missing from the policy because its ReloadCommitted failed and no later load succeeded yet.
func (s *Service) Behind() bool { return s.swapped.Load() < s.behindUntil.Load() }

// OnReload calls fn with the previous and the new policy after every successful Reload, in reload order, and now with the current one as both; fn runs under the reload lock and must not block.
func (s *Service) OnReload(fn func(old, new Policy)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.onReload = fn
	cur := s.cur.Load().policy
	fn(cur, cur)
}

// HoldReloads blocks publishing until release is called; a load started before release loads again, so every later publish includes writes committed under the hold.
func (s *Service) HoldReloads() (release func()) {
	s.mu.Lock()
	return func() {
		s.heldUntil = s.loads.Load()
		s.mu.Unlock()
	}
}

// WithFreshness makes Current report false while fresh does.
func (s *Service) WithFreshness(fresh func() bool) *Service {
	s.fresh.Store(&fresh)
	return s
}

// Enforcer is the current policy, or one that allows nothing while the policy is not fresh; callers that can answer Unavailable check Current.
func (s *Service) Enforcer() *Enforcer {
	if !s.isFresh() {
		return s.denyAll
	}
	return s.cur.Load()
}

// LastKnown is the latest policy however stale; only the documented exemptions may use it: gateway allow lists, node pushes and the SSH-key re-pushes to nodes.
func (s *Service) LastKnown() *Enforcer { return s.cur.Load() }

// Current makes the policy a platform authz source; it is loaded at construction, so only freshness can withhold it.
func (s *Service) Current() (*Enforcer, bool) {
	if !s.isFresh() {
		return nil, false
	}
	return s.cur.Load(), true
}

func (s *Service) isFresh() bool {
	fresh := s.fresh.Load()
	return fresh == nil || (*fresh)()
}

func (s *Service) Allowed(sub string, org uuid.UUID, obj, act string) bool {
	return s.Enforcer().Allowed(sub, org, obj, act)
}

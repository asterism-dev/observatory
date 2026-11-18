package integrations

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"sync"
	"time"

	"github.com/google/uuid"
)

// SignIns holds pending PKCE sign-ins in memory; a hub restart means "start again".
type SignIns[T any] struct {
	ttl     time.Duration
	now     func() time.Time
	mu      sync.Mutex
	pending map[string]signIn[T]
}

type signIn[T any] struct {
	verifier  string
	user, org uuid.UUID
	value     T
	started   time.Time
}

func NewSignIns[T any](ttl time.Duration, now func() time.Time) *SignIns[T] {
	return &SignIns[T]{ttl: ttl, now: now, pending: map[string]signIn[T]{}}
}

func (s *SignIns[T]) Start(user, org uuid.UUID, v T) (state, challenge string, err error) {
	verifier, err := randomString()
	if err != nil {
		return "", "", err
	}
	if state, err = randomString(); err != nil {
		return "", "", err
	}
	sum := sha256.Sum256([]byte(verifier))
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	for k, e := range s.pending {
		if s.expired(e, now) {
			delete(s.pending, k)
		}
	}
	s.pending[state] = signIn[T]{verifier: verifier, user: user, org: org, value: v, started: now}
	return state, base64.RawURLEncoding.EncodeToString(sum[:]), nil
}

func (s *SignIns[T]) Take(state string, user, org uuid.UUID) (verifier string, v T, ok bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	e, found := s.pending[state]
	if !found {
		return "", v, false
	}
	if s.expired(e, s.now()) {
		delete(s.pending, state)
		return "", v, false
	}
	// Another user's state must not cancel the owner's sign-in.
	if e.user != user || e.org != org {
		return "", v, false
	}
	delete(s.pending, state)
	return e.verifier, e.value, true
}

func (s *SignIns[T]) expired(e signIn[T], now time.Time) bool { return now.Sub(e.started) > s.ttl }

func randomString() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

package authz

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"connectrpc.com/connect"
	"github.com/google/uuid"

	iamv1 "github.com/asterism-dev/observatory/gen/go/observatory/iam/v1"
	"github.com/asterism-dev/observatory/gen/go/observatory/iam/v1/iamv1connect"
	coreauthz "github.com/asterism-dev/observatory/internal/authz"
)

func TestPolicyPublishDoesNotBlock(t *testing.T) {
	ps := NewPublisher()
	// A subscriber that never drains must not stall Publish (which runs under authz's reload lock).
	ch := ps.subscribe()
	defer ps.unsubscribe(ch)
	done := make(chan struct{})
	go func() {
		for range 1000 {
			ps.Publish(coreauthz.Policy{Teams: []coreauthz.TeamRef{{OrgID: uuid.New(), ID: uuid.New()}}})
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Publish blocked on a slow subscriber")
	}
	if got := ps.latest(); got == nil || got.Revision == 0 {
		t.Fatalf("latest: %v", got)
	}
}

func TestPolicyPublishSkipsUnchanged(t *testing.T) {
	ps := NewPublisher()
	ps.Publish(coreauthz.Policy{})
	rev := ps.latest().Revision
	ps.Publish(coreauthz.Policy{})
	if got := ps.latest().Revision; got != rev {
		t.Fatalf("unchanged snapshot bumped revision %d → %d", rev, got)
	}
	ps.Publish(coreauthz.Policy{Teams: []coreauthz.TeamRef{{OrgID: uuid.New(), ID: uuid.New()}}})
	if got := ps.latest().Revision; got <= rev {
		t.Fatalf("changed snapshot kept revision %d", got)
	}
}

func TestPublisherSendsNoKeepaliveWhileBehind(t *testing.T) {
	old := keepaliveEvery
	keepaliveEvery = 50 * time.Millisecond
	t.Cleanup(func() { keepaliveEvery = old })
	var behind atomic.Bool
	ps := NewPublisher().WithBehind(behind.Load)
	ps.Publish(coreauthz.Policy{})
	path, h := ps.Handler("")
	mux := http.NewServeMux()
	mux.Handle(path, h)
	srv := httptest.NewServer(mux)
	defer srv.Close()
	stream, err := iamv1connect.NewPolicyServiceClient(srv.Client(), srv.URL).WatchPolicy(t.Context(), connect.NewRequest(&iamv1.WatchPolicyRequest{}))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = stream.Close() }()
	got := make(chan uint64, 16)
	go func() {
		for stream.Receive() {
			got <- stream.Msg().Revision
		}
		close(got)
	}()
	next := func(within time.Duration) (uint64, bool) {
		select {
		case rev, ok := <-got:
			return rev, ok
		case <-time.After(within):
			return 0, false
		}
	}
	rev := ps.Revision()
	if r, ok := next(5 * time.Second); !ok || r != rev {
		t.Fatalf("first snapshot: %d %v", r, ok)
	}
	if r, ok := next(5 * time.Second); !ok || r != rev {
		t.Fatalf("keepalive: %d %v", r, ok)
	}
	behind.Store(true)
	// One keepalive may already be on its way.
	next(keepaliveEvery)
	if r, ok := next(4 * keepaliveEvery); ok {
		t.Fatalf("keepalive %d sent while behind: watchers would trust a policy missing a commit", r)
	}
	behind.Store(false)
	ps.Publish(coreauthz.Policy{Teams: []coreauthz.TeamRef{{OrgID: uuid.New(), ID: uuid.New()}}})
	if r, ok := next(5 * time.Second); !ok || r != ps.Revision() || r <= rev {
		t.Fatalf("caught-up revision: %d %v", r, ok)
	}
}

// A watcher that reconnects after silence must not take a behind publisher's first snapshot as liveness.
func TestReconnectingWatcherGoesStaleWhilePublisherBehind(t *testing.T) {
	oldKeep, oldSilent := keepaliveEvery, silentAfter
	keepaliveEvery, silentAfter = 30*time.Millisecond, 150*time.Millisecond
	t.Cleanup(func() { keepaliveEvery, silentAfter = oldKeep, oldSilent })
	var behind atomic.Bool
	ps := NewPublisher().WithBehind(behind.Load)
	ps.Publish(coreauthz.Policy{})
	path, h := ps.Handler("/internal/iam")
	var conns atomic.Int32
	mux := http.NewServeMux()
	mux.Handle(path, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conns.Add(1)
		h.ServeHTTP(w, r)
	}))
	srv := httptest.NewServer(mux)
	defer srv.Close()
	clock := &fakeClock{}
	clock.ns.Store(time.Now().UnixNano())
	w, err := NewWatcher(srv.URL, testToken, srv.Client())
	if err != nil {
		t.Fatal(err)
	}
	w.WithClock(clock.now)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { w.Run(ctx); close(done) }()
	defer func() { cancel(); <-done }()
	waitApplied(t, w, ps.Revision())

	behind.Store(true)
	clock.add(silentAfter + maxStale + time.Second)
	// Two reconnects: each new stream would have refreshed liveness with its first snapshot.
	for start := conns.Load(); conns.Load() < start+2; {
		if _, ok := w.Current(); ok {
			t.Fatal("current while the publisher is behind")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if _, ok := w.Current(); ok {
		t.Fatal("current after reconnecting to a behind publisher")
	}

	behind.Store(false)
	ps.Publish(coreauthz.Policy{Teams: []coreauthz.TeamRef{{OrgID: uuid.New(), ID: uuid.New()}}})
	waitApplied(t, w, ps.Revision())
	if _, ok := w.Current(); !ok {
		t.Fatal("not current after the publisher caught up")
	}
}

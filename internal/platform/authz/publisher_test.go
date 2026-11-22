package authz

import (
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
	defer stream.Close()
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

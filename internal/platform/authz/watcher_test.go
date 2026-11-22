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
	"github.com/asterism-dev/observatory/internal/platform/catalog"
)

type fakeClock struct{ ns atomic.Int64 }

func (c *fakeClock) now() time.Time      { return time.Unix(0, c.ns.Load()) }
func (c *fakeClock) add(d time.Duration) { c.ns.Add(int64(d)) }

func TestWatcherStaleAfterMaxStale(t *testing.T) {
	fp := &fakePolicy{conns: make(chan chan *iamv1.Policy)}
	path, h := iamv1connect.NewPolicyServiceHandler(fp)
	mux := http.NewServeMux()
	mux.Handle("/internal/iam"+path, http.StripPrefix("/internal/iam", h))
	srv := httptest.NewServer(mux)
	defer srv.Close()
	w, err := NewWatcher(srv.URL, testToken, srv.Client())
	if err != nil {
		t.Fatal(err)
	}
	clock := &fakeClock{}
	clock.ns.Store(time.Now().UnixNano())
	w.now = clock.now
	var changes atomic.Int32
	w.OnChange(func() { changes.Add(1) })
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { w.Run(ctx); close(done) }()
	defer func() { cancel(); <-done }()

	conn := func() chan *iamv1.Policy {
		t.Helper()
		select {
		case ch := <-fp.conns:
			return ch
		case <-time.After(10 * time.Second):
			t.Fatal("watcher did not connect")
			return nil
		}
	}
	waitRev := func(rev uint64) {
		t.Helper()
		wctx, wcancel := context.WithTimeout(ctx, 5*time.Second)
		defer wcancel()
		if err := w.WaitRevision(wctx, rev); err != nil {
			t.Fatalf("revision %d: %v", rev, err)
		}
	}
	snapshot := func(rev uint64) *iamv1.Policy {
		return &iamv1.Policy{Revision: rev, Memberships: []*iamv1.MemberRole{{OrgId: uuid.NewString(), UserId: uuid.NewString(), Role: "member"}}}
	}

	if _, ok := w.Current(); ok {
		t.Fatal("current before the first snapshot")
	}
	ch := conn()
	ch <- snapshot(7)
	waitRev(7)
	if p, rev, ok := w.Policy(); !ok || rev != 7 || len(p.Memberships) != 1 {
		t.Fatalf("policy: %v %d %v", p, rev, ok)
	}
	if _, ok := w.Current(); !ok {
		t.Fatal("not current with a live stream")
	}
	if n := changes.Load(); n != 1 {
		t.Fatalf("OnChange ran %d times, want 1", n)
	}

	close(ch)
	deadline := time.Now().Add(5 * time.Second)
	for w.downSince.Load() == 0 {
		if time.Now().After(deadline) {
			t.Fatal("stream loss not noticed")
		}
		time.Sleep(10 * time.Millisecond)
	}
	clock.add(maxStale)
	if _, ok := w.Current(); !ok {
		t.Fatal("stale at exactly maxStale")
	}
	clock.add(time.Second)
	if _, ok := w.Current(); ok {
		t.Fatal("still current after maxStale without a stream")
	}
	if _, rev, ok := w.Policy(); !ok || rev != 7 {
		t.Fatalf("policy dropped while stale: %d %v", rev, ok)
	}

	ch = conn()
	ch <- snapshot(8)
	waitRev(8)
	if _, ok := w.Current(); !ok {
		t.Fatal("not current after the stream came back")
	}
}

// startWatcher runs a watcher on a fake clock against fp and returns it with the first stream's feed.
func startWatcher(t *testing.T, fp *fakePolicy) (*Watcher, *fakeClock, chan *iamv1.Policy) {
	t.Helper()
	path, h := iamv1connect.NewPolicyServiceHandler(fp)
	mux := http.NewServeMux()
	mux.Handle("/internal/iam"+path, http.StripPrefix("/internal/iam", h))
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
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
	t.Cleanup(func() { cancel(); <-done })
	select {
	case ch := <-fp.conns:
		return w, clock, ch
	case <-time.After(10 * time.Second):
		t.Fatal("watcher did not connect")
		return nil, nil, nil
	}
}

func waitApplied(t *testing.T, w *Watcher, rev uint64) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := w.WaitRevision(ctx, rev); err != nil {
		t.Fatalf("revision %d: %v", rev, err)
	}
}

func TestBackoffCapsQuickly(t *testing.T) {
	d := minBackoff
	var got []time.Duration
	for range 5 {
		got = append(got, d)
		d = nextBackoff(d)
	}
	want := []time.Duration{time.Second, 2 * time.Second, 4 * time.Second, 5 * time.Second, 5 * time.Second}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("sequence %v, want %v", got, want)
		}
	}
}

func TestWatcherSilentStreamGoesStale(t *testing.T) {
	w, clock, ch := startWatcher(t, &fakePolicy{conns: make(chan chan *iamv1.Policy)})
	ch <- &iamv1.Policy{Revision: 3}
	waitApplied(t, w, 3)
	// The stream stays open but silent, as a half-open connection would.
	clock.add(silentAfter + maxStale)
	if _, ok := w.Current(); !ok {
		t.Fatal("stale at exactly silentAfter + maxStale")
	}
	clock.add(time.Second)
	if _, ok := w.Current(); ok {
		t.Fatal("still current on a stream silent past silentAfter + maxStale")
	}
	// A keepalive repeats the current revision and makes the stream live again.
	ch <- &iamv1.Policy{Revision: 3}
	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, ok := w.Current(); ok {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("a keepalive did not refresh the stream")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestWatcherRejectedSnapshotIsNoLiveness(t *testing.T) {
	w, clock, ch := startWatcher(t, &fakePolicy{conns: make(chan chan *iamv1.Policy)})
	var changes atomic.Int32
	w.OnChange(func() { changes.Add(1) })
	ch <- &iamv1.Policy{Revision: 3}
	waitApplied(t, w, 3)
	bad := &iamv1.Policy{Revision: 4, Memberships: []*iamv1.MemberRole{{OrgId: "not-a-uuid", UserId: uuid.NewString(), Role: "member"}}}
	ch <- bad
	deadline := time.Now().Add(5 * time.Second)
	for w.downSince.Load() == 0 {
		if time.Now().After(deadline) {
			t.Fatal("a rejected snapshot did not count as blind")
		}
		time.Sleep(10 * time.Millisecond)
	}
	// Its keepalives repeat the rejected revision and must not refresh liveness either.
	ch <- bad
	clock.add(maxStale + time.Second)
	if _, ok := w.Current(); ok {
		t.Fatal("current after maxStale of rejected snapshots")
	}
	if _, rev, _ := w.Policy(); rev != 3 || changes.Load() != 1 {
		t.Fatalf("revision %d after %d changes, want 3 after 1", rev, changes.Load())
	}
}

func testCatalog(service, action string) coreauthz.Catalog {
	return coreauthz.Catalog{Service: service, Actions: []coreauthz.Action{{Name: action, Object: "org/echoes/*"}}}
}

func withCatalogs(p *iamv1.Policy, cs ...coreauthz.Catalog) *iamv1.Policy {
	for _, c := range cs {
		p.Catalogs = append(p.Catalogs, catalog.ToProto(c))
	}
	return p
}

func TestInterceptorUnavailableUntilCatalog(t *testing.T) {
	f := newFixture(t)
	cat := testCatalog("test", "test.echoes.get")
	f.w.Require("test", catalog.Digest(cat))
	ch := f.connect(t)
	ch <- f.snapshot(1, f.member)
	waitApplied(t, f.w, 1)
	echo := uuid.NewString()
	wantCode(t, f.getEcho(t, f.member, f.org.String(), echo), connect.CodeUnavailable)
	ch <- withCatalogs(f.snapshot(2, f.member), cat)
	eventually(t, 0, func() error { return f.getEcho(t, f.member, f.org.String(), echo) })
	if p, _, _ := f.w.Policy(); len(p.Catalogs) != 1 || catalog.Digest(p.Catalogs[0]) != catalog.Digest(cat) {
		t.Fatalf("policy catalogs: %+v", p.Catalogs)
	}
}

func TestRequireIgnoresOtherServices(t *testing.T) {
	w, clock, ch := startWatcher(t, &fakePolicy{conns: make(chan chan *iamv1.Policy)})
	audit, hub := testCatalog("audit", "audit.events.list"), testCatalog("hub", "compute.instances.get")
	hub2 := testCatalog("hub", "compute.instances.list")
	w.Require("audit", catalog.Digest(audit))
	current := func() bool { _, ok := w.Current(); return ok }

	ch <- withCatalogs(&iamv1.Policy{Revision: 1}, hub)
	waitApplied(t, w, 1)
	if current() {
		t.Fatal("current without the required audit digest")
	}
	ch <- withCatalogs(&iamv1.Policy{Revision: 2}, hub, audit)
	waitApplied(t, w, 2)
	if !current() {
		t.Fatal("not current with the required digest")
	}
	ch <- withCatalogs(&iamv1.Policy{Revision: 3}, hub2, audit)
	waitApplied(t, w, 3)
	if !current() {
		t.Fatal("another service's catalog change made the watcher not current")
	}
	// A required digest does not lift staleness.
	clock.add(silentAfter + maxStale + time.Second)
	if current() {
		t.Fatal("current on a stale stream")
	}
	ch <- withCatalogs(&iamv1.Policy{Revision: 3}, hub2, audit)
	waitUntilCurrent(t, w)

	w.Require("audit", catalog.Digest(testCatalog("audit", "audit.events.get")))
	if current() {
		t.Fatal("current with a stale audit digest")
	}
}

func waitUntilCurrent(t *testing.T, w *Watcher) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, ok := w.Current(); ok {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("watcher did not become current")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

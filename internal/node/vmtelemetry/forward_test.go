package vmtelemetry

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/rand"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
)

type hubReq struct {
	path, vm, stream, auth, ctype, cenc string
	body                                []byte
}

// fakeHub records requests; status answers each one, 200 when nil.
type fakeHub struct {
	*httptest.Server
	mu     sync.Mutex
	reqs   []hubReq
	status func(n int) int
}

func newFakeHub(t *testing.T, status func(n int) int) *fakeHub {
	h := &fakeHub{status: status}
	h.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		zr, err := gzip.NewReader(r.Body)
		if err != nil {
			t.Errorf("hub body not gzip: %v", err)
			return
		}
		body, _ := io.ReadAll(zr)
		h.mu.Lock()
		h.reqs = append(h.reqs, hubReq{r.URL.Path, r.Header.Get("X-Observatory-VM"), r.Header.Get("X-Observatory-Stream"),
			r.Header.Get("Authorization"), r.Header.Get("Content-Type"), r.Header.Get("Content-Encoding"), body})
		n := len(h.reqs)
		h.mu.Unlock()
		if h.status != nil {
			w.Header().Set("Location", "/elsewhere")
			w.WriteHeader(h.status(n))
		}
	}))
	t.Cleanup(h.Close)
	return h
}

func (h *fakeHub) requests() []hubReq {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]hubReq(nil), h.reqs...)
}

func fastRetry(t *testing.T) {
	old := retryMin
	retryMin = time.Millisecond
	t.Cleanup(func() { retryMin = old })
}

func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(5 * time.Millisecond) {
		if cond() {
			return
		}
	}
	t.Fatalf("timed out waiting for %s", what)
}

func newTestForwarder(t *testing.T, base string) *Forwarder {
	f := NewForwarder(base, "cred", http.DefaultClient)
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
		defer cancel()
		f.Close(ctx)
	})
	return f
}

func queued(f *Forwarder, vm uuid.UUID) [][]byte {
	f.mu.Lock()
	defer f.mu.Unlock()
	q := f.queues[vm]
	if q == nil {
		return nil
	}
	var out [][]byte
	for _, it := range q.items {
		zr, _ := gzip.NewReader(bytes.NewReader(it.body))
		b, _ := io.ReadAll(zr)
		out = append(out, b)
	}
	return out
}

func TestVMForwarderRetryClass(t *testing.T) {
	fastRetry(t)
	vm := uuid.New()
	retried := newFakeHub(t, func(n int) int {
		if n == 1 {
			return http.StatusServiceUnavailable
		}
		return http.StatusOK
	})
	newTestForwarder(t, retried.URL).Send(vm, "logs", "app", []byte("a"), 1)
	eventually(t, "retry after 503", func() bool { return len(retried.requests()) == 2 })
	got := retried.requests()[1]
	if got.path != "/otlp/v1/logs" || string(got.body) != "a" {
		t.Fatalf("retried request = %+v", got)
	}

	for _, code := range []int{http.StatusNotFound, http.StatusForbidden, http.StatusTemporaryRedirect} {
		rejected := newFakeHub(t, func(int) int { return code })
		f := newTestForwarder(t, rejected.URL)
		f.Send(vm, "metrics", "usage", []byte("x"), 0)
		f.Send(vm, "metrics", "usage", []byte("y"), 0)
		eventually(t, "both sent", func() bool { return len(rejected.requests()) == 2 })
		time.Sleep(50 * time.Millisecond)
		if reqs := rejected.requests(); len(reqs) != 2 || string(reqs[0].body) != "x" || string(reqs[1].body) != "y" {
			t.Fatalf("%d retried: %+v", code, reqs)
		}
	}
}

func TestVMForwarderBounded(t *testing.T) {
	fastRetry(t)
	down := httptest.NewServer(http.NotFoundHandler())
	down.Close()
	f := newTestForwarder(t, down.URL)
	a, b := uuid.New(), uuid.New()
	f.Send(b, "logs", "app", []byte("b"), 1)
	start := time.Now()
	for i := range 200 {
		f.Send(a, "logs", "app", fmt.Appendf(nil, "%d", i), 1)
	}
	if d := time.Since(start); d > time.Second {
		t.Fatalf("200 sends took %v", d)
	}
	got := queued(f, a)
	if len(got) != maxQueueBodies || string(got[0]) != "136" || string(got[63]) != "199" {
		t.Fatalf("queue of a: %d bodies, first %q", len(got), got[0])
	}
	if got := queued(f, b); len(got) != 1 || string(got[0]) != "b" {
		t.Fatalf("queue of b = %q", got)
	}

	c := uuid.New()
	for range 6 {
		big := make([]byte, 1<<20)
		rand.Read(big)
		f.Send(c, "logs", "app", big, 1)
	}
	f.mu.Lock()
	size, n := f.queues[c].size, len(f.queues[c].items)
	f.mu.Unlock()
	if size > maxQueueBytes || n >= 6 {
		t.Fatalf("queue of c: %d bodies, %d bytes", n, size)
	}
}

func TestVMForwarderForget(t *testing.T) {
	old := retryMin
	retryMin = 10 * time.Second
	t.Cleanup(func() { retryMin = old })
	down := httptest.NewServer(http.NotFoundHandler())
	down.Close()
	f := newTestForwarder(t, down.URL)
	vm := uuid.New()
	f.Send(vm, "logs", "app", []byte("a"), 1)
	time.Sleep(50 * time.Millisecond) // the sender's first post has failed and it waits to retry
	f.Forget(vm)
	if got := queued(f, vm); got != nil {
		t.Fatalf("queue after Forget: %q", got)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	done := make(chan struct{})
	go func() { f.wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-ctx.Done():
		t.Fatal("sender still running after Forget")
	}
}

func TestVMForwarderCountsEvictions(t *testing.T) {
	fastRetry(t)
	down := httptest.NewServer(http.NotFoundHandler())
	down.Close()
	f := newTestForwarder(t, down.URL)
	d := &drops{n: map[string]int64{}}
	NewSink(f, d.add)
	vm := uuid.New()
	f.Send(vm, "metrics", "app", []byte("m"), 0)
	for range maxQueueBodies {
		f.Send(vm, "logs", "console", []byte("l"), 3)
	}
	if d.get("body") != 1 || d.get("logs") != 0 {
		t.Fatalf("metrics body evicted: drops %v", d.n)
	}
	f.Send(vm, "logs", "console", []byte("l"), 3)
	if d.get("logs") != 3 {
		t.Fatalf("logs body evicted: drops %v", d.n)
	}
}

// Package vmtelemetry receives telemetry from VM guests over vsock, enforces per-VM limits and forwards it to the hub.
package vmtelemetry

import (
	"bytes"
	"compress/gzip"
	"context"
	"io"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/google/uuid"
)

const (
	maxQueueBodies = 64
	maxQueueBytes  = 4 << 20
	sendTimeout    = 10 * time.Second
	retryMax       = 30 * time.Second
)

var retryMin = time.Second

type item struct {
	seq            uint64
	signal, stream string
	body           []byte // gzip
}

type queue struct {
	items  []item
	size   int
	ctx    context.Context
	cancel context.CancelFunc
}

// Forwarder sends VM telemetry to the hub with one queue and sender per VM, so one VM's backlog never evicts another's.
type Forwarder struct {
	base, credential string
	hc               *http.Client
	ctx              context.Context
	cancel           context.CancelFunc
	wg               sync.WaitGroup

	mu     sync.Mutex
	seq    uint64
	queues map[uuid.UUID]*queue // a VM has a queue while its sender runs
	closed bool
}

func NewForwarder(hubBase, credential string, hc *http.Client) *Forwarder {
	ctx, cancel := context.WithCancel(context.Background())
	c := *hc
	// A redirect would resend the node credential to wherever the response points.
	c.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return &Forwarder{base: hubBase, credential: credential, hc: &c, ctx: ctx, cancel: cancel, queues: map[uuid.UUID]*queue{}}
}

// Send queues an OTLP protobuf body of signal ("logs" or "metrics") for the hub, dropping vmID's oldest bodies past the bound.
func (f *Forwarder) Send(vmID uuid.UUID, signal, stream string, body []byte) {
	var b bytes.Buffer
	zw := gzip.NewWriter(&b)
	zw.Write(body)
	zw.Close()
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.closed {
		return
	}
	q := f.queues[vmID]
	if q == nil {
		ctx, cancel := context.WithCancel(f.ctx)
		q = &queue{ctx: ctx, cancel: cancel}
		f.queues[vmID] = q
		f.wg.Add(1)
		go f.run(vmID, q)
	}
	f.seq++
	q.items = append(q.items, item{f.seq, signal, stream, b.Bytes()})
	q.size += b.Len()
	for len(q.items) > maxQueueBodies || q.size > maxQueueBytes {
		q.size -= len(q.items[0].body)
		q.items[0] = item{}
		q.items = q.items[1:]
	}
}

// run sends q's head until q is empty; the head stays queued while it is retried, so Send may still evict it.
func (f *Forwarder) run(vmID uuid.UUID, q *queue) {
	defer f.wg.Done()
	var delay time.Duration
	for {
		f.mu.Lock()
		if len(q.items) == 0 {
			if f.queues[vmID] == q {
				delete(f.queues, vmID)
			}
			f.mu.Unlock()
			q.cancel()
			return
		}
		it := q.items[0]
		f.mu.Unlock()
		if f.post(q.ctx, vmID, it) {
			delay = min(max(2*delay, retryMin), retryMax)
			select {
			case <-q.ctx.Done():
			case <-time.After(delay):
			}
			continue
		}
		delay = 0
		f.mu.Lock()
		if len(q.items) > 0 && q.items[0].seq == it.seq {
			q.size -= len(it.body)
			q.items[0] = item{}
			q.items = q.items[1:]
		}
		f.mu.Unlock()
	}
}

// post reports whether it should be retried: no response, 429 and 5xx are; a 403 means the VM left this node.
func (f *Forwarder) post(qctx context.Context, vmID uuid.UUID, it item) bool {
	ctx, cancel := context.WithTimeout(qctx, sendTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, f.base+"/otlp/v1/"+it.signal, bytes.NewReader(it.body))
	if err != nil {
		slog.Warn("vm telemetry request", "vm", vmID, "err", err)
		return false
	}
	req.Header.Set("Authorization", "Bearer "+f.credential)
	req.Header.Set("Content-Type", "application/x-protobuf")
	req.Header.Set("Content-Encoding", "gzip")
	req.Header.Set("X-Observatory-VM", vmID.String())
	req.Header.Set("X-Observatory-Stream", it.stream)
	resp, err := f.hc.Do(req)
	if err != nil {
		return qctx.Err() == nil
	}
	io.Copy(io.Discard, io.LimitReader(resp.Body, 4<<10))
	resp.Body.Close()
	switch code := resp.StatusCode; {
	case code == http.StatusTooManyRequests || code >= 500:
		return true
	case code >= 300:
		slog.Warn("vm telemetry rejected", "vm", vmID, "stream", it.stream, "status", code)
	}
	return false
}

// Forget drops vmID's queue and ends its sender, which stops retrying at once.
func (f *Forwarder) Forget(vmID uuid.UUID) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if q := f.queues[vmID]; q != nil {
		q.items, q.size = nil, 0
		delete(f.queues, vmID)
		q.cancel()
	}
}

// Close stops taking bodies and delivers the queued ones until ctx ends, then drops the rest.
func (f *Forwarder) Close(ctx context.Context) {
	f.mu.Lock()
	f.closed = true
	f.mu.Unlock()
	done := make(chan struct{})
	go func() { f.wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-ctx.Done():
	}
	f.cancel()
	<-done
}

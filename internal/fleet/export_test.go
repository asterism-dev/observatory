package fleet

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"time"

	"github.com/coder/websocket"
	"github.com/google/uuid"

	"github.com/asterism-dev/observatory/internal/proto"
)

var (
	DesiredRetry     = &desiredRetry
	IntegrationRetry = &integrationRetry
	ImageRetryMin    = &imageRetryMin
	Clock            = &clock
)

func SetUpdateTimeouts(f *Fleet, back, vms, poll time.Duration) {
	f.updateTimeouts.back, f.updateTimeouts.vms, f.updateTimeouts.poll = back, vms, poll
}

func SetBetweenVMReads(f *Fleet, hook func()) { f.betweenVMReads = hook }

func Seen(f *Fleet, node, vm uuid.UUID) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	c := f.conns[node]
	return c != nil && c.seen[vm.String()]
}

func SetVMErrorGrace(f *Fleet, d time.Duration) { f.updateTimeouts.vmErrGrace = d }

// AttachDeadConn stands in for a connection whose handler has ended but which is not detached yet.
func AttachDeadConn(f *Fleet, node, org uuid.UUID) {
	done := make(chan struct{})
	close(done)
	f.attach(&conn{node: node, org: org, update: make(chan struct{}), done: done, seen: map[string]bool{},
		requests: make(chan proto.Message), pending: map[string]chan proto.Message{}})
}

func UpdateErr(f *Fleet, node uuid.UUID) (string, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	s, ok := f.updateErrs[node]
	return s, ok
}

// AttachStalledConn stands in for a live connection whose push loop never takes the update request; end closes it.
func AttachStalledConn(f *Fleet, node, org uuid.UUID) (end func()) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if ws, err := websocket.Accept(w, r, nil); err == nil {
			ws.Reader(context.Background())
		}
	}))
	ws, _, err := websocket.Dial(context.Background(), "ws"+strings.TrimPrefix(srv.URL, "http"), nil)
	if err != nil {
		panic(err)
	}
	done := make(chan struct{})
	f.attach(&conn{node: node, org: org, ws: ws, update: make(chan struct{}), done: done, seen: map[string]bool{},
		requests: make(chan proto.Message), pending: map[string]chan proto.Message{}})
	return func() { close(done) }
}

func PendingRequests(f *Fleet, node uuid.UUID) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	if c := f.conns[node]; c != nil {
		return len(c.pending)
	}
	return 0
}

func SetPing(f *Fleet, every, timeout time.Duration) { f.ping.every, f.ping.timeout = every, timeout }

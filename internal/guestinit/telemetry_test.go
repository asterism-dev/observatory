package guestinit

import (
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	collogspb "go.opentelemetry.io/proto/otlp/collector/logs/v1"
	logspb "go.opentelemetry.io/proto/otlp/logs/v1"
	gproto "google.golang.org/protobuf/proto"
)

type nodeRequest struct {
	path, stream, encoding, contentType string
	body                                []byte
}

type fakeNode struct {
	mu   sync.Mutex
	reqs []nodeRequest
}

func (n *fakeNode) requests() []nodeRequest {
	n.mu.Lock()
	defer n.mu.Unlock()
	return append([]nodeRequest(nil), n.reqs...)
}

// startFakeNode serves the node's sink protocol on a unix socket; the returned dialer fails while down is set.
func startFakeNode(t *testing.T, down *atomic.Bool) (func(context.Context) (net.Conn, error), *fakeNode) {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "gi")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	sock := filepath.Join(dir, "t.sock")
	l, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	node := &fakeNode{}
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		zr, err := gzip.NewReader(r.Body)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		body, _ := io.ReadAll(zr)
		node.mu.Lock()
		node.reqs = append(node.reqs, nodeRequest{r.URL.Path, r.Header.Get("X-Observatory-Stream"), r.Header.Get("Content-Encoding"), r.Header.Get("Content-Type"), body})
		node.mu.Unlock()
	}))
	srv.Listener = l
	srv.Start()
	t.Cleanup(srv.Close)
	return func(ctx context.Context) (net.Conn, error) {
		if down != nil && down.Load() {
			return nil, errors.New("node down")
		}
		return (&net.Dialer{}).DialContext(ctx, "unix", sock)
	}, node
}

func decodeLogs(t *testing.T, body []byte) []*logspb.LogRecord {
	t.Helper()
	var req collogspb.ExportLogsServiceRequest
	if err := gproto.Unmarshal(body, &req); err != nil {
		t.Fatal(err)
	}
	var out []*logspb.LogRecord
	for _, rl := range req.ResourceLogs {
		for _, sl := range rl.ScopeLogs {
			out = append(out, sl.LogRecords...)
		}
	}
	return out
}

func TestSinkPostsWithStream(t *testing.T) {
	dial, node := startFakeNode(t, nil)
	s := newSink(dial)
	s.Logs("service", []*logspb.LogRecord{logRecord(time.Now(), "hello", infoLevel)})
	waitFor(t, func() bool { return len(node.requests()) == 1 })
	r := node.requests()[0]
	if r.path != "/v1/logs" || r.stream != "service" || r.encoding != "gzip" || r.contentType != "application/x-protobuf" {
		t.Fatalf("request = %+v", r)
	}
	recs := decodeLogs(t, r.body)
	if len(recs) != 1 || recs[0].GetBody().GetStringValue() != "hello" {
		t.Fatalf("records = %v", recs)
	}
	if !s.Connected() {
		t.Fatal("not connected after a post")
	}
}

func TestSinkBufferBounded(t *testing.T) {
	defer func(d time.Duration) { sinkBackoff = d }(sinkBackoff)
	sinkBackoff = 50 * time.Millisecond
	var down atomic.Bool
	down.Store(true)
	dial, node := startFakeNode(t, &down)
	s := newSink(dial)
	start := time.Now()
	for i := range 1000 {
		s.Logs("app", []*logspb.LogRecord{logRecord(time.Now(), strings.Repeat("x", i%7)+"|"+strconv.Itoa(i), infoLevel)})
	}
	if d := time.Since(start); d > 500*time.Millisecond {
		t.Fatalf("1000 Logs calls took %v", d)
	}
	down.Store(false)
	var got []string
	waitFor(t, func() bool {
		got = got[:0]
		for _, r := range node.requests() {
			for _, rec := range decodeLogs(t, r.body) {
				_, n, _ := strings.Cut(rec.GetBody().GetStringValue(), "|")
				got = append(got, n)
			}
		}
		return len(got) >= maxSinkQueue
	})
	time.Sleep(100 * time.Millisecond)
	if n := len(node.requests()); n != maxSinkQueue {
		t.Fatalf("%d requests reached the node, want %d", n, maxSinkQueue)
	}
	for i, n := range got {
		if want := strconv.Itoa(1000 - maxSinkQueue + i); n != want {
			t.Fatalf("record %d = %s, want %s", i, n, want)
		}
	}
}

func TestLogfSwitchesToSystem(t *testing.T) {
	var errOut bytes.Buffer
	defer func(w io.Writer, s *Sink) { stderr, telemetry = w, s }(stderr, telemetry)
	stderr = &errOut
	dial, node := startFakeNode(t, nil)
	telemetry = newSink(dial)

	logf("init: early %d", 1)
	if errOut.String() != "init: early 1\n" {
		t.Fatalf("stderr = %q", errOut.String())
	}
	telemetry.Logs("service", []*logspb.LogRecord{logRecord(time.Now(), "x", infoLevel)})
	waitFor(t, telemetry.Connected)
	errOut.Reset()

	logf("init: late %s", "x")
	waitFor(t, func() bool { return len(node.requests()) == 2 })
	if errOut.Len() != 0 {
		t.Fatalf("stderr after connect = %q", errOut.String())
	}
	r := node.requests()[1]
	if r.stream != "system" {
		t.Fatalf("stream = %q", r.stream)
	}
	var req collogspb.ExportLogsServiceRequest
	if err := gproto.Unmarshal(r.body, &req); err != nil {
		t.Fatal(err)
	}
	rl := req.ResourceLogs[0]
	if attrs := rl.GetResource().GetAttributes(); len(attrs) != 1 || attrs[0].Key != "service.name" || attrs[0].Value.GetStringValue() != "observatory-init" {
		t.Fatalf("resource = %v", attrs)
	}
	if body := rl.ScopeLogs[0].LogRecords[0].GetBody().GetStringValue(); body != "init: late x" {
		t.Fatalf("body = %q", body)
	}
}

func TestConsolefAlsoWritesStderrWhenConnected(t *testing.T) {
	var errOut bytes.Buffer
	defer func(w io.Writer, s *Sink) { stderr, telemetry = w, s }(stderr, telemetry)
	stderr = &errOut
	dial, node := startFakeNode(t, nil)
	telemetry = newSink(dial)
	telemetry.Logs("service", []*logspb.LogRecord{logRecord(time.Now(), "x", infoLevel)})
	waitFor(t, telemetry.Connected)

	consolef("init: reboot: %v", "failed")
	if errOut.String() != "init: reboot: failed\n" {
		t.Fatalf("stderr = %q", errOut.String())
	}
	waitFor(t, func() bool { return len(node.requests()) == 2 })
	if r := node.requests()[1]; r.stream != "system" || decodeLogs(t, r.body)[0].GetBody().GetStringValue() != "init: reboot: failed" {
		t.Fatalf("request = %+v", r)
	}
}

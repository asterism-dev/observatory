package vmtelemetry

import (
	"bytes"
	"compress/gzip"
	"context"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"sync"
	"testing"

	"github.com/google/uuid"
	collogspb "go.opentelemetry.io/proto/otlp/collector/logs/v1"
	colmetricspb "go.opentelemetry.io/proto/otlp/collector/metrics/v1"
	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	logspb "go.opentelemetry.io/proto/otlp/logs/v1"
	metricspb "go.opentelemetry.io/proto/otlp/metrics/v1"
	gproto "google.golang.org/protobuf/proto"
)

type drops struct {
	mu sync.Mutex
	n  map[string]int64
}

func (d *drops) add(_ uuid.UUID, kind string, n int64) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.n[kind] += n
}

func (d *drops) get(kind string) int64 {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.n[kind]
}

type testEnv struct {
	hub   *fakeHub
	sink  *Sink
	drops *drops
}

func newTestEnv(t *testing.T) *testEnv {
	fastRetry(t)
	hub := newFakeHub(t, nil)
	d := &drops{n: map[string]int64{}}
	return &testEnv{hub, NewSink(newTestForwarder(t, hub.URL), d.add), d}
}

// serve returns a client of vm's sink over a unix socket, as Firecracker hands the node guest connections.
func (e *testEnv) serve(t *testing.T, vm uuid.UUID) *http.Client {
	dir, err := os.MkdirTemp("", "vmt")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	sock := filepath.Join(dir, "s")
	l, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() { e.sink.Serve(l, vm); close(done) }()
	t.Cleanup(func() { l.Close(); <-done })
	return &http.Client{Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", sock)
	}}}
}

func post(t *testing.T, c *http.Client, path, stream string, body []byte, gz bool) int {
	t.Helper()
	if gz {
		var b bytes.Buffer
		zw := gzip.NewWriter(&b)
		zw.Write(body)
		zw.Close()
		body = b.Bytes()
	}
	req, _ := http.NewRequest(http.MethodPost, "http://vsock"+path, bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/x-protobuf")
	req.Header.Set("X-Observatory-Stream", stream)
	if gz {
		req.Header.Set("Content-Encoding", "gzip")
	}
	resp, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	return resp.StatusCode
}

func logsBody(t *testing.T, n int) []byte {
	recs := make([]*logspb.LogRecord, n)
	for i := range recs {
		recs[i] = &logspb.LogRecord{Body: &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: fmt.Sprint(i)}}}
	}
	return marshal(t, &collogspb.ExportLogsServiceRequest{ResourceLogs: []*logspb.ResourceLogs{{ScopeLogs: []*logspb.ScopeLogs{{LogRecords: recs}}}}})
}

// gaugeBody has one point per label value, each its own series.
func gaugeBody(t *testing.T, values ...string) []byte {
	pts := make([]*metricspb.NumberDataPoint, len(values))
	for i, v := range values {
		pts[i] = &metricspb.NumberDataPoint{Attributes: []*commonpb.KeyValue{{Key: "k", Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: v}}}}}
	}
	m := &metricspb.Metric{Name: "orders_total", Data: &metricspb.Metric_Gauge{Gauge: &metricspb.Gauge{DataPoints: pts}}}
	return marshal(t, &colmetricspb.ExportMetricsServiceRequest{ResourceMetrics: []*metricspb.ResourceMetrics{{ScopeMetrics: []*metricspb.ScopeMetrics{{Metrics: []*metricspb.Metric{m}}}}}})
}

func seq(from, to int) []string {
	var out []string
	for i := from; i < to; i++ {
		out = append(out, fmt.Sprint(i))
	}
	return out
}

func marshal(t *testing.T, m gproto.Message) []byte {
	b, err := gproto.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func (h *fakeHub) logRecords(t *testing.T, vm uuid.UUID) int {
	n := 0
	for _, r := range h.requests() {
		if r.path != "/otlp/v1/logs" || r.vm != vm.String() {
			continue
		}
		var req collogspb.ExportLogsServiceRequest
		if err := gproto.Unmarshal(r.body, &req); err != nil {
			t.Fatal(err)
		}
		for _, rl := range req.ResourceLogs {
			for _, sl := range rl.ScopeLogs {
				n += len(sl.LogRecords)
			}
		}
	}
	return n
}

func (h *fakeHub) points(t *testing.T, stream string) int {
	n := 0
	for _, r := range h.requests() {
		if r.path != "/otlp/v1/metrics" || r.stream != stream {
			continue
		}
		var req colmetricspb.ExportMetricsServiceRequest
		if err := gproto.Unmarshal(r.body, &req); err != nil {
			t.Fatal(err)
		}
		for _, rm := range req.ResourceMetrics {
			for _, sm := range rm.ScopeMetrics {
				for _, m := range sm.Metrics {
					n += len(m.GetGauge().GetDataPoints())
				}
			}
		}
	}
	return n
}

func TestVMSinkForwardsWithHeaders(t *testing.T) {
	e := newTestEnv(t)
	vm := uuid.New()
	c := e.serve(t, vm)
	if code := post(t, c, "/v1/logs", "app", logsBody(t, 3), true); code != http.StatusOK {
		t.Fatalf("status %d", code)
	}
	e.sink.Emit(vm, "console", []*logspb.LogRecord{{}})
	eventually(t, "forwarded", func() bool { return len(e.hub.requests()) == 2 })
	got := e.hub.requests()
	want := hubReq{path: "/otlp/v1/logs", vm: vm.String(), stream: "app", auth: "Bearer cred", ctype: "application/x-protobuf", cenc: "gzip"}
	if got[0].body = nil; !reflect.DeepEqual(got[0], want) {
		t.Fatalf("request = %+v", got[0])
	}
	if e.hub.logRecords(t, vm) != 4 || got[1].stream != "console" {
		t.Fatalf("records %d, second stream %q", e.hub.logRecords(t, vm), got[1].stream)
	}
}

func TestVMSinkRejectsNodeStreams(t *testing.T) {
	e := newTestEnv(t)
	vm := uuid.New()
	c := e.serve(t, vm)
	for _, tc := range []struct{ path, stream string }{
		{"/v1/logs", "console"}, {"/v1/logs", "egress"}, {"/v1/logs", "usage"}, {"/v1/logs", "bogus"}, {"/v1/logs", ""},
		{"/v1/metrics", "system"}, {"/v1/metrics", "service"}, {"/v1/metrics", "console"},
	} {
		if code := post(t, c, tc.path, tc.stream, logsBody(t, 1), false); code != http.StatusBadRequest {
			t.Errorf("%s %q: status %d", tc.path, tc.stream, code)
		}
	}
	if code := post(t, c, "/v1/traces", "app", nil, false); code != http.StatusNotFound {
		t.Errorf("traces: status %d", code)
	}
	if resp, err := c.Get("http://vsock/v1/logs"); err != nil || resp.StatusCode != http.StatusNotFound {
		t.Errorf("GET: %v %v", resp, err)
	}
	if code := post(t, c, "/v1/logs", "service", []byte("not protobuf\xff"), false); code != http.StatusBadRequest {
		t.Errorf("garbage: status %d", code)
	}
	if code := post(t, c, "/v1/logs", "system", logsBody(t, 1), false); code != http.StatusOK {
		t.Fatalf("valid: status %d", code)
	}
	eventually(t, "valid request", func() bool { return len(e.hub.requests()) > 0 })
	if got := e.hub.requests(); len(got) != 1 || got[0].stream != "system" {
		t.Fatalf("hub got %+v", got)
	}
}

func TestVMSinkLimits(t *testing.T) {
	e := newTestEnv(t)
	vm := uuid.New()
	c := e.serve(t, vm)

	if code := post(t, c, "/v1/logs", "app", logsBody(t, 600), true); code != http.StatusOK {
		t.Fatalf("logs: status %d", code)
	}
	eventually(t, "logs", func() bool { return e.hub.logRecords(t, vm) > 0 })
	if n := e.hub.logRecords(t, vm); n != 500 || e.drops.get("logs") != 100 {
		t.Fatalf("forwarded %d, dropped %d", n, e.drops.get("logs"))
	}

	post(t, c, "/v1/metrics", "app", gaugeBody(t, seq(0, maxSeries+1)...), false)
	post(t, c, "/v1/metrics", "app", gaugeBody(t, "0", "new", fmt.Sprint(maxSeries-1)), false)
	post(t, c, "/v1/metrics", "usage", gaugeBody(t, seq(5000, 5010)...), false)
	eventually(t, "usage", func() bool { return e.hub.points(t, "usage") > 0 })
	if app, usage := e.hub.points(t, "app"), e.hub.points(t, "usage"); app != maxSeries+2 || usage != 10 || e.drops.get("series") != 2 {
		t.Fatalf("app points %d, usage points %d, series dropped %d", app, usage, e.drops.get("series"))
	}

	if code := post(t, c, "/v1/logs", "app", make([]byte, maxBody+1), false); code != http.StatusRequestEntityTooLarge {
		t.Fatalf("big body: status %d", code)
	}
	if code := post(t, c, "/v1/logs", "app", make([]byte, maxBody+1), true); code != http.StatusRequestEntityTooLarge {
		t.Fatalf("big gzip body: status %d", code)
	}
	if e.drops.get("body") != 2 {
		t.Fatalf("body drops %d", e.drops.get("body"))
	}
}

func TestVMSinkIsolation(t *testing.T) {
	e := newTestEnv(t)
	a, b := uuid.New(), uuid.New()
	ca, cb := e.serve(t, a), e.serve(t, b)
	post(t, ca, "/v1/logs", "app", logsBody(t, 600), false)
	post(t, ca, "/v1/logs", "app", logsBody(t, 200), false)
	post(t, cb, "/v1/logs", "app", logsBody(t, 300), false)
	eventually(t, "b forwarded", func() bool { return e.hub.logRecords(t, b) == 300 })
	if n := e.hub.logRecords(t, a); n > 510 {
		t.Fatalf("a forwarded %d", n)
	}

	e.sink.Forget(a)
	post(t, ca, "/v1/logs", "app", logsBody(t, 500), false)
	eventually(t, "a reset", func() bool { return e.hub.logRecords(t, a) >= 1000 })
}

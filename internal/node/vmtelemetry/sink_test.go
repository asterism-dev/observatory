package vmtelemetry

import (
	"bytes"
	"compress/gzip"
	"context"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	collogspb "go.opentelemetry.io/proto/otlp/collector/logs/v1"
	colmetricspb "go.opentelemetry.io/proto/otlp/collector/metrics/v1"
	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	logspb "go.opentelemetry.io/proto/otlp/logs/v1"
	metricspb "go.opentelemetry.io/proto/otlp/metrics/v1"
	resourcepb "go.opentelemetry.io/proto/otlp/resource/v1"
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
	e.sink.Register(vm)
	done := make(chan struct{})
	go func() { e.sink.Serve(l, vm); close(done) }()
	t.Cleanup(func() { l.Close(); <-done })
	return &http.Client{Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", sock)
	}}}
}

func post(t *testing.T, c *http.Client, path, stream string, body []byte, gz bool) int {
	t.Helper()
	code, err := do(c, path, stream, body, gz)
	if err != nil {
		t.Fatal(err)
	}
	return code
}

func do(c *http.Client, path, stream string, body []byte, gz bool) (int, error) {
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
		return 0, err
	}
	resp.Body.Close()
	return resp.StatusCode, nil
}

func logsBody(t *testing.T, n int) []byte {
	recs := make([]*logspb.LogRecord, n)
	for i := range recs {
		recs[i] = &logspb.LogRecord{Body: &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: fmt.Sprint(i)}}}
	}
	return marshal(t, &collogspb.ExportLogsServiceRequest{ResourceLogs: []*logspb.ResourceLogs{{ScopeLogs: []*logspb.ScopeLogs{{LogRecords: recs}}}}})
}

// gaugeBody has one point per label value, each its own series.
func gaugeBody(t *testing.T, name string, values ...string) []byte {
	pts := make([]*metricspb.NumberDataPoint, len(values))
	for i, v := range values {
		pts[i] = &metricspb.NumberDataPoint{Attributes: []*commonpb.KeyValue{{Key: "k", Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: v}}}}}
	}
	m := &metricspb.Metric{Name: name, Data: &metricspb.Metric_Gauge{Gauge: &metricspb.Gauge{DataPoints: pts}}}
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

	post(t, c, "/v1/metrics", "app", gaugeBody(t, "orders_total", seq(0, maxSeries+1)...), false)
	post(t, c, "/v1/metrics", "app", gaugeBody(t, "orders_total", "0", "new", fmt.Sprint(maxSeries-1)), false)
	post(t, c, "/v1/metrics", "app", gaugeBody(t, "orders_total", "new2", "new3"), false)
	post(t, c, "/v1/metrics", "usage", gaugeBody(t, "system.memory.usage", seq(5000, 5010)...), false)
	eventually(t, "usage", func() bool { return e.hub.points(t, "usage") > 0 })
	appReqs := 0
	for _, r := range e.hub.requests() {
		if r.stream == "app" && r.path == "/otlp/v1/metrics" {
			appReqs++
		}
	}
	if appReqs != 2 {
		t.Fatalf("%d app metric requests forwarded, want 2 (the all-dropped one forwards nothing)", appReqs)
	}
	if app, usage := e.hub.points(t, "app"), e.hub.points(t, "usage"); app != maxSeries+2 || usage != 10 || e.drops.get("series") != 4 {
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

	before := e.hub.logRecords(t, a)
	e.sink.Forget(a)
	e.sink.Emit(a, "console", []*logspb.LogRecord{{}})
	post(t, ca, "/v1/logs", "app", logsBody(t, 100), false)
	ca = e.serve(t, a)
	post(t, ca, "/v1/logs", "app", logsBody(t, 500), false)
	eventually(t, "a restarted", func() bool { return e.hub.logRecords(t, a) >= before+500 })
	time.Sleep(50 * time.Millisecond)
	if n := e.hub.logRecords(t, a); n > before+510 {
		t.Fatalf("after Forget: %d records forwarded beyond the new burst", n-before-500)
	}
}

// hostileBody has resAttrs resource attributes and points attribute-less points of one gauge.
func hostileBody(t *testing.T, resAttrs, points int) []byte {
	res := make([]*commonpb.KeyValue, resAttrs)
	for i := range res {
		res[i] = &commonpb.KeyValue{Key: fmt.Sprint("k", i), Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: "v"}}}
	}
	pts := make([]*metricspb.NumberDataPoint, points)
	for i := range pts {
		pts[i] = &metricspb.NumberDataPoint{}
	}
	m := &metricspb.Metric{Name: "m", Data: &metricspb.Metric_Gauge{Gauge: &metricspb.Gauge{DataPoints: pts}}}
	return marshal(t, &colmetricspb.ExportMetricsServiceRequest{ResourceMetrics: []*metricspb.ResourceMetrics{{
		Resource: &resourcepb.Resource{Attributes: res}, ScopeMetrics: []*metricspb.ScopeMetrics{{Metrics: []*metricspb.Metric{m}}},
	}}})
}

func TestVMSinkHostileMetricsDontStall(t *testing.T) {
	e := newTestEnv(t)
	a, b := uuid.New(), uuid.New()
	ca, cb := e.serve(t, a), e.serve(t, b)
	start := time.Now()
	if code := post(t, ca, "/v1/metrics", "app", hostileBody(t, 2000, 1000), false); code != http.StatusBadRequest || e.drops.get("body") != 1 {
		t.Fatalf("2 000 resource attributes: status %d, body drops %d", code, e.drops.get("body"))
	}
	t.Logf("rejected in %v", time.Since(start))

	done := make(chan error, 1)
	go func() {
		code, err := do(ca, "/v1/metrics", "app", hostileBody(t, maxAttrs, 60000), false)
		if err == nil && code != http.StatusOK {
			err = fmt.Errorf("status %d", code)
		}
		done <- err
	}()
	start = time.Now()
	if code := post(t, cb, "/v1/logs", "app", logsBody(t, 1), false); code != http.StatusOK || time.Since(start) > time.Second {
		t.Fatalf("other VM: status %d after %v", code, time.Since(start))
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("128 resource attributes x 60 000 points still running")
	}
}

func TestVMSinkUsageAllowlist(t *testing.T) {
	e := newTestEnv(t)
	vm := uuid.New()
	c := e.serve(t, vm)
	post(t, c, "/v1/metrics", "usage", gaugeBody(t, "orders_total", seq(0, 10)...), false)
	post(t, c, "/v1/metrics", "usage", gaugeBody(t, "system.cpu.time", seq(0, maxUsageSeries+100)...), false)
	eventually(t, "usage", func() bool { return e.hub.points(t, "usage") > 0 })
	if n := e.hub.points(t, "usage"); n != 2000 || e.drops.get("series") != 110 {
		t.Fatalf("usage points %d, series dropped %d", n, e.drops.get("series"))
	}
	for _, r := range e.hub.requests() {
		if bytes.Contains(r.body, []byte("orders_total")) {
			t.Fatal("non-usage metric forwarded on usage")
		}
	}
}

func TestVMSinkConnectionCap(t *testing.T) {
	e := newTestEnv(t)
	vm := uuid.New()
	c := e.serve(t, vm)
	sock := c.Transport.(*http.Transport).DialContext
	var idle []net.Conn
	for range maxConns {
		conn, err := sock(context.Background(), "", "")
		if err != nil {
			t.Fatal(err)
		}
		defer conn.Close()
		idle = append(idle, conn)
	}
	c.Timeout = 300 * time.Millisecond
	if _, err := do(c, "/v1/logs", "app", logsBody(t, 1), false); err == nil {
		t.Fatal("request served past the connection cap")
	}
	idle[0].Close()
	eventually(t, "served after a connection closed", func() bool {
		code, err := do(c, "/v1/logs", "app", logsBody(t, 1), false)
		return err == nil && code == http.StatusOK
	})
}

func TestVMSinkDecodeBounds(t *testing.T) {
	e := newTestEnv(t)
	vm := uuid.New()
	c := e.serve(t, vm)
	if code := post(t, c, "/v1/logs", "app", logsBody(t, maxItems+1), true); code != http.StatusRequestEntityTooLarge || e.drops.get("body") != 1 {
		t.Fatalf("too many records: status %d, body drops %d", code, e.drops.get("body"))
	}

	old := decodeWait
	decodeWait = 50 * time.Millisecond
	t.Cleanup(func() { decodeWait = old })
	for range maxDecodes {
		e.sink.decodes <- struct{}{}
	}
	if code := post(t, c, "/v1/logs", "app", logsBody(t, 1), false); code != http.StatusServiceUnavailable {
		t.Fatalf("all decode slots taken: status %d", code)
	}
	<-e.sink.decodes
	if code := post(t, c, "/v1/logs", "app", logsBody(t, 1), false); code != http.StatusOK {
		t.Fatalf("a decode slot free: status %d", code)
	}
}

func TestVMSinkByteRate(t *testing.T) {
	e := newTestEnv(t)
	vm := uuid.New()
	c := e.serve(t, vm)
	big := func() []byte {
		return marshal(t, &collogspb.ExportLogsServiceRequest{ResourceLogs: []*logspb.ResourceLogs{{ScopeLogs: []*logspb.ScopeLogs{{LogRecords: []*logspb.LogRecord{
			{Body: &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: strings.Repeat("x", 900<<10)}}},
		}}}}}})
	}
	for i := range 2 {
		if code := post(t, c, "/v1/logs", "app", big(), true); code == http.StatusTooManyRequests {
			t.Fatalf("request %d within the burst: status %d", i, code)
		}
	}
	if code := post(t, c, "/v1/logs", "app", big(), true); code != http.StatusTooManyRequests || e.drops.get("body") != 1 {
		t.Fatalf("past the burst: status %d, body drops %d", code, e.drops.get("body"))
	}
}

func TestVMSinkEmitFromRegistration(t *testing.T) {
	e := newTestEnv(t)
	vm := uuid.New()
	e.sink.Emit(vm, "console", []*logspb.LogRecord{{}})
	e.sink.Register(vm)
	e.sink.Emit(vm, "console", []*logspb.LogRecord{{}, {}})
	eventually(t, "early console lines", func() bool { return e.hub.logRecords(t, vm) == 2 })
	e.sink.Forget(vm)
	e.sink.Emit(vm, "console", []*logspb.LogRecord{{}})
	e.sink.Emit(uuid.New(), "console", []*logspb.LogRecord{{}})
	time.Sleep(50 * time.Millisecond)
	if n := e.hub.logRecords(t, vm); n != 2 || len(e.hub.requests()) != 1 {
		t.Fatalf("records %d, requests %d: before Register or after Forget forwarded", n, len(e.hub.requests()))
	}
}

func TestVMSinkKeepsQueueAfterExit(t *testing.T) {
	fastRetry(t)
	old := forgetDelay
	forgetDelay = 200 * time.Millisecond
	t.Cleanup(func() { forgetDelay = old })
	down := httptest.NewServer(http.NotFoundHandler())
	down.Close()
	fw := newTestForwarder(t, down.URL)
	sink := NewSink(fw, nil)
	a, b := uuid.New(), uuid.New()
	for _, vm := range []uuid.UUID{a, b} {
		sink.Register(vm)
		sink.Emit(vm, "console", []*logspb.LogRecord{{}})
		sink.Forget(vm)
	}
	sink.Register(b) // b boots again before the delay ends
	if len(queued(fw, a)) != 1 || len(queued(fw, b)) != 1 {
		t.Fatal("queue dropped at exit")
	}
	eventually(t, "a's queue forgotten", func() bool { return queued(fw, a) == nil })
	if len(queued(fw, b)) != 1 {
		t.Fatal("queue of a VM registered again was forgotten")
	}
}

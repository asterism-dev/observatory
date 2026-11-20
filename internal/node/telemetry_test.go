package node

import (
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"io"
	"log"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"go.opentelemetry.io/otel/attribute"
	sdklog "go.opentelemetry.io/otel/sdk/log"
	collogs "go.opentelemetry.io/proto/otlp/collector/logs/v1"
	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	logspb "go.opentelemetry.io/proto/otlp/logs/v1"
	"google.golang.org/protobuf/proto"
)

type logCollector struct {
	mu      sync.Mutex
	reqs    []*collogs.ExportLogsServiceRequest
	headers http.Header
}

func (c *logCollector) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	var body io.Reader = r.Body
	if r.Header.Get("Content-Encoding") == "gzip" {
		body, _ = gzip.NewReader(r.Body)
	}
	raw, _ := io.ReadAll(body)
	req := &collogs.ExportLogsServiceRequest{}
	if r.URL.Path == "/otlp/v1/logs" && proto.Unmarshal(raw, req) == nil {
		c.mu.Lock()
		c.reqs = append(c.reqs, req)
		c.headers = r.Header.Clone()
		c.mu.Unlock()
	}
	w.Header().Set("Content-Type", "application/x-protobuf")
	w.WriteHeader(http.StatusOK)
}

func (c *logCollector) records() []*logspb.LogRecord {
	c.mu.Lock()
	defer c.mu.Unlock()
	var out []*logspb.LogRecord
	for _, req := range c.reqs {
		for _, rl := range req.ResourceLogs {
			for _, sl := range rl.ScopeLogs {
				out = append(out, sl.LogRecords...)
			}
		}
	}
	return out
}

func attrMap(kvs []*commonpb.KeyValue) map[string]string {
	m := map[string]string{}
	for _, kv := range kvs {
		m[kv.Key] = kv.Value.GetStringValue()
	}
	return m
}

func startTestTelemetry(t *testing.T, connectURL string, client *http.Client) *Telemetry {
	t.Helper()
	prev := slog.Default()
	t.Cleanup(func() { slog.SetDefault(prev) })
	tel, err := startTelemetry(context.Background(), connectURL, "cred", "1.2.3", client)
	if err != nil {
		t.Fatal(err)
	}
	return tel
}

func shutdownTelemetry(t *testing.T, tel *Telemetry) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := tel.Shutdown(ctx); err != nil {
		t.Fatal(err)
	}
}

func TestTelemetrySendsLogs(t *testing.T) {
	c := &logCollector{}
	hub := httptest.NewServer(c)
	defer hub.Close()
	tel := startTestTelemetry(t, hub.URL+"/api/nodes/connect", nil)

	slog.Info("vm started", "vm", "vm01")
	shutdownTelemetry(t, tel)

	recs := c.records()
	if len(recs) != 1 {
		t.Fatalf("records = %d, want 1", len(recs))
	}
	if recs[0].Body.GetStringValue() != "vm started" || recs[0].SeverityText != "INFO" {
		t.Fatalf("record = %v", recs[0])
	}
	attrs := attrMap(recs[0].Attributes)
	if attrs["vm"] != "vm01" || attrs["source"] != "observatory-node" {
		t.Fatalf("attrs = %v", attrs)
	}
	if got := c.headers.Get("Authorization"); got != "Bearer cred" {
		t.Fatalf("Authorization = %q", got)
	}
	res := attrMap(c.reqs[0].ResourceLogs[0].Resource.Attributes)
	if res["service.name"] != "observatory-node" || res["service.version"] != "1.2.3" {
		t.Fatalf("resource = %v", res)
	}
}

func TestTelemetryKeepsExplicitSource(t *testing.T) {
	c := &logCollector{}
	hub := httptest.NewServer(c)
	defer hub.Close()
	tel := startTestTelemetry(t, hub.URL+"/api/nodes/connect", nil)

	slog.Info("boot", "source", "firecracker")
	shutdownTelemetry(t, tel)

	recs := c.records()
	if len(recs) != 1 {
		t.Fatalf("records = %d, want 1", len(recs))
	}
	var sources []string
	for _, kv := range recs[0].Attributes {
		if kv.Key == "source" {
			sources = append(sources, kv.Value.GetStringValue())
		}
	}
	if len(sources) != 1 || sources[0] != "firecracker" {
		t.Fatalf("source attrs = %v", sources)
	}
}

func TestTelemetryLoggerSourceAndGroups(t *testing.T) {
	c := &logCollector{}
	hub := httptest.NewServer(c)
	defer hub.Close()
	tel := startTestTelemetry(t, hub.URL+"/api/nodes/connect", nil)

	slog.Default().With("source", "firecracker").Info("with")
	slog.Default().WithGroup("g").Info("grouped", "k", "v")
	shutdownTelemetry(t, tel)

	for _, r := range c.records() {
		var sources []string
		for _, kv := range r.Attributes {
			if kv.Key == "source" {
				sources = append(sources, kv.Value.GetStringValue())
			}
		}
		switch r.Body.GetStringValue() {
		case "with":
			if len(sources) != 1 || sources[0] != "firecracker" {
				t.Fatalf("with: source attrs = %v", sources)
			}
		case "grouped":
			if len(sources) != 0 {
				t.Fatalf("grouped: source attrs = %v", sources)
			}
		}
	}
	if len(c.records()) != 2 {
		t.Fatalf("records = %d, want 2", len(c.records()))
	}
}

func TestTelemetryLegacyLogBridged(t *testing.T) {
	c := &logCollector{}
	hub := httptest.NewServer(c)
	defer hub.Close()
	tel := startTestTelemetry(t, hub.URL+"/api/nodes/connect", nil)

	log.Printf("x %d", 1)
	shutdownTelemetry(t, tel)

	recs := c.records()
	if len(recs) != 1 || recs[0].Body.GetStringValue() != "x 1" {
		t.Fatalf("records = %v", recs)
	}
}

func TestTelemetryHubDownDoesNotBlock(t *testing.T) {
	hub := httptest.NewServer(http.NotFoundHandler())
	url := hub.URL
	hub.Close()
	tel := startTestTelemetry(t, url+"/api/nodes/connect", nil)

	start := time.Now()
	for range 20000 {
		slog.Info("spam")
	}
	if d := time.Since(start); d > time.Second {
		t.Fatalf("logging took %v", d)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	start = time.Now()
	_ = tel.Shutdown(ctx)
	if d := time.Since(start); d > 3*time.Second {
		t.Fatalf("shutdown took %v", d)
	}
}

type stubTransport struct {
	mu   sync.Mutex
	urls []string
}

func (s *stubTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	s.mu.Lock()
	s.urls = append(s.urls, r.URL.String())
	s.mu.Unlock()
	return &http.Response{
		StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"application/x-protobuf"}},
		Body: io.NopCloser(bytes.NewReader(nil)), Request: r,
	}, nil
}

func TestTelemetryEndpointFromConnectURL(t *testing.T) {
	stub := &stubTransport{}
	tel := startTestTelemetry(t, "wss://hub.example/api/nodes/connect", &http.Client{Transport: stub})

	slog.Info("hello")
	shutdownTelemetry(t, tel)

	stub.mu.Lock()
	defer stub.mu.Unlock()
	if len(stub.urls) == 0 || stub.urls[0] != "https://hub.example/otlp/v1/logs" {
		t.Fatalf("urls = %v", stub.urls)
	}
}

func TestTelemetryRejectedExportNoFeedbackLoop(t *testing.T) {
	var logPosts atomic.Int32
	hub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/otlp/v1/logs" {
			logPosts.Add(1)
		}
		http.NotFound(w, r)
	}))
	defer hub.Close()
	tel := startTestTelemetry(t, hub.URL+"/api/nodes/connect", nil)

	slog.Info("once")
	time.Sleep(4500 * time.Millisecond)
	_ = tel.Shutdown(context.Background())

	if n := logPosts.Load(); n > 2 {
		t.Fatalf("log exports = %d, want <= 2 (export errors must not be re-logged into the exporter)", n)
	}
}

func TestOTelErrorHandlerThrottles(t *testing.T) {
	var buf bytes.Buffer
	h := newOTelErrorHandler(&buf)
	for range 5 {
		h.Handle(errors.New("failed to upload logs: 404 Not Found"))
	}
	h.Handle(errors.New("other"))
	if n := strings.Count(buf.String(), "\n"); n != 2 {
		t.Fatalf("lines = %d, want 2:\n%s", n, buf.String())
	}
}

func TestTelemetryKeepsLogsWhileHubDown(t *testing.T) {
	c := &logCollector{}
	var down atomic.Bool
	var refused atomic.Int32
	down.Store(true)
	hub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if down.Load() {
			refused.Add(1)
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		c.ServeHTTP(w, r)
	}))
	defer hub.Close()
	tel := startTestTelemetry(t, hub.URL+"/api/nodes/connect", nil)

	slog.Warn("a")
	slog.Warn("b")
	for deadline := time.Now().Add(5 * time.Second); refused.Load() == 0; time.Sleep(50 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("no export attempted while down")
		}
	}
	slog.Warn("c")
	time.Sleep(1500 * time.Millisecond)
	down.Store(false)
	slog.Info("d")
	shutdownTelemetry(t, tel)

	var got []string
	for _, r := range c.records() {
		got = append(got, r.Body.GetStringValue())
	}
	if strings.Join(got, ",") != "a,b,c,d" {
		t.Fatalf("records = %v, want a,b,c,d", got)
	}
}

type flakyExporter struct {
	fail   bool
	bodies []string
}

func (f *flakyExporter) Export(_ context.Context, records []sdklog.Record) error {
	if f.fail {
		return errors.New("down")
	}
	for _, r := range records {
		f.bodies = append(f.bodies, r.Body().AsString())
	}
	return nil
}
func (f *flakyExporter) Shutdown(context.Context) error   { return nil }
func (f *flakyExporter) ForceFlush(context.Context) error { return nil }

func TestRetryingLogExporterDropsOldestBeyondBound(t *testing.T) {
	inner := &flakyExporter{fail: true}
	e := &retryingLogExporter{Exporter: inner, limit: 3}
	export := func(bodies ...string) error {
		recs := make([]sdklog.Record, len(bodies))
		for i, b := range bodies {
			recs[i].SetBody(attribute.StringValue(b))
		}
		return e.Export(context.Background(), recs)
	}
	if export("1", "2") == nil || export("3", "4", "5") == nil {
		t.Fatal("failing export must return an error")
	}
	inner.fail = false
	if err := export("6"); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(inner.bodies, ","); got != "4,5,6" {
		t.Fatalf("exported %s, want 4,5,6", got)
	}
}

func TestTelemetryDurationsAsStrings(t *testing.T) {
	c := &logCollector{}
	hub := httptest.NewServer(c)
	defer hub.Close()
	tel := startTestTelemetry(t, hub.URL+"/api/nodes/connect", nil)

	slog.Default().With("wait", 5*time.Second).Info("built", "took", 79*time.Second)
	shutdownTelemetry(t, tel)

	recs := c.records()
	if len(recs) != 1 {
		t.Fatalf("records = %d, want 1", len(recs))
	}
	attrs := attrMap(recs[0].Attributes)
	if attrs["took"] != "1m19s" || attrs["wait"] != "5s" {
		t.Fatalf("attrs = %v", attrs)
	}
}

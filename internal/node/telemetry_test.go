package node

import (
	"bytes"
	"compress/gzip"
	"context"
	"io"
	"log"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

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

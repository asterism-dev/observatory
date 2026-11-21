package telemetry_test

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/google/uuid"
	collogspb "go.opentelemetry.io/proto/otlp/collector/logs/v1"
	colmetricspb "go.opentelemetry.io/proto/otlp/collector/metrics/v1"

	"github.com/asterism-dev/observatory/internal/telemetry"
)

func TestClientPathsAndErrors(t *testing.T) {
	status := 200
	var path, fields, ctype string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path, fields, ctype = r.URL.Path, r.Header.Get("VL-Stream-Fields"), r.Header.Get("Content-Type")
		_, _ = io.Copy(io.Discard, r.Body)
		w.WriteHeader(status)
	}))
	defer srv.Close()
	c := &telemetry.Client{MetricsURL: srv.URL, LogsURL: srv.URL}
	ctx := context.Background()

	if err := c.WriteMetrics(ctx, &colmetricspb.ExportMetricsServiceRequest{}); err != nil || path != "/opentelemetry/v1/metrics" || ctype != "application/x-protobuf" {
		t.Fatalf("%v %q %q", err, path, ctype)
	}
	if err := c.WriteLogs(ctx, &collogspb.ExportLogsServiceRequest{}, telemetry.NodeLogStreamFields); err != nil || path != "/insert/opentelemetry/v1/logs" || fields != "observatory.org,observatory.node,service.name" {
		t.Fatalf("%v %q %q", err, path, fields)
	}
	status = 500
	if err := c.WriteMetrics(ctx, &colmetricspb.ExportMetricsServiceRequest{}); !errors.Is(err, telemetry.ErrStore) {
		t.Fatalf("5xx: %v", err)
	}
	status = 400
	if err := c.WriteMetrics(ctx, &colmetricspb.ExportMetricsServiceRequest{}); err == nil || errors.Is(err, telemetry.ErrStore) {
		t.Fatalf("4xx: %v", err)
	}
	srv.Close()
	if err := c.WriteMetrics(ctx, &colmetricspb.ExportMetricsServiceRequest{}); !errors.Is(err, telemetry.ErrStore) {
		t.Fatalf("closed: %v", err)
	}
}

func TestClientRefusesRedirects(t *testing.T) {
	hits := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		http.Redirect(w, r, "/elsewhere", http.StatusFound)
	}))
	defer srv.Close()
	custom := &http.Client{}
	for _, c := range []*telemetry.Client{{MetricsURL: srv.URL, LogsURL: srv.URL}, {MetricsURL: srv.URL, LogsURL: srv.URL, HTTP: custom}} {
		hits = 0
		if err := c.WriteMetrics(context.Background(), &colmetricspb.ExportMetricsServiceRequest{}); err == nil || hits != 1 {
			t.Fatalf("write: %v, %d hits", err, hits)
		}
		hits = 0
		if _, err := c.Logs(context.Background(), telemetry.LogQuery{Node: uuid.New(), Limit: 1}); err == nil || hits != 1 {
			t.Fatalf("query: %v, %d hits", err, hits)
		}
	}
	if custom.CheckRedirect != nil {
		t.Fatal("caller's client mutated")
	}
	if hc := (&telemetry.Client{HTTP: custom}).StoreClient(time.Second); hc.Timeout != time.Second {
		t.Fatalf("custom client timeout: %s", hc.Timeout)
	}
}

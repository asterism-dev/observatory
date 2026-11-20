package telemetry_test

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

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

package telemetry_test

import (
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"runtime"
	"testing"
	"time"

	"github.com/google/uuid"
	collogspb "go.opentelemetry.io/proto/otlp/collector/logs/v1"
	colmetricspb "go.opentelemetry.io/proto/otlp/collector/metrics/v1"
	"google.golang.org/protobuf/proto"

	"github.com/asterism-dev/observatory/internal/node"
	"github.com/asterism-dev/observatory/internal/telemetry"
)

func TestNodeTelemetryRoundTrip(t *testing.T) {
	metricsURL, logsURL := os.Getenv("OBSERVATORY_TEST_METRICS_URL"), os.Getenv("OBSERVATORY_TEST_LOGS_URL")
	if metricsURL == "" || logsURL == "" {
		t.Skip("OBSERVATORY_TEST_METRICS_URL and OBSERVATORY_TEST_LOGS_URL not set")
	}
	org, other, nodeID := uuid.New(), uuid.New(), uuid.New()
	id := telemetry.Identity{OrgID: org, NodeID: nodeID, NodeName: "roundtrip"}
	client := &telemetry.Client{MetricsURL: metricsURL, LogsURL: logsURL}

	hub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body io.Reader = r.Body
		if r.Header.Get("Content-Encoding") == "gzip" {
			zr, err := gzip.NewReader(r.Body)
			if err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			body = zr
		}
		raw, err := io.ReadAll(body)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		switch r.URL.Path {
		case "/otlp/v1/metrics":
			var req colmetricspb.ExportMetricsServiceRequest
			if err = proto.Unmarshal(raw, &req); err == nil {
				telemetry.StampMetrics(&req, id)
				err = client.WriteMetrics(r.Context(), &req)
			}
		case "/otlp/v1/logs":
			var req collogspb.ExportLogsServiceRequest
			if err = proto.Unmarshal(raw, &req); err == nil {
				telemetry.StampLogs(&req, id)
				err = client.WriteLogs(r.Context(), &req, telemetry.NodeLogStreamFields)
			}
		default:
			http.NotFound(w, r)
			return
		}
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer hub.Close()

	prev := slog.Default()
	t.Cleanup(func() { slog.SetDefault(prev) })
	tel, err := node.StartTelemetry(context.Background(), hub.URL, "test", "test")
	if err != nil {
		t.Fatal(err)
	}
	if err := (&node.Agent{}).RegisterHostMetrics(tel.Meter, []string{"/"}); err != nil {
		t.Fatal(err)
	}
	slog.Warn("image pull failed, retrying", "err", "connection reset by peer", "vm", "vm01")
	// rate() needs two samples; the periodic reader exports every 10s, shutdown adds the second.
	if runtime.GOOS == "linux" {
		time.Sleep(11 * time.Second)
	}
	if err := tel.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}

	from, to := time.Now().Add(-5*time.Minute), time.Now().Add(time.Minute)
	logs := func(org uuid.UUID, text string, fields map[string]string) []telemetry.LogLine {
		lines, err := client.Logs(context.Background(), telemetry.LogQuery{Org: org, Node: nodeID, From: from, To: to, Text: text, Fields: fields, Limit: 10})
		if err != nil {
			t.Fatal(err)
		}
		return lines
	}
	waitFor(t, "log by text", func() bool { return len(logs(org, "connection reset", nil)) > 0 })
	waitFor(t, "log by field", func() bool { return len(logs(org, "", map[string]string{"vm": "vm01"})) > 0 })
	line := logs(org, "connection reset", nil)[0]
	if line.Level != "warn" || line.Source != "observatory-node" {
		t.Fatalf("level %q source %q", line.Level, line.Source)
	}
	if got := logs(other, "connection reset", nil); len(got) != 0 {
		t.Fatalf("second org sees %d lines", len(got))
	}

	// Only vms.running exists off Linux, so the host charts are asserted on Linux alone.
	waitFor(t, "vms.running", func() bool {
		return queryCount(t, metricsURL, fmt.Sprintf(`observatory_node_vms_running{observatory_node=%q}`, nodeID), org) > 0
	})
	if queryCount(t, metricsURL, fmt.Sprintf(`observatory_node_vms_running{observatory_node=%q}`, nodeID), other) != 0 {
		t.Fatal("second org sees metrics")
	}
	if runtime.GOOS != "linux" {
		return
	}
	waitFor(t, "usage charts", func() bool {
		u, err := client.NodeUsage(context.Background(), org, nodeID, from, to)
		if err != nil {
			t.Fatal(err)
		}
		for _, chart := range []string{"cpu", "mem", "disk", "net"} {
			if len(u.Charts[chart]) == 0 {
				return false
			}
		}
		return true
	})
}

func queryCount(t *testing.T, metricsURL, expr string, org uuid.UUID) int {
	t.Helper()
	resp, err := http.PostForm(metricsURL+"/api/v1/query", url.Values{"query": {expr}, "extra_label": {"observatory_org=" + org.String()}})
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var body struct {
		Data struct{ Result []json.RawMessage } `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	return len(body.Data.Result)
}

// VictoriaMetrics hides samples younger than its 30s -search.latencyOffset.
func waitFor(t *testing.T, what string, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(90 * time.Second)
	for !ok() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(500 * time.Millisecond)
	}
}

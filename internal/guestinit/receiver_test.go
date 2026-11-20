package guestinit

import (
	"bytes"
	"compress/gzip"
	"context"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	collogspb "go.opentelemetry.io/proto/otlp/collector/logs/v1"
	logspb "go.opentelemetry.io/proto/otlp/logs/v1"
	"google.golang.org/protobuf/encoding/protojson"
	gproto "google.golang.org/protobuf/proto"
)

func startTestReceiver(t *testing.T) (url string, node *fakeNode) {
	t.Helper()
	dial, node := startFakeNode(t, nil)
	addr := freeAddr(t)
	stop, err := startReceiver(context.Background(), newSink(dial), addr)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(stop)
	return "http://" + addr, node
}

func freeAddr(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	return l.Addr().String()
}

func logsRequest(msg string) *collogspb.ExportLogsServiceRequest {
	return &collogspb.ExportLogsServiceRequest{ResourceLogs: []*logspb.ResourceLogs{{ScopeLogs: []*logspb.ScopeLogs{{LogRecords: []*logspb.LogRecord{logRecord(time.Now(), msg, infoLevel)}}}}}}
}

func post(t *testing.T, url, ct string, body []byte, gz bool) *http.Response {
	t.Helper()
	if gz {
		var b bytes.Buffer
		zw := gzip.NewWriter(&b)
		zw.Write(body)
		zw.Close()
		body = b.Bytes()
	}
	req, _ := http.NewRequest("POST", url, bytes.NewReader(body))
	req.Header.Set("Content-Type", ct)
	if gz {
		req.Header.Set("Content-Encoding", "gzip")
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { resp.Body.Close() })
	return resp
}

func TestReceiverHTTPProtobuf(t *testing.T) {
	url, node := startTestReceiver(t)
	body, _ := gproto.Marshal(logsRequest("pb"))
	resp := post(t, url+"/v1/logs", "application/x-protobuf", body, true)
	if resp.StatusCode != 200 || resp.Header.Get("Content-Type") != "application/x-protobuf" {
		t.Fatalf("response = %d %v", resp.StatusCode, resp.Header)
	}
	waitFor(t, func() bool { return len(node.requests()) == 1 })
	r := node.requests()[0]
	if r.path != "/v1/logs" || r.stream != "app" || decodeLogs(t, r.body)[0].GetBody().GetStringValue() != "pb" {
		t.Fatalf("request = %+v", r)
	}
}

func TestReceiverHTTPJSON(t *testing.T) {
	url, node := startTestReceiver(t)
	body, _ := protojson.Marshal(logsRequest("js"))
	resp := post(t, url+"/v1/logs", "application/json", body, false)
	if resp.StatusCode != 200 || resp.Header.Get("Content-Type") != "application/json" {
		t.Fatalf("response = %d %v", resp.StatusCode, resp.Header)
	}
	waitFor(t, func() bool { return len(node.requests()) == 1 })
	r := node.requests()[0]
	if r.contentType != "application/x-protobuf" || decodeLogs(t, r.body)[0].GetBody().GetStringValue() != "js" {
		t.Fatalf("request = %+v", r)
	}
}

func TestReceiverMetrics(t *testing.T) {
	url, node := startTestReceiver(t)
	resp := post(t, url+"/v1/metrics", "application/json", []byte(`{"resourceMetrics":[{"scopeMetrics":[{"metrics":[{"name":"m"}]}]}]}`), false)
	if resp.StatusCode != 200 {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	waitFor(t, func() bool { return len(node.requests()) == 1 })
	if r := node.requests()[0]; r.path != "/v1/metrics" || r.stream != "app" {
		t.Fatalf("request = %+v", r)
	}
}

func TestReceiverRejectsTraces(t *testing.T) {
	url, _ := startTestReceiver(t)
	if resp := post(t, url+"/v1/traces", "application/x-protobuf", nil, false); resp.StatusCode != 404 {
		t.Fatalf("status = %d", resp.StatusCode)
	}
}

func TestReceiverBodyLimit(t *testing.T) {
	url, _ := startTestReceiver(t)
	big := []byte(strings.Repeat("x", maxReceiverBody+1))
	if resp := post(t, url+"/v1/logs", "application/x-protobuf", big, false); resp.StatusCode != 413 {
		t.Fatalf("plain status = %d", resp.StatusCode)
	}
	if resp := post(t, url+"/v1/logs", "application/x-protobuf", big, true); resp.StatusCode != 413 {
		t.Fatalf("gzip status = %d", resp.StatusCode)
	}
}

func TestReceiverPortTaken(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	var out bytes.Buffer
	defer func(w io.Writer, s *Sink) { stderr, telemetry = w, s }(stderr, telemetry)
	stderr, telemetry = &out, nil
	stop, err := startReceiver(context.Background(), nil, l.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	stop()
	if !strings.Contains(out.String(), "otlp receiver") {
		t.Fatalf("no log: %q", out.String())
	}
}

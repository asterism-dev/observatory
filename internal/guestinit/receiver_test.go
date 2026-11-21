package guestinit

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/hex"
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
		if _, err := zw.Write(body); err != nil {
			t.Fatal(err)
		}
		if err := zw.Close(); err != nil {
			t.Fatal(err)
		}
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

func TestReceiverJSONHexIDs(t *testing.T) {
	url, node := startTestReceiver(t)
	body := `{"resourceLogs":[{"scopeLogs":[{"logRecords":[{"traceId":"5b8efff798038103d269b633813fc60c","spanId":"eee19b7ec3c1b174","body":{"stringValue":"id"}}]}]}]}`
	if resp := post(t, url+"/v1/logs", "application/json", []byte(body), false); resp.StatusCode != 200 {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	waitFor(t, func() bool { return len(node.requests()) == 1 })
	rec := decodeLogs(t, node.requests()[0].body)[0]
	if hex.EncodeToString(rec.TraceId) != "5b8efff798038103d269b633813fc60c" || hex.EncodeToString(rec.SpanId) != "eee19b7ec3c1b174" {
		t.Fatalf("ids = %x %x", rec.TraceId, rec.SpanId)
	}
}

func TestReceiverConnectionCap(t *testing.T) {
	url, _ := startTestReceiver(t)
	addr := strings.TrimPrefix(url, "http://")
	var held []net.Conn
	for range maxReceiverConns {
		c, err := net.Dial("tcp", addr)
		if err != nil {
			t.Fatal(err)
		}
		held = append(held, c)
	}
	defer func() {
		for _, c := range held {
			c.Close()
		}
	}()
	time.Sleep(100 * time.Millisecond)
	extra, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer extra.Close()
	extra.SetReadDeadline(time.Now().Add(2 * time.Second))
	if _, err := extra.Read(make([]byte, 1)); err != io.EOF {
		t.Fatalf("extra connection read = %v, want EOF", err)
	}
	held[0].Close()
	held = held[1:]
	waitFor(t, func() bool {
		resp, err := http.Get(url + "/v1/traces")
		if err != nil {
			return false
		}
		resp.Body.Close()
		return resp.StatusCode == 404
	})
}

func TestReceiverKeepsResourceAndScope(t *testing.T) {
	url, node := startTestReceiver(t)
	body := `{"resourceLogs":[{"resource":{"attributes":[{"key":"service.name","value":{"stringValue":"shop"}}]},` +
		`"scopeLogs":[{"scope":{"name":"checkout"},"logRecords":[{"body":{"stringValue":"a"}}]},{"scope":{"name":"cart"},"logRecords":[{"body":{"stringValue":"b"}}]}]}]}`
	if resp := post(t, url+"/v1/logs", "application/json", []byte(body), false); resp.StatusCode != 200 {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	waitFor(t, func() bool { return len(node.requests()) > 0 })
	time.Sleep(50 * time.Millisecond)
	var scopes []string
	for _, r := range node.requests() {
		var req collogspb.ExportLogsServiceRequest
		if err := gproto.Unmarshal(r.body, &req); err != nil {
			t.Fatal(err)
		}
		for _, rl := range req.ResourceLogs {
			if a := rl.GetResource().GetAttributes(); len(a) != 1 || a[0].Key != "service.name" || a[0].Value.GetStringValue() != "shop" {
				t.Fatalf("resource = %v", rl.GetResource())
			}
			for _, sl := range rl.ScopeLogs {
				scopes = append(scopes, sl.GetScope().GetName()+":"+sl.LogRecords[0].Body.GetStringValue())
			}
		}
	}
	if strings.Join(scopes, ",") != "checkout:a,cart:b" {
		t.Fatalf("scopes = %v", scopes)
	}
}

func TestReceiverJSONKeepsLargeNumbers(t *testing.T) {
	url, node := startTestReceiver(t)
	body := `{"resourceLogs":[{"scopeLogs":[{"logRecords":[{"timeUnixNano":1700000000123456789,"body":{"stringValue":"n"}}]}]}]}`
	if resp := post(t, url+"/v1/logs", "application/json", []byte(body), false); resp.StatusCode != 200 {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	waitFor(t, func() bool { return len(node.requests()) == 1 })
	if got := decodeLogs(t, node.requests()[0].body)[0].TimeUnixNano; got != 1700000000123456789 {
		t.Fatalf("timeUnixNano = %d", got)
	}
	if resp := post(t, url+"/v1/logs", "application/json", []byte(`{} trailing`), false); resp.StatusCode != 400 {
		t.Fatalf("trailing data: status = %d", resp.StatusCode)
	}
}

package api

import (
	"bytes"
	"compress/gzip"
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"
	collogspb "go.opentelemetry.io/proto/otlp/collector/logs/v1"
	colmetricspb "go.opentelemetry.io/proto/otlp/collector/metrics/v1"
	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	logspb "go.opentelemetry.io/proto/otlp/logs/v1"
	metricspb "go.opentelemetry.io/proto/otlp/metrics/v1"
	resourcepb "go.opentelemetry.io/proto/otlp/resource/v1"
	gproto "google.golang.org/protobuf/proto"

	"github.com/asterism-dev/observatory/internal/proto"
	"github.com/asterism-dev/observatory/internal/store"
	"github.com/asterism-dev/observatory/internal/telemetry"
)

type fakeStore struct {
	*httptest.Server
	mu      sync.Mutex
	status  int
	paths   []string
	queries []string
	bodies  [][]byte
	header  http.Header
}

func newFakeStore(t *testing.T) *fakeStore {
	f := &fakeStore{status: 200}
	f.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		f.mu.Lock()
		defer f.mu.Unlock()
		f.paths, f.queries, f.bodies, f.header = append(f.paths, r.URL.Path), append(f.queries, r.URL.RawQuery), append(f.bodies, b), r.Header.Clone()
		w.WriteHeader(f.status)
	}))
	t.Cleanup(f.Close)
	return f
}

func (f *fakeStore) received() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.bodies)
}

type otlpEnv struct {
	*env
	mux        *http.ServeMux
	org        uuid.UUID
	node       string
	credential string
	vm, vl     *fakeStore
	alice      store.User
	tok        string
}

func newOTLPEnv(t *testing.T) *otlpEnv {
	e := newEnv(t)
	alice, tok := e.user("alice")
	org := e.createOrg(tok, "Acme")
	c := e.createNode(tok, org, "n1")
	rec := e.enroll(tokenOf(c.InstallCommand))
	var enrolled struct{ Credential string }
	decode(t, rec, &enrolled)
	o := &otlpEnv{env: e, org: org, node: c.Node.ID, credential: enrolled.Credential, vm: newFakeStore(t), vl: newFakeStore(t), alice: alice, tok: tok}
	o.mux = http.NewServeMux()
	OTLPEndpoints(o.mux, e.store, &telemetry.Client{MetricsURL: o.vm.URL, LogsURL: o.vl.URL})
	return o
}

func (o *otlpEnv) post(path, cred, ctype, encoding string, body []byte) *httptest.ResponseRecorder {
	return o.postVM(path, cred, ctype, encoding, "", "", body)
}

func (o *otlpEnv) postVM(path, cred, ctype, encoding, vm, stream string, body []byte) *httptest.ResponseRecorder {
	req := httptest.NewRequest("POST", path, bytes.NewReader(body))
	if vm != "" {
		req.Header.Set("X-Observatory-VM", vm)
	}
	if stream != "" {
		req.Header.Set("X-Observatory-Stream", stream)
	}
	if cred != "" {
		req.Header.Set("Authorization", "Bearer "+cred)
	}
	req.Header.Set("Content-Type", ctype)
	if encoding != "" {
		req.Header.Set("Content-Encoding", encoding)
	}
	rec := httptest.NewRecorder()
	o.mux.ServeHTTP(rec, req)
	return rec
}

func gz(t *testing.T, b []byte) []byte {
	var buf bytes.Buffer
	w := gzip.NewWriter(&buf)
	if _, err := w.Write(b); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func metricsBody(t *testing.T) []byte {
	b, err := gproto.Marshal(&colmetricsReq)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

var colmetricsReq = colmetricspb.ExportMetricsServiceRequest{ResourceMetrics: []*metricspb.ResourceMetrics{{
	Resource: &resourcepb.Resource{Attributes: []*commonpb.KeyValue{
		{Key: "observatory.org", Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: "evil"}}},
	}},
}}}

const (
	metricsPath = "/otlp/v1/metrics"
	logsPath    = "/otlp/v1/logs"
	protobuf    = "application/x-protobuf"
)

func TestIngestMetricsStampsAndForwards(t *testing.T) {
	o := newOTLPEnv(t)
	rec := o.post(metricsPath, o.credential, protobuf, "", metricsBody(t))
	if rec.Code != 200 || rec.Header().Get("Content-Type") != protobuf {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}
	var resp colmetricspb.ExportMetricsServiceResponse
	if err := gproto.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if o.vm.received() != 1 || o.vm.paths[0] != "/opentelemetry/v1/metrics" {
		t.Fatalf("%v", o.vm.paths)
	}
	var got colmetricspb.ExportMetricsServiceRequest
	if err := gproto.Unmarshal(o.vm.bodies[0], &got); err != nil {
		t.Fatal(err)
	}
	attrs := map[string]string{}
	for _, kv := range got.ResourceMetrics[0].Resource.Attributes {
		attrs[kv.Key] = kv.Value.GetStringValue()
	}
	if attrs["observatory.org"] != o.org.String() || attrs["observatory.node"] != o.node || attrs["observatory.node_name"] != "n1" {
		t.Fatalf("%v", attrs)
	}
}

func TestIngestLogsGzip(t *testing.T) {
	o := newOTLPEnv(t)
	b, err := gproto.Marshal(&collogspb.ExportLogsServiceRequest{})
	if err != nil {
		t.Fatal(err)
	}
	rec := o.post(logsPath, o.credential, protobuf, "gzip", gz(t, b))
	if rec.Code != 200 {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}
	if o.vl.received() != 1 || o.vl.paths[0] != "/insert/opentelemetry/v1/logs" || o.vl.header.Get("VL-Stream-Fields") != "observatory.org,observatory.node,service.name" {
		t.Fatalf("%v %v", o.vl.paths, o.vl.header)
	}
}

func TestIngestUnauthorized(t *testing.T) {
	o := newOTLPEnv(t)
	for _, cred := range []string{"", "wrong", o.node + ".wrong"} {
		if rec := o.post(metricsPath, cred, protobuf, "", metricsBody(t)); rec.Code != 401 {
			t.Fatalf("%q: %d", cred, rec.Code)
		}
	}
	if o.vm.received() != 0 {
		t.Fatal("forwarded")
	}
}

func TestIngestQuarantined(t *testing.T) {
	o := newOTLPEnv(t)
	if err := o.net.SetStatus(context.Background(), proto.GatewayStatus{Quarantined: []string{o.node}}); err != nil {
		t.Fatal(err)
	}
	if rec := o.post(metricsPath, o.credential, protobuf, "", metricsBody(t)); rec.Code != 403 {
		t.Fatalf("%d", rec.Code)
	}
	if o.vm.received() != 0 {
		t.Fatal("forwarded")
	}
}

func TestIngestBodyLimitAfterGzip(t *testing.T) {
	o := newOTLPEnv(t)
	rec := o.post(metricsPath, o.credential, protobuf, "gzip", gz(t, make([]byte, 4<<20+1)))
	if rec.Code != 413 {
		t.Fatalf("%d", rec.Code)
	}
	if rec := o.post(metricsPath, o.credential, protobuf, "", make([]byte, 4<<20+1)); rec.Code != 413 {
		t.Fatalf("raw: %d", rec.Code)
	}
	if o.vm.received() != 0 {
		t.Fatal("forwarded")
	}
}

func TestIngestStoreDown(t *testing.T) {
	o := newOTLPEnv(t)
	o.vm.status = 500
	if rec := o.post(metricsPath, o.credential, protobuf, "", metricsBody(t)); rec.Code != 503 {
		t.Fatalf("500: %d", rec.Code)
	}
	o.vm.Close()
	if rec := o.post(metricsPath, o.credential, protobuf, "", metricsBody(t)); rec.Code != 503 {
		t.Fatalf("closed: %d", rec.Code)
	}
}

func TestIngestDisabled(t *testing.T) {
	e := newEnv(t)
	mux := http.NewServeMux()
	OTLPEndpoints(mux, e.store, nil)
	req := httptest.NewRequest("POST", metricsPath, strings.NewReader(""))
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != 404 {
		t.Fatalf("%d", rec.Code)
	}
}

func TestIngestJSONRejected(t *testing.T) {
	o := newOTLPEnv(t)
	if rec := o.post(metricsPath, o.credential, "application/json", "", []byte("{}")); rec.Code != 415 {
		t.Fatalf("%d", rec.Code)
	}
}

func TestIngestInvalidProtobuf(t *testing.T) {
	o := newOTLPEnv(t)
	if rec := o.post(metricsPath, o.credential, protobuf, "", []byte{0xff, 0xff}); rec.Code != 400 {
		t.Fatalf("%d", rec.Code)
	}
}

func TestIngestStoreRejectedLogged(t *testing.T) {
	o := newOTLPEnv(t)
	o.vm.status = 400
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })
	if rec := o.post(metricsPath, o.credential, protobuf, "", metricsBody(t)); rec.Code != 400 {
		t.Fatalf("%d", rec.Code)
	}
	if !strings.Contains(buf.String(), "level=WARN") || !strings.Contains(buf.String(), "status 400") {
		t.Fatalf("log = %q", buf.String())
	}
}

func (o *otlpEnv) createVM(org uuid.UUID, node, name string) string {
	o.t.Helper()
	v, err := o.store.CreateVM(context.Background(), store.VM{OrgID: org, NodeID: uuid.MustParse(node), UserID: &o.alice.ID, Name: name, CPUs: 1, MemoryMB: 512, DiskGB: 10, CreatedBy: o.alice.ID})
	if err != nil {
		o.t.Fatal(err)
	}
	return v.ID.String()
}

func logsBody(t *testing.T) []byte {
	b, err := gproto.Marshal(&collogspb.ExportLogsServiceRequest{ResourceLogs: []*logspb.ResourceLogs{{
		Resource: &resourcepb.Resource{Attributes: []*commonpb.KeyValue{
			{Key: "observatory.vm", Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: "evil"}}},
		}},
	}}})
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestIngestVM(t *testing.T) {
	o := newOTLPEnv(t)
	vm := o.createVM(o.org, o.node, "vm01")
	if rec := o.postVM(logsPath, o.credential, protobuf, "", vm, "console", logsBody(t)); rec.Code != 200 {
		t.Fatalf("logs: %d %s", rec.Code, rec.Body.String())
	}
	if got := o.vl.header.Get("VL-Stream-Fields"); got != "observatory.org,observatory.vm,observatory.stream" {
		t.Fatalf("stream fields %q", got)
	}
	var logs collogspb.ExportLogsServiceRequest
	if err := gproto.Unmarshal(o.vl.bodies[0], &logs); err != nil {
		t.Fatal(err)
	}
	attrs := map[string]string{}
	for _, kv := range logs.ResourceLogs[0].Resource.Attributes {
		attrs[kv.Key] = kv.Value.GetStringValue()
	}
	if attrs["observatory.vm"] != vm || attrs["observatory.vm_name"] != "vm01" || attrs["observatory.stream"] != "console" || attrs["observatory.node"] != o.node {
		t.Fatalf("%v", attrs)
	}
	if rec := o.postVM(metricsPath, o.credential, protobuf, "", vm, "usage", metricsBody(t)); rec.Code != 200 {
		t.Fatalf("metrics: %d %s", rec.Code, rec.Body.String())
	}
	var metrics colmetricspb.ExportMetricsServiceRequest
	if err := gproto.Unmarshal(o.vm.bodies[0], &metrics); err != nil {
		t.Fatal(err)
	}
	attrs = map[string]string{}
	for _, kv := range metrics.ResourceMetrics[0].Resource.Attributes {
		attrs[kv.Key] = kv.Value.GetStringValue()
	}
	if attrs["observatory.vm"] != vm || attrs["observatory.vm_name"] != "vm01" || attrs["observatory.org"] != o.org.String() {
		t.Fatalf("%v", attrs)
	}
}

func TestIngestVMOtherNode(t *testing.T) {
	o := newOTLPEnv(t)
	o.requireVMForbidden(o.createVM(o.org, o.enrolledNode(o.tok, o.org, "n2"), "elsewhere"))
}

func TestIngestVMOtherOrg(t *testing.T) {
	o := newOTLPEnv(t)
	org := o.createOrg(o.tok, "Other")
	o.requireVMForbidden(o.createVM(org, o.enrolledNode(o.tok, org, "n1"), "foreign"))
}

func TestIngestVMUnknown(t *testing.T) {
	o := newOTLPEnv(t)
	o.requireVMForbidden(uuid.NewString())
}

func (o *otlpEnv) requireVMForbidden(vm string) {
	o.t.Helper()
	if rec := o.postVM(logsPath, o.credential, protobuf, "", vm, "app", logsBody(o.t)); rec.Code != 403 {
		o.t.Fatalf("logs: %d", rec.Code)
	}
	if rec := o.postVM(metricsPath, o.credential, protobuf, "", vm, "app", metricsBody(o.t)); rec.Code != 403 {
		o.t.Fatalf("metrics: %d", rec.Code)
	}
	if o.vl.received()+o.vm.received() != 0 {
		o.t.Fatal("forwarded")
	}
}

func TestIngestVMBadStream(t *testing.T) {
	o := newOTLPEnv(t)
	vm := o.createVM(o.org, o.node, "vm01")
	for _, c := range []struct {
		path, stream string
		body         []byte
	}{{logsPath, "usage", logsBody(t)}, {metricsPath, "console", metricsBody(t)}, {logsPath, "", logsBody(t)}, {metricsPath, "", metricsBody(t)}, {logsPath, "observatory", logsBody(t)}} {
		if rec := o.postVM(c.path, o.credential, protobuf, "", vm, c.stream, c.body); rec.Code != 400 {
			t.Fatalf("%s %q: %d", c.path, c.stream, rec.Code)
		}
	}
	if rec := o.postVM(logsPath, o.credential, protobuf, "", "not-a-uuid", "app", logsBody(t)); rec.Code != 400 {
		t.Fatalf("bad vm id: %d", rec.Code)
	}
	if o.vl.received()+o.vm.received() != 0 {
		t.Fatal("forwarded")
	}
}

func TestIngestVMBuilder(t *testing.T) {
	o := newOTLPEnv(t)
	v, err := o.store.CreateVM(context.Background(), store.VM{OrgID: o.org, NodeID: uuid.MustParse(o.node), UserID: &o.alice.ID, Name: "builder", Kind: store.VMKindBuilder,
		CPUs: 1, MemoryMB: 512, DiskGB: 10, CreatedBy: o.alice.ID})
	if err != nil {
		t.Fatal(err)
	}
	o.requireVMForbidden(v.ID.String())
}

func TestIngestVMQuarantinedFirst(t *testing.T) {
	o := newOTLPEnv(t)
	vm := o.createVM(o.org, o.node, "vm01")
	if err := o.net.SetStatus(context.Background(), proto.GatewayStatus{Quarantined: []string{o.node}}); err != nil {
		t.Fatal(err)
	}
	rec := o.postVM(logsPath, o.credential, protobuf, "", vm, "app", logsBody(t))
	if rec.Code != 403 || !strings.Contains(rec.Body.String(), "node quarantined") {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}
	if o.vl.received() != 0 {
		t.Fatal("forwarded")
	}
}

package telemetry

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

var (
	testOrg  = uuid.MustParse("11111111-1111-1111-1111-111111111111")
	testNode = uuid.MustParse("22222222-2222-2222-2222-222222222222")
)

func TestUsageStep(t *testing.T) {
	now := time.Now()
	for _, c := range []struct {
		span time.Duration
		want time.Duration
	}{{time.Hour, 12 * time.Second}, {6 * time.Hour, 72 * time.Second}, {7 * 24 * time.Hour, 2016 * time.Second}, {5 * time.Minute, 10 * time.Second}} {
		if got := usageStep(now.Add(-c.span), now); got != c.want {
			t.Errorf("%v: got %v want %v", c.span, got, c.want)
		}
	}
}

// splitQuoted returns the unquoted parts of a LogsQL query and the unquoted values of its string literals.
func splitQuoted(t *testing.T, q string) (bare string, literals []string) {
	t.Helper()
	for {
		i := strings.IndexByte(q, '"')
		if i < 0 {
			return bare + q, literals
		}
		bare += q[:i]
		lit, err := strconv.QuotedPrefix(q[i:])
		if err != nil {
			t.Fatalf("unterminated literal in %q: %v", q, err)
		}
		v, _ := strconv.Unquote(lit)
		literals = append(literals, v)
		q = q[i+len(lit):]
	}
}

func TestLogsQLQuotesUserInput(t *testing.T) {
	text := "a\" OR * | delete\nnext"
	q := buildLogsQL(LogQuery{Text: text, Fields: map[string]string{"vm": "x\"y", "k | delete": "v"}})
	bare, lits := splitQuoted(t, q)
	for _, want := range []string{text, "x\"y", "k | delete", "v"} {
		if !slices.Contains(lits, want) {
			t.Errorf("%q not a literal in %s", want, q)
		}
	}
	if strings.Contains(bare, "|") || strings.Contains(bare, "OR") || strings.Contains(bare, "delete") || strings.Contains(bare, "\n") {
		t.Errorf("user input outside literals: %q (query %s)", bare, q)
	}
}

func TestLogsQLFilters(t *testing.T) {
	from := time.Date(2026, 11, 20, 10, 0, 0, 0, time.UTC)
	to := from.Add(time.Hour)
	q := buildLogsQL(LogQuery{From: from, To: to, Level: "error", Source: "firecracker", Fields: map[string]string{"vm": "vm01"}})
	want := `_time:[2026-11-20T10:00:00Z, 2026-11-20T11:00:00Z] "severity_text":="ERROR" source:="firecracker" "vm":="vm01"`
	if q != want {
		t.Fatalf("got  %s\nwant %s", q, want)
	}
	if q := buildLogsQL(LogQuery{Level: "ERROR"}); q != "*" {
		t.Fatalf("unknown level must add no filter: %q", q)
	}
	if q := buildLogsQL(LogQuery{}); q != "*" {
		t.Fatalf("empty: %q", q)
	}
	if q := buildLogsQL(LogQuery{Text: "connection reset"}); q != `*:"connection reset"` {
		t.Fatalf("text: %q", q)
	}
}

func TestLogsQLRejectsReservedField(t *testing.T) {
	q := buildLogsQL(LogQuery{Fields: map[string]string{"observatory.org": "other", "observatory.node": "other"}})
	if q != "*" {
		t.Fatalf("got %q", q)
	}
}

func TestNodeUsageQueries(t *testing.T) {
	var queries []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		if r.URL.Path != "/api/v1/query_range" || r.Form.Get("extra_label") != "observatory_org="+testOrg.String() || r.Form.Get("step") != "12s" {
			t.Errorf("bad request %s %v", r.URL.Path, r.Form)
		}
		q := r.Form.Get("query")
		queries = append(queries, q)
		switch {
		case strings.Contains(q, "system_memory_usage_bytes"):
			_, _ = w.Write([]byte(`{"status":"success","data":{"resultType":"matrix","result":[
				{"metric":{"state":"used"},"values":[[100,"1"],[112,"2"]]},
				{"metric":{"state":"cached"},"values":[[112,"3"],[124,"NaN"]]}]}}`))
		default:
			_, _ = w.Write([]byte(`{"status":"success","data":{"resultType":"matrix","result":[]}}`))
		}
	}))
	defer srv.Close()
	c := &Client{MetricsURL: srv.URL}
	to := time.Unix(3700, 0)
	u, err := c.NodeUsage(context.Background(), testOrg, testNode, to.Add(-time.Hour), to)
	if err != nil {
		t.Fatal(err)
	}
	if len(queries) != 4 {
		t.Fatalf("queries: %v", queries)
	}
	for _, q := range queries {
		if !strings.Contains(q, `observatory_node="`+testNode.String()+`"`) {
			t.Errorf("unscoped query %s", q)
		}
	}
	if !slices.Equal(u.T, []int64{100, 112}) {
		t.Fatalf("T: %v", u.T)
	}
	got, _ := json.Marshal(u.Charts["mem"])
	if want := `[{"name":"used","values":[1,2]},{"name":"cached","values":[null,3]}]`; string(got) != want {
		t.Fatalf("mem: %s", got)
	}
	for _, k := range []string{"cpu", "disk", "net"} {
		if s, ok := u.Charts[k]; !ok || len(s) != 0 {
			t.Errorf("%s: %v %v", k, s, ok)
		}
	}
}

func TestLogsScoped(t *testing.T) {
	var form map[string][]string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		form = r.Form
		if r.URL.Path != "/select/logsql/query" {
			t.Errorf("path %s", r.URL.Path)
		}
		_, _ = w.Write([]byte(`{"_msg":"older","_time":"2026-11-20T10:00:00Z","_stream":"{}","_stream_id":"x","severity_text":"WARN","source":"firecracker","vm":"vm01"}
{"_msg":"image pull failed","_time":"2026-11-20T10:00:01.5Z","severity_text":"ERROR","source":"observatory-node","err":"connection reset"}
`))
	}))
	defer srv.Close()
	c := &Client{LogsURL: srv.URL}
	lines, err := c.Logs(context.Background(), LogQuery{Org: testOrg, Node: testNode, Text: "x"})
	if err != nil {
		t.Fatal(err)
	}
	wantFilter := `observatory.org:="` + testOrg.String() + `" observatory.node:="` + testNode.String() + `" observatory.vm:=""`
	if f := form["extra_filters"]; len(f) != 1 || f[0] != wantFilter {
		t.Fatalf("extra_filters %v", f)
	}
	if form["query"][0] != `*:"x"` || form["limit"][0] != "500" {
		t.Fatalf("form %v", form)
	}
	got, _ := json.Marshal(lines)
	want := `[{"time":"2026-11-20T10:00:01.5Z","level":"error","source":"observatory-node","msg":"image pull failed","fields":{"err":"connection reset"}},` +
		`{"time":"2026-11-20T10:00:00Z","level":"warn","source":"firecracker","msg":"older","fields":{"vm":"vm01"}}]`
	if string(got) != want {
		t.Fatalf("got  %s\nwant %s", got, want)
	}
}

func TestTailLogs(t *testing.T) {
	var path, filters string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		path, filters = r.URL.Path, r.Form.Get("extra_filters")
		_, _ = w.Write([]byte(`{"_msg":"a","_time":"2026-11-20T10:00:00Z"}` + "\n" + `{"_msg":"b","_time":"2026-11-20T10:00:01Z"}` + "\n"))
	}))
	defer srv.Close()
	c := &Client{LogsURL: srv.URL}
	var msgs []string
	err := c.TailLogs(context.Background(), LogQuery{Org: testOrg, Node: testNode}, func(l LogLine) error {
		msgs = append(msgs, l.Msg)
		return nil
	})
	if err != nil || path != "/select/logsql/tail" || !strings.Contains(filters, testOrg.String()) || !slices.Equal(msgs, []string{"a", "b"}) {
		t.Fatalf("%v %s %s %v", err, path, filters, msgs)
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

type closeTracker struct {
	io.Reader
	closed bool
}

func (b *closeTracker) Close() error { b.closed = true; return nil }

func TestLogsCanceledClosesBody(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	body := &closeTracker{Reader: strings.NewReader("")}
	c := &Client{LogsURL: "http://store", HTTP: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		cancel()
		return &http.Response{StatusCode: http.StatusOK, Body: body, Request: r}, nil
	})}}
	if _, err := c.Logs(ctx, LogQuery{Org: testOrg, Node: testNode}); err != context.Canceled {
		t.Fatalf("err = %v", err)
	}
	if !body.closed {
		t.Fatal("body not closed")
	}
}

func TestNodeQueriesExcludeVMs(t *testing.T) {
	selector := regexp.MustCompile(`[a-z_]+\{[^}]*\}`)
	for chart, nq := range nodeQueries {
		sels := selector.FindAllString(nq.expr, -1)
		if len(sels) == 0 {
			t.Fatalf("%s: no selectors", chart)
		}
		for _, s := range sels {
			if !strings.Contains(s, `observatory_vm=""`) {
				t.Errorf("%s: selector %s includes VM series", chart, s)
			}
		}
	}
	var filters string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		filters = r.Form.Get("extra_filters")
	}))
	defer srv.Close()
	c := &Client{LogsURL: srv.URL}
	if _, err := c.Logs(context.Background(), LogQuery{Org: testOrg, Node: testNode}); err != nil {
		t.Fatal(err)
	}
	// Verified against VictoriaLogs v1.54.0: `field:=""` matches records where the field is absent or empty.
	if !strings.HasSuffix(filters, ` observatory.vm:=""`) {
		t.Fatalf("extra_filters %q", filters)
	}
}

var testVM = uuid.MustParse("33333333-3333-3333-3333-333333333333")

func TestVMUsageQueries(t *testing.T) {
	var queries []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		if r.Form.Get("extra_label") != "observatory_org="+testOrg.String() {
			t.Errorf("bad request %v", r.Form)
		}
		queries = append(queries, r.Form.Get("query"))
		_, _ = w.Write([]byte(`{"status":"success","data":{"resultType":"matrix","result":[]}}`))
	}))
	defer srv.Close()
	c := &Client{MetricsURL: srv.URL}
	to := time.Unix(3700, 0)
	if _, err := c.VMUsage(context.Background(), testOrg, testVM, to.Add(-time.Hour), to); err != nil {
		t.Fatal(err)
	}
	if len(queries) != 4 {
		t.Fatalf("queries: %v", queries)
	}
	for _, q := range queries {
		if !strings.Contains(q, `observatory_vm="`+testVM.String()+`"`) || strings.Contains(q, "observatory_node") || strings.Contains(q, `observatory_vm=""`) {
			t.Errorf("bad scope %s", q)
		}
	}
}

func TestVMLogsScoped(t *testing.T) {
	var form map[string][]string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		form = r.Form
		_, _ = w.Write([]byte(`{"_msg":"hi","_time":"2026-11-20T10:00:00Z","severity_text":"INFO","observatory.stream":"service"}` + "\n"))
	}))
	defer srv.Close()
	c := &Client{LogsURL: srv.URL}
	lines, err := c.Logs(context.Background(), LogQuery{Org: testOrg, VM: testVM, Stream: "service"})
	if err != nil {
		t.Fatal(err)
	}
	wantFilter := `observatory.org:="` + testOrg.String() + `" observatory.vm:="` + testVM.String() + `"`
	if f := form["extra_filters"]; len(f) != 1 || f[0] != wantFilter {
		t.Fatalf("extra_filters %v", f)
	}
	if form["query"][0] != `"observatory.stream":="service"` {
		t.Fatalf("query %v", form["query"])
	}
	if len(lines) != 1 || lines[0].Stream != "service" || len(lines[0].Fields) != 0 {
		t.Fatalf("lines %+v", lines)
	}
}

func TestLogQueryNeedsOneScope(t *testing.T) {
	c := &Client{LogsURL: "http://unused.invalid"}
	for _, q := range []LogQuery{{Org: testOrg}, {Org: testOrg, Node: testNode, VM: testVM}} {
		if _, err := c.Logs(context.Background(), q); err == nil {
			t.Errorf("%+v: want error", q)
		}
	}
}

func TestVMLatestBatch(t *testing.T) {
	other := uuid.MustParse("44444444-4444-4444-4444-444444444444")
	var queries []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		if r.URL.Path != "/api/v1/query" || r.Form.Get("extra_label") != "observatory_org="+testOrg.String() {
			t.Errorf("bad request %s %v", r.URL.Path, r.Form)
		}
		q := r.Form.Get("query")
		queries = append(queries, q)
		val := func(v string) string {
			return `{"status":"success","data":{"result":[{"metric":{"observatory_vm":"` + testVM.String() + `"},"value":[1700000000,"` + v + `"]}]}}`
		}
		switch {
		case strings.Contains(q, "timestamp("):
			_, _ = w.Write([]byte(val("1699999990")))
		case strings.Contains(q, "cpu_time"):
			_, _ = w.Write([]byte(val("12.5")))
		case strings.Contains(q, "used|cached|free"):
			_, _ = w.Write([]byte(val("2000")))
		case strings.Contains(q, "memory_usage"):
			_, _ = w.Write([]byte(val("500")))
		case strings.Contains(q, "filesystem_usage"):
			_, _ = w.Write([]byte(val("70")))
		default:
			_, _ = w.Write([]byte(val("100")))
		}
	}))
	defer srv.Close()
	c := &Client{MetricsURL: srv.URL}
	got, err := c.VMLatest(context.Background(), testOrg, []uuid.UUID{testVM, other})
	if err != nil {
		t.Fatal(err)
	}
	if len(queries) != 6 {
		t.Fatalf("queries: %v", queries)
	}
	for _, q := range queries {
		if !strings.Contains(q, `observatory_vm=~"`+testVM.String()+`|`+other.String()+`"`) {
			t.Errorf("unscoped %s", q)
		}
	}
	want := VMNow{At: time.Unix(1699999990, 0), CPUPercent: 12.5, MemUsedBytes: 500, MemTotalBytes: 2000, DiskUsedBytes: 70, DiskTotalBytes: 100}
	if len(got) != 1 || got[testVM] != want {
		t.Fatalf("got %+v", got)
	}
}

func TestVMDrops(t *testing.T) {
	var form map[string][]string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		form = r.Form
		_, _ = w.Write([]byte(`{"status":"success","data":{"result":[{"metric":{"kind":"logs"},"value":[1,"41.9"]},{"metric":{"kind":"body"},"value":[1,"2"]}]}}`))
	}))
	defer srv.Close()
	c := &Client{MetricsURL: srv.URL}
	to := time.Unix(3700, 0)
	got, err := c.VMDrops(context.Background(), testOrg, testVM, to.Add(-time.Hour), to)
	if err != nil {
		t.Fatal(err)
	}
	if got["logs"] != 42 || got["body"] != 2 || len(got) != 2 {
		t.Fatalf("got %v", got)
	}
	if q := form["query"][0]; !strings.Contains(q, `increase_pure(observatory_vm_telemetry_dropped_total{vm="`+testVM.String()+`",observatory_vm="",observatory_node!=""}[3600s])`) || form["extra_label"][0] != "observatory_org="+testOrg.String() {
		t.Fatalf("form %v", form)
	}
}

package api

import (
	"bufio"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/asterism-dev/observatory/internal/telemetry"
)

var logsEndpoints = []string{"query", "tail", "hits", "field_names", "field_values", "stream_field_names", "stream_field_values", "streams", "stream_ids", "stats_query", "stats_query_range", "facets"}

func newLogsEnv(t *testing.T) *promEnv {
	p := newPromEnv(t)
	p.api.telemetry = &telemetry.Client{LogsURL: p.vm.URL}
	return p
}

func onlyLogsOrgScope(t *testing.T, v url.Values, org uuid.UUID) {
	t.Helper()
	for k := range v {
		if l := strings.ToLower(k); l == "extra_stream_filters" || (l == "extra_filters" && k != "extra_filters") {
			t.Fatalf("client scope param %q forwarded: %v", k, v)
		}
	}
	if got := v["extra_filters"]; len(got) != 1 || got[0] != `observatory.org:="`+org.String()+`"` {
		t.Fatalf("extra_filters: %q", got)
	}
}

const smuggledLogs = "extra_filters=observatory.org%3A%3DB&EXTRA_FILTERS=x&extra%5Ffilters=y&extra_stream_filters=%7Bobservatory.org%3D%22B%22%7D&Extra_Stream_Filters=z&extra%5Fstream%5Ffilters=w"

func TestLogsProxyStripsScopeParams(t *testing.T) {
	p := newLogsEnv(t)

	rec := p.prom("GET", "/grafana/logs/select/logsql/query?query=error&"+smuggledLogs, p.token, "")
	if rec.Code != 200 {
		t.Fatalf("get: %d %s", rec.Code, rec.Body.String())
	}
	q := p.forwarded(0)
	onlyLogsOrgScope(t, q, p.org)
	if q.Get("query") != "error" || p.vm.paths[0] != "/select/logsql/query" {
		t.Fatalf("forwarded %s ? %v", p.vm.paths[0], q)
	}

	rec = p.prom("POST", "/grafana/logs/select/logsql/hits?"+smuggledLogs, p.token, "query=*&step=1h&"+smuggledLogs)
	if rec.Code != 200 {
		t.Fatalf("post: %d %s", rec.Code, rec.Body.String())
	}
	form := p.forwarded(1)
	onlyLogsOrgScope(t, form, p.org)
	if form.Get("query") != "*" || form.Get("step") != "1h" || p.vm.paths[1] != "/select/logsql/hits" {
		t.Fatalf("post forwarded: %v", form)
	}
}

func TestLogsProxyPathAllowlist(t *testing.T) {
	p := newLogsEnv(t)
	for _, ep := range logsEndpoints {
		for _, method := range []string{"GET", "POST"} {
			if rec := p.prom(method, "/grafana/logs/select/logsql/"+ep, p.token, ""); rec.Code != 200 {
				t.Fatalf("%s %s: %d %s", method, ep, rec.Code, rec.Body.String())
			}
			if got := p.vm.paths[len(p.vm.paths)-1]; got != "/select/logsql/"+ep {
				t.Fatalf("%s forwarded to %s", ep, got)
			}
		}
	}
	hits := p.vm.received()
	for _, target := range []string{
		"/grafana/logs/select/logsql/../../insert/jsonline",
		"/grafana/logs/select/logsql/query/../../../delete/run_task",
		"/grafana/logs/select/logsql/./query",
		"/grafana/logs/select/logsql//query",
		"/grafana/logs//select/logsql/query",
		"/grafana//logs/select/logsql/query",
		"/grafana/logs/select/logsql/query/",
		"/grafana/logs/select/logsql/queryx",
		"/grafana/logs/select/logsql/query%2F..%2F..%2F..%2Finsert%2Fjsonline",
		"/grafana/logs/select%2Flogsql/query",
		"/grafana%2Flogs/select/logsql/query",
		"/grafana/logs/select/logsql/qu%65ry",
		"/grafana/logs/select/logsql/",
		"/grafana/logs/select/logsql/tenant_ids",
		"/grafana/logs/select/logsql/query_time_series",
		"/grafana/logs/insert/jsonline",
		"/grafana/logs/insert/opentelemetry/v1/logs",
		"/grafana/logs/delete/run_task",
		"/grafana/logs/internal/force_flush",
		"/grafana/logs/metrics",
		"/grafana/logs/select/vmui",
		"/grafana/logs/",
		"/grafana/logs",
		"/grafana/logs/api/v1/query",
	} {
		if rec := p.prom("GET", target, p.token, ""); rec.Code != 404 {
			t.Fatalf("%s: %d", target, rec.Code)
		}
	}
	for _, method := range []string{"PUT", "DELETE", "PATCH", "OPTIONS"} {
		if rec := p.prom(method, "/grafana/logs/select/logsql/query", p.token, ""); rec.Code != 404 {
			t.Fatalf("%s: %d", method, rec.Code)
		}
	}
	if p.vm.received() != hits {
		t.Fatal("rejected request reached the store")
	}
	if rec := p.prom("GET", "/grafana/logs/select/logsql/query", "", ""); rec.Code != 401 {
		t.Fatalf("no token: %d", rec.Code)
	}
}

func TestLogsProxyTenantHeadersStripped(t *testing.T) {
	p := newLogsEnv(t)
	req := httptest.NewRequest("GET", "/grafana/logs/select/logsql/query?query=*", nil)
	req.Header.Set("Authorization", "Bearer "+p.token)
	req.Header.Set("AccountID", "7")
	req.Header.Set("ProjectID", "8")
	rec := httptest.NewRecorder()
	p.h.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}
	for _, h := range []string{"AccountID", "ProjectID", "Authorization"} {
		if v := p.vm.header.Get(h); v != "" {
			t.Fatalf("%s forwarded: %q", h, v)
		}
	}
}

// tailUpstream emits a row every 20 ms until the proxy hangs up.
func tailUpstream(t *testing.T) *httptest.Server {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/stream+json")
		for i := 0; ; i++ {
			if _, err := fmt.Fprintf(w, "{\"_msg\":\"row %d\"}\n", i); err != nil {
				return
			}
			w.(http.Flusher).Flush()
			select {
			case <-r.Context().Done():
				return
			case <-time.After(20 * time.Millisecond):
			}
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func (p *promEnv) openTail(t *testing.T) (*http.Response, *bufio.Reader) {
	hub := httptest.NewUnstartedServer(p.h)
	hub.Config.WriteTimeout = 100 * time.Millisecond
	hub.Start()
	t.Cleanup(hub.Close)
	req, _ := http.NewRequest("GET", hub.URL+"/grafana/logs/select/logsql/tail?query=*", nil)
	req.Header.Set("Authorization", "Bearer "+p.token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { resp.Body.Close() })
	if resp.StatusCode != 200 {
		t.Fatalf("tail: %d", resp.StatusCode)
	}
	return resp, bufio.NewReader(resp.Body)
}

func TestLogsProxyTail(t *testing.T) {
	p := newLogsEnv(t)
	p.api.telemetry = &telemetry.Client{LogsURL: tailUpstream(t).URL}
	p.api.grafanaTailMax = 600 * time.Millisecond

	start := time.Now()
	resp, body := p.openTail(t)
	if ct := resp.Header.Get("Content-Type"); ct != "application/stream+json" {
		t.Fatalf("content type %q", ct)
	}
	// The first row arrives long before the cap, so it was flushed rather than buffered.
	if line, err := body.ReadString('\n'); err != nil || line != "{\"_msg\":\"row 0\"}\n" || time.Since(start) > 300*time.Millisecond {
		t.Fatalf("first row %q %v after %s", line, err, time.Since(start))
	}
	rest, _ := io.ReadAll(body)
	took := time.Since(start)
	// Rows past the server's 100 ms write timeout prove the tail replaced that deadline.
	if n := strings.Count(string(rest), "\n"); n < 10 || took < 600*time.Millisecond || took > 3*time.Second {
		t.Fatalf("%d more rows, stream ended after %s", n, took)
	}
}

func TestLogsProxyTailEndsOnRevoke(t *testing.T) {
	p := newLogsEnv(t)
	p.api.telemetry = &telemetry.Client{LogsURL: tailUpstream(t).URL}
	p.api.grafanaTailRecheck = 50 * time.Millisecond
	tok := p.createTelemetryToken(p.tokSes, p.org, "revoked")
	p.token = tok.Token

	_, body := p.openTail(t)
	if _, err := body.ReadString('\n'); err != nil {
		t.Fatal(err)
	}
	if rec := p.do("DELETE", tokensPath(p.org)+"/"+tok.ID, p.tokSes); rec.Code != 204 {
		t.Fatalf("revoke: %d %s", rec.Code, rec.Body.String())
	}
	revoked := time.Now()
	_, _ = io.ReadAll(body)
	if took := time.Since(revoked); took > 2*time.Second {
		t.Fatalf("tail outlived its token by %s", took)
	}
}

type deadlineRecorder struct {
	*httptest.ResponseRecorder
	mu        sync.Mutex
	deadlines []time.Time
}

func (d *deadlineRecorder) SetWriteDeadline(t time.Time) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.deadlines = append(d.deadlines, t)
	return nil
}

func (d *deadlineRecorder) last() (time.Time, int) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if len(d.deadlines) == 0 {
		return time.Time{}, 0
	}
	return d.deadlines[len(d.deadlines)-1], len(d.deadlines)
}

func (p *promEnv) serveTail(w http.ResponseWriter) {
	req := httptest.NewRequest("GET", "/grafana/logs/select/logsql/tail?query=*", nil)
	req.Header.Set("Authorization", "Bearer "+p.token)
	p.h.ServeHTTP(w, req)
}

func TestLogsProxyTailWriteDeadline(t *testing.T) {
	p := newLogsEnv(t)
	p.api.telemetry = &telemetry.Client{LogsURL: tailUpstream(t).URL}
	p.api.grafanaTailMax = 300 * time.Millisecond
	rec := &deadlineRecorder{ResponseRecorder: httptest.NewRecorder()}
	start := time.Now()
	p.serveTail(rec)
	if d, _ := rec.last(); d.Before(start.Add(5300*time.Millisecond)) || d.After(time.Now().Add(5300*time.Millisecond)) {
		t.Fatalf("tail write deadline %s, want start + tail max + 5 s (%s)", d, start.Add(5300*time.Millisecond))
	}
}

func TestLogsProxyTailRevokeCutsWriteDeadline(t *testing.T) {
	p := newLogsEnv(t)
	p.api.telemetry = &telemetry.Client{LogsURL: tailUpstream(t).URL}
	p.api.grafanaTailRecheck = 50 * time.Millisecond
	tok := p.createTelemetryToken(p.tokSes, p.org, "revoked")
	p.token = tok.Token
	rec := &deadlineRecorder{ResponseRecorder: httptest.NewRecorder()}
	done := make(chan struct{})
	go func() { p.serveTail(rec); close(done) }()
	// grafanaRequest sets the first deadline, the tail the second.
	for _, n := rec.last(); n < 2; _, n = rec.last() {
		time.Sleep(5 * time.Millisecond)
	}
	if rec := p.do("DELETE", tokensPath(p.org)+"/"+tok.ID, p.tokSes); rec.Code != 204 {
		t.Fatalf("revoke: %d %s", rec.Code, rec.Body.String())
	}
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("tail outlived its token")
	}
	if d, _ := rec.last(); d.After(time.Now()) {
		t.Fatalf("write deadline %s still in the future after revoke", d)
	}
}

func TestLogsProxyDenyList(t *testing.T) {
	p := newLogsEnv(t)
	if rec := p.prom("GET", "/grafana/logs/select/logsql/query?query="+url.QueryEscape("a | union (b)"), p.token, ""); rec.Code != 200 {
		t.Fatalf("empty deny list: %d", rec.Code)
	}
	defer func(l []string) { telemetry.LeakingLogsQL = l }(telemetry.LeakingLogsQL)
	telemetry.LeakingLogsQL = []string{"union", "_stream_id:in"}
	hits := p.vm.received()
	for _, q := range []string{
		"a | union (b)",
		"a|UNION(b)",
		"a | Union\t(b)",
		"_stream_id:in(b | fields _stream_id)",
		"x or _stream_id : IN (b | fields _stream_id)",
		// The scan is not a parser, so quoted text counts too.
		`"a | union (b)"`,
		"`union`",
	} {
		if rec := p.prom("GET", "/grafana/logs/select/logsql/query?query="+url.QueryEscape(q), p.token, ""); rec.Code != 400 {
			t.Fatalf("%q: %d", q, rec.Code)
		}
		if rec := p.prom("POST", "/grafana/logs/select/logsql/stats_query", p.token, url.Values{"query": {q}}.Encode()); rec.Code != 400 {
			t.Fatalf("post %q: %d", q, rec.Code)
		}
	}
	if p.vm.received() != hits {
		t.Fatal("denied query reached the store")
	}
	for _, q := range []string{"unions", "reunion", "union_x", "_stream_id:abc", "_stream_id.in", "in(x)"} {
		if rec := p.prom("GET", "/grafana/logs/select/logsql/query?query="+url.QueryEscape(q), p.token, ""); rec.Code != 200 {
			t.Fatalf("%q: %d %s", q, rec.Code, rec.Body.String())
		}
	}
}

// TestLogsProxyNoCrossOrg runs the probe's constructs through the proxy against a real VictoriaLogs holding two orgs.
func TestLogsProxyNoCrossOrg(t *testing.T) {
	logsURL := os.Getenv("OBSERVATORY_TEST_LOGS_URL")
	if logsURL == "" {
		t.Skip("OBSERVATORY_TEST_LOGS_URL not set")
	}
	p := newPromEnv(t)
	p.api.telemetry = &telemetry.Client{LogsURL: logsURL}
	_, bobTok := p.user("bob")
	orgB := p.createOrg(bobTok, "Other")
	run := strings.ReplaceAll(uuid.NewString(), "-", "")[:12]
	mA, mB := "gfa"+run, "gfb"+run

	var lines strings.Builder
	for i := range 3 {
		for _, org := range []uuid.UUID{p.org, orgB} {
			m, extra := mA, ""
			if org == orgB {
				m, extra = mB, fmt.Sprintf(",%q:%q", mB, mB)
			}
			fmt.Fprintf(&lines, `{"_msg":"%s shared%s line %d","_time":%q,"observatory.org":%q,"probe":%q,"k":"shared%s"%s}`+"\n",
				m, run, i, time.Now().UTC().Format(time.RFC3339Nano), org, run, run, extra)
		}
	}
	resp, err := http.Post(logsURL+"/insert/jsonline?_stream_fields=observatory.org,probe,"+mB, "application/stream+json", strings.NewReader(lines.String()))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode >= 300 {
		t.Fatalf("seed: %d", resp.StatusCode)
	}

	from, to := time.Now().Add(-time.Hour).Format(time.RFC3339), time.Now().Add(5*time.Minute).Format(time.RFC3339)
	call := func(ep, q string, extra url.Values) (int, string) {
		t.Helper()
		form := url.Values{"query": {q}}
		switch ep {
		case "tail":
			form.Set("start_offset", "1h")
		case "stats_query", "stats_query_range":
			form.Set("query", q+" | stats by (observatory.org, "+mB+") count() c")
			form.Set("time", to)
			form.Set("step", "1h")
		case "hits":
			form.Set("step", "1h")
			form["field"] = []string{"observatory.org", mB}
		case "field_values":
			form.Set("field", mB)
		case "stream_field_values":
			form.Set("field", "observatory.org")
		case "facets":
			form.Set("keep_const_fields", "1")
		}
		if ep != "tail" && ep != "stats_query" {
			form.Set("start", from)
			form.Set("end", to)
		}
		for k, v := range extra {
			form[k] = v
		}
		req := httptest.NewRequest("POST", "/grafana/logs/select/logsql/"+ep, strings.NewReader(form.Encode()))
		req.Header.Set("Authorization", "Bearer "+p.token)
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.Header.Set("AccountID", "0")
		rec := httptest.NewRecorder()
		p.h.ServeHTTP(rec, req)
		return rec.Code, rec.Body.String()
	}

	deadline := time.Now().Add(30 * time.Second)
	for {
		// Wait on the store itself so both orgs' rows are queryable before asserting the proxy hides one.
		resp, err := http.PostForm(logsURL+"/select/logsql/query", url.Values{"query": {"probe:=" + run}})
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if strings.Count(string(body), "\n") == 6 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("seeded logs never visible")
		}
		time.Sleep(500 * time.Millisecond)
	}
	if _, body := call("query", "probe:="+run, nil); strings.Count(body, "\n") != 3 || !strings.Contains(body, mA) {
		t.Errorf("own logs through the proxy: %s", body)
	}

	p.api.grafanaTailMax = 2 * time.Second
	smuggle := url.Values{"extra_filters": {fmt.Sprintf("observatory.org:=%q", orgB)}, "extra_stream_filters": {fmt.Sprintf("{observatory.org=%q}", orgB)}}
	// oracle queries return org A rows only if their subquery saw org B.
	queries := []struct {
		q      string
		oracle bool
	}{
		{mB, false},
		{"*", false},
		{fmt.Sprintf("observatory.org:=%q", orgB), false},
		{fmt.Sprintf("_stream:{observatory.org=%q}", orgB), false},
		{mA + " or " + mB, false},
		{"options(ignore_global_time_filter=true) " + mB, false},
		{fmt.Sprintf("options(global_filter=(observatory.org:=%q)) %s", orgB, mB), false},
		{fmt.Sprintf("%s k:in(%s | fields k)", mA, mB), true},
		{fmt.Sprintf("_stream_id:in(%s | fields _stream_id)", mB), false},
		{fmt.Sprintf("%s | filter k:in(%s | fields k)", mA, mB), true},
		{fmt.Sprintf("%s | union (%s)", mA, mB), false},
		{fmt.Sprintf("%s | union (options(ignore_global_time_filter=true) %s | stream_context before 10 after 10)", mA, mB), false},
		{fmt.Sprintf("%s | join by (probe) (%s | fields probe, observatory.org, %s) inner", mA, mB, mB), false},
		{fmt.Sprintf("%s | union (%s | join by (probe) (%s | fields probe, %s))", mA, mB, mB, mB), false},
		{fmt.Sprintf("%s k:in(probe:=%s k:in(%s | fields k) | fields k)", mA, run, mB), true},
		{"probe:=" + run + " | stream_context before 10 after 10", false},
		{"probe:=" + run + " | facets", false},
		{"probe:=" + run + " | field_values observatory.org", false},
		{"probe:=" + run + " | uniq by (observatory.org)", false},
	}
	for _, c := range queries {
		// Org B's private field name is echoed in group-by results, so only its value position counts.
		q, markers := c.q, []string{`":"` + mB, orgB.String()}
		if c.oracle {
			markers = append(markers, mA, p.org.String())
		}
		for _, ep := range logsEndpoints {
			for _, extra := range []url.Values{nil, smuggle} {
				status, body := call(ep, q, extra)
				leaked := slices.ContainsFunc(markers, func(m string) bool {
					// Error bodies echo the query, so only markers the query lacks count there.
					return strings.Contains(body, m) && (status == 200 || !strings.Contains(q, m))
				})
				if leaked {
					t.Errorf("%s %q (smuggle=%v) leaks org B: %d %.300s", ep, q, extra != nil, status, body)
				}
			}
		}
	}
}

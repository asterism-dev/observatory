package api

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/asterism-dev/observatory/internal/telemetry"
)

type promEnv struct {
	*env
	vm     *fakeStore
	org    uuid.UUID
	token  string
	tokSes string
}

func newPromEnv(t *testing.T) *promEnv {
	e := newEnv(t)
	_, tok := e.user("alice")
	org := e.createOrg(tok, "Acme")
	p := &promEnv{env: e, vm: newFakeStore(t), org: org, tokSes: tok}
	p.token = e.createTelemetryToken(tok, org, "grafana").Token
	e.api.telemetry = &telemetry.Client{MetricsURL: p.vm.URL}
	return p
}

func (p *promEnv) prom(method, target, token, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, target, strings.NewReader(body))
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	if method == "POST" {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	rec := httptest.NewRecorder()
	p.h.ServeHTTP(rec, req)
	return rec
}

func (p *promEnv) forwarded(i int) url.Values {
	p.t.Helper()
	q, err := url.ParseQuery(p.vm.queries[i])
	form, err2 := url.ParseQuery(string(p.vm.bodies[i]))
	if err != nil || err2 != nil {
		p.t.Fatal(err, err2)
	}
	for k, v := range q {
		form[k] = append(form[k], v...)
	}
	return form
}

func onlyOrgScope(t *testing.T, v url.Values, org uuid.UUID) {
	t.Helper()
	for k := range v {
		if l := strings.ToLower(k); l == "extra_filters" || l == "extra_filters[]" || (l == "extra_label" && k != "extra_label") {
			t.Fatalf("client scope param %q forwarded: %v", k, v)
		}
	}
	if got := v["extra_label"]; len(got) != 1 || got[0] != "observatory_org="+org.String() {
		t.Fatalf("extra_label: %q", got)
	}
}

const smuggled = "extra_label=observatory_org%3DB&EXTRA_LABEL=x&extra%5Flabel=y&Extra_Filters=z&extra_filters[]=w&extra_filters%5B%5D=v&eXtra_filters%5b%5d=u&extra_label=again"

func TestPromProxyStripsScopeParams(t *testing.T) {
	p := newPromEnv(t)

	rec := p.prom("GET", "/grafana/prometheus/api/v1/query?query=up&"+smuggled, p.token, "")
	if rec.Code != 200 {
		t.Fatalf("get: %d %s", rec.Code, rec.Body.String())
	}
	q := p.forwarded(0)
	onlyOrgScope(t, q, p.org)
	if q.Get("query") != "up" || p.vm.paths[0] != "/api/v1/query" {
		t.Fatalf("forwarded %s ? %v", p.vm.paths[0], q)
	}
	if p.vm.header.Get("Authorization") != "" {
		t.Fatal("bearer token forwarded upstream")
	}

	rec = p.prom("POST", "/grafana/prometheus/api/v1/query_range?"+smuggled, p.token, "query=sum(up)&step=15&"+smuggled)
	if rec.Code != 200 {
		t.Fatalf("post: %d %s", rec.Code, rec.Body.String())
	}
	form := p.forwarded(1)
	onlyOrgScope(t, form, p.org)
	if form.Get("query") != "sum(up)" || form.Get("step") != "15" {
		t.Fatalf("post forwarded: %v", form)
	}
}

func TestPromProxyPathAllowlist(t *testing.T) {
	p := newPromEnv(t)
	allowed := map[string]string{
		"query": "/api/v1/query", "query_range": "/api/v1/query_range", "series": "/api/v1/series", "labels": "/api/v1/labels",
		"label/observatory_vm/values": "/api/v1/label/observatory_vm/values", "label/__name__/values": "/api/v1/label/__name__/values",
		"status/buildinfo": "/api/v1/status/buildinfo",
	}
	for suffix, upstream := range allowed {
		for _, method := range []string{"GET", "POST"} {
			if rec := p.prom(method, "/grafana/prometheus/api/v1/"+suffix, p.token, ""); rec.Code != 200 {
				t.Fatalf("%s %s: %d %s", method, suffix, rec.Code, rec.Body.String())
			}
			if got := p.vm.paths[len(p.vm.paths)-1]; got != upstream {
				t.Fatalf("%s forwarded to %s", suffix, got)
			}
		}
	}
	hits := p.vm.received()
	for _, target := range []string{
		"/grafana/prometheus/api/v1/../admin/tsdb/delete_series",
		"/grafana/prometheus/api/v1/query/../../admin/tsdb/delete_series",
		"/grafana/prometheus/api/v1/./query",
		"/grafana/prometheus/api/v1//query",
		"/grafana//prometheus/api/v1/query",
		"/grafana/prometheus/api/v1/query/",
		"/grafana/prometheus/api/v1/queryx",
		"/grafana/prometheus/api/v1/query%2F..%2F..%2Fadmin%2Ftsdb%2Fdelete_series",
		"/grafana/prometheus/api/v1%2Fquery",
		"/grafana%2Fprometheus/api/v1/query",
		"/grafana/prometheus/api/v1/qu%65ry",
		"/grafana/prometheus/api/v1/label/../values",
		"/grafana/prometheus/api/v1/label/a/b/values",
		"/grafana/prometheus/api/v1/label//values",
		"/grafana/prometheus/api/v1/status/tsdb",
		"/grafana/prometheus/api/v1/import",
		"/grafana/prometheus/api/v1/import/prometheus",
		"/grafana/prometheus/api/v1/export",
		"/grafana/prometheus/api/v1/write",
		"/grafana/prometheus/api/v1/admin/tsdb/delete_series",
		"/grafana/prometheus/snapshot/create",
		"/grafana/prometheus/",
		"/grafana/prometheus",
	} {
		if rec := p.prom("GET", target, p.token, ""); rec.Code != 404 {
			t.Fatalf("%s: %d", target, rec.Code)
		}
	}
	for _, method := range []string{"PUT", "DELETE", "PATCH", "OPTIONS"} {
		if rec := p.prom(method, "/grafana/prometheus/api/v1/query", p.token, ""); rec.Code != 404 {
			t.Fatalf("%s: %d", method, rec.Code)
		}
	}
	if p.vm.received() != hits {
		t.Fatal("rejected request reached the store")
	}
}

func TestPromProxyAuth(t *testing.T) {
	p := newPromEnv(t)
	if rec := p.prom("GET", "/grafana/prometheus/api/v1/query", "", ""); rec.Code != 401 {
		t.Fatalf("no token: %d", rec.Code)
	}
	if rec := p.prom("GET", "/grafana/prometheus/api/v1/query", p.tokSes, ""); rec.Code != 401 {
		t.Fatalf("session token: %d", rec.Code)
	}
	req := httptest.NewRequest("GET", "/grafana/prometheus/api/v1/query", nil)
	req.AddCookie(&http.Cookie{Name: "obs_session", Value: p.tokSes})
	rec := httptest.NewRecorder()
	p.h.ServeHTTP(rec, req)
	if rec.Code != 401 || rec.Header().Get("Access-Control-Allow-Origin") != "" {
		t.Fatalf("cookie: %d %v", rec.Code, rec.Header())
	}
	p.api.telemetry = nil
	if rec := p.prom("GET", "/grafana/prometheus/api/v1/query", p.token, ""); rec.Code != 404 {
		t.Fatalf("telemetry off: %d", rec.Code)
	}
}

func TestPromProxyMetadataStub(t *testing.T) {
	p := newPromEnv(t)
	for _, method := range []string{"GET", "POST"} {
		rec := p.prom(method, "/grafana/prometheus/api/v1/metadata?metric=up", p.token, "")
		if rec.Code != 200 || strings.TrimSpace(rec.Body.String()) != `{"status":"success","data":{}}` {
			t.Fatalf("%s: %d %s", method, rec.Code, rec.Body.String())
		}
	}
	if p.vm.received() != 0 {
		t.Fatal("metadata reached the store")
	}
}

func TestPromProxyRateLimit(t *testing.T) {
	p := newPromEnv(t)
	other := p.createTelemetryToken(p.tokSes, p.org, "other").Token
	now := time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC)
	p.api.now = func() time.Time { return now }
	for i := range 100 {
		if rec := p.prom("GET", "/grafana/prometheus/api/v1/query", p.token, ""); rec.Code != 200 {
			t.Fatalf("request %d: %d", i, rec.Code)
		}
	}
	if rec := p.prom("GET", "/grafana/prometheus/api/v1/query", p.token, ""); rec.Code != 429 {
		t.Fatalf("over burst: %d", rec.Code)
	}
	if rec := p.prom("GET", "/grafana/prometheus/api/v1/query", other, ""); rec.Code != 200 {
		t.Fatalf("other token: %d", rec.Code)
	}
	now = now.Add(time.Second)
	for i := range 20 {
		if rec := p.prom("GET", "/grafana/prometheus/api/v1/query", p.token, ""); rec.Code != 200 {
			t.Fatalf("refilled %d: %d", i, rec.Code)
		}
	}
	if rec := p.prom("GET", "/grafana/prometheus/api/v1/query", p.token, ""); rec.Code != 429 {
		t.Fatalf("after refill: %d", rec.Code)
	}

	now = now.Add(2 * time.Minute)
	p.api.grafanaLimit.allow(uuid.New(), now)
	if n := len(p.api.grafanaLimit.buckets); n != 1 {
		t.Fatalf("idle buckets kept: %d", n)
	}
}

func TestPromProxyBodyLimit(t *testing.T) {
	p := newPromEnv(t)
	fits := "query=" + strings.Repeat("a", 1<<20-len("query="))
	if rec := p.prom("POST", "/grafana/prometheus/api/v1/query", p.token, fits); rec.Code != 200 {
		t.Fatalf("1 MiB: %d", rec.Code)
	}
	if rec := p.prom("POST", "/grafana/prometheus/api/v1/query", p.token, fits+"a"); rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("over 1 MiB: %d", rec.Code)
	}
	req := httptest.NewRequest("POST", "/grafana/prometheus/api/v1/query", strings.NewReader("query=up"))
	req.Header.Set("Authorization", "Bearer "+p.token)
	req.Header.Set("Content-Type", "multipart/form-data; boundary=x")
	rec := httptest.NewRecorder()
	p.h.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnsupportedMediaType {
		t.Fatalf("multipart: %d", rec.Code)
	}
	if p.vm.received() != 1 {
		t.Fatalf("store hits: %d", p.vm.received())
	}
}

func TestPromProxyUpstreamError(t *testing.T) {
	p := newPromEnv(t)
	host := strings.TrimPrefix(p.vm.URL, "http://")
	p.vm.status = 500
	rec := p.prom("GET", "/grafana/prometheus/api/v1/query", p.token, "")
	if rec.Code != 502 || strings.Contains(rec.Body.String(), host) {
		t.Fatalf("5xx: %d %s", rec.Code, rec.Body.String())
	}
	p.vm.status = 400
	if rec := p.prom("GET", "/grafana/prometheus/api/v1/query", p.token, ""); rec.Code != 400 {
		t.Fatalf("4xx passes through: %d", rec.Code)
	}
	p.vm.Close()
	rec = p.prom("GET", "/grafana/prometheus/api/v1/query", p.token, "")
	if rec.Code != 502 || strings.Contains(rec.Body.String(), host) || strings.Contains(rec.Body.String(), "127.0.0.1") {
		t.Fatalf("down: %d %s", rec.Code, rec.Body.String())
	}
}

func TestPromProxyRefusesRedirects(t *testing.T) {
	p := newPromEnv(t)
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		http.Redirect(w, r, "/api/v1/query?query=up", http.StatusFound)
	}))
	defer srv.Close()
	p.api.telemetry = &telemetry.Client{MetricsURL: srv.URL}
	rec := p.prom("GET", "/grafana/prometheus/api/v1/query?query=up", p.token, "")
	if rec.Code != 502 || hits.Load() != 1 || strings.Contains(rec.Body.String(), "elsewhere") {
		t.Fatalf("redirect: %d, %d hits, %s", rec.Code, hits.Load(), rec.Body.String())
	}
}

func TestPromProxyGetIgnoresFormBody(t *testing.T) {
	p := newPromEnv(t)
	req := httptest.NewRequest("GET", "/grafana/prometheus/api/v1/query?query=up", strings.NewReader("extra_label=observatory_org%3DB&extra_filters[]=x"))
	req.Header.Set("Authorization", "Bearer "+p.token)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	p.h.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}
	onlyOrgScope(t, p.forwarded(0), p.org)
}

func TestPromProxyOutlivesServerWriteTimeout(t *testing.T) {
	p := newPromEnv(t)
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(400 * time.Millisecond)
		_, _ = io.WriteString(w, `{"status":"success"}`)
	}))
	defer slow.Close()
	p.api.telemetry = &telemetry.Client{MetricsURL: slow.URL}
	hub := httptest.NewUnstartedServer(p.h)
	hub.Config.WriteTimeout = 100 * time.Millisecond
	hub.Start()
	defer hub.Close()
	req, _ := http.NewRequest("GET", hub.URL+"/grafana/prometheus/api/v1/query?query=up", nil)
	req.Header.Set("Authorization", "Bearer "+p.token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil || resp.StatusCode != 200 || string(body) != `{"status":"success"}` {
		t.Fatalf("%d %q %v", resp.StatusCode, body, err)
	}
}

func TestPromProxyCrossOrg(t *testing.T) {
	metricsURL := os.Getenv("OBSERVATORY_TEST_METRICS_URL")
	if metricsURL == "" {
		t.Skip("OBSERVATORY_TEST_METRICS_URL not set")
	}
	p := newPromEnv(t)
	p.api.telemetry = &telemetry.Client{MetricsURL: metricsURL}
	_, bobTok := p.user("bob")
	orgB := p.createOrg(bobTok, "Other")
	run := uuid.NewString()
	ts := time.Now().Add(-time.Minute).UnixMilli()
	var lines strings.Builder
	for _, org := range []uuid.UUID{p.org, orgB} {
		fmt.Fprintf(&lines, "grafana_probe{observatory_org=%q,run=%q} 1 %d\n", org, run, ts)
	}
	for _, step := range []func() (*http.Response, error){
		func() (*http.Response, error) {
			return http.Post(metricsURL+"/api/v1/import/prometheus", "text/plain", strings.NewReader(lines.String()))
		},
		func() (*http.Response, error) { return http.Get(metricsURL + "/internal/force_flush") },
	} {
		resp, err := step()
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode >= 300 {
			t.Fatalf("seed: %d", resp.StatusCode)
		}
	}

	type result struct {
		Status string
		Data   json.RawMessage
	}
	get := func(target string) result {
		t.Helper()
		rec := p.prom("GET", target, p.token, "")
		var r result
		if rec.Code != 200 || json.Unmarshal(rec.Body.Bytes(), &r) != nil || r.Status != "success" {
			t.Fatalf("%s: %d %s", target, rec.Code, rec.Body.String())
		}
		return r
	}
	sel := func(extra string) string {
		return url.QueryEscape(fmt.Sprintf(`grafana_probe{run=%q%s}`, run, extra))
	}
	deadline := time.Now().Add(30 * time.Second)
	for !strings.Contains(string(get("/grafana/prometheus/api/v1/query?query="+sel("")).Data), p.org.String()) {
		if time.Now().After(deadline) {
			t.Fatal("own series never visible")
		}
		time.Sleep(500 * time.Millisecond)
	}

	window := fmt.Sprintf("&start=%d&end=%d", time.Now().Add(-10*time.Minute).Unix(), time.Now().Unix())
	b := fmt.Sprintf(`,observatory_org=%q`, orgB)
	for _, target := range []string{
		"/grafana/prometheus/api/v1/query?query=" + sel(""),
		"/grafana/prometheus/api/v1/query?query=" + sel(b),
		"/grafana/prometheus/api/v1/query?query=" + url.QueryEscape(fmt.Sprintf(`{__name__=~".+",observatory_org=%q}`, orgB)),
		"/grafana/prometheus/api/v1/query?query=" + sel("") + "&extra_label=observatory_org%3D" + orgB.String(),
		"/grafana/prometheus/api/v1/query?query=" + sel("") + "&" + url.Values{"extra_filters[]": {fmt.Sprintf(`{observatory_org=%q}`, orgB)}, "extra_filters": {fmt.Sprintf(`{observatory_org=%q}`, orgB)}}.Encode(),
		"/grafana/prometheus/api/v1/query_range?step=15&query=" + sel(b) + window,
		"/grafana/prometheus/api/v1/series?match[]=" + sel(b) + window,
		"/grafana/prometheus/api/v1/series?match[]=" + sel("") + window,
		"/grafana/prometheus/api/v1/label/observatory_org/values?" + window[1:],
	} {
		if data := get(target).Data; strings.Contains(string(data), orgB.String()) {
			t.Fatalf("%s leaks org B: %s", target, data)
		}
	}
	if data := get("/grafana/prometheus/api/v1/label/run/values?match[]=" + sel(b) + window).Data; strings.Contains(string(data), run) {
		t.Fatalf("label values leak org B: %s", data)
	}
	rec := p.prom("POST", "/grafana/prometheus/api/v1/query", p.token, "query="+sel(b))
	if rec.Code != 200 || strings.Contains(rec.Body.String(), orgB.String()) || strings.Contains(rec.Body.String(), run) {
		t.Fatalf("post leaks org B: %d %s", rec.Code, rec.Body.String())
	}
}

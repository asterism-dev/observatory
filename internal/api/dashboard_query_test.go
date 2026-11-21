package api

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/asterism-dev/observatory/internal/authz"
	"github.com/asterism-dev/observatory/internal/store"
	"github.com/asterism-dev/observatory/internal/telemetry"
)

// dashQueryEnv: alice owns the org, bob owns vmA, carol owns vmB, dave is a member without VMs.
type dashQueryEnv struct {
	*env
	org                                 uuid.UUID
	vmA, vmB                            string
	bob                                 store.User
	aliceTok, bobTok, carolTok, daveTok string

	mu     sync.Mutex
	paths  []string
	forms  []url.Values
	status int
	reply  string
}

func newDashQueryEnv(t *testing.T) *dashQueryEnv {
	t.Helper()
	d := &dashQueryEnv{env: newEnv(t), status: 200}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		d.mu.Lock()
		d.paths, d.forms = append(d.paths, r.URL.Path), append(d.forms, r.Form)
		status, reply := d.status, d.reply
		d.mu.Unlock()
		w.WriteHeader(status)
		if reply != "" {
			_, _ = w.Write([]byte(reply))
			return
		}
		switch r.URL.Path {
		case "/api/v1/query_range":
			fmt.Fprintf(w, `{"data":{"resultType":"matrix","result":[{"metric":{"observatory_vm":%q},"values":[[1000,"1"],[1015,"NaN"]]},{"metric":{"observatory_vm":%q},"values":[[1000,"9"]]},{"metric":{},"values":[[1015,"2.5"]]}]}}`, d.vmA, d.vmB)
		case "/api/v1/query":
			fmt.Fprintf(w, `{"data":{"resultType":"vector","result":[{"metric":{"observatory_vm":%q},"value":[1000,"1.5"]},{"metric":{"observatory_vm":%q},"value":[1000,"9"]}]}}`, d.vmA, d.vmB)
		case "/api/v1/label/__name__/values":
			_, _ = w.Write([]byte(`{"status":"success","data":["orders_total","up"]}`))
		}
	}))
	t.Cleanup(srv.Close)
	d.api.telemetry = &telemetry.Client{MetricsURL: srv.URL}
	_, d.aliceTok = d.user("alice")
	bob, bobTok := d.user("bob")
	carol, carolTok := d.user("carol")
	dave, daveTok := d.user("dave")
	d.bob, d.bobTok, d.carolTok, d.daveTok = bob, bobTok, carolTok, daveTok
	d.org = d.createOrg(d.aliceTok, "Acme")
	for _, u := range []store.User{bob, carol, dave} {
		d.addMember(d.org, u, store.RoleMember)
	}
	node := d.enrolledNode(d.aliceTok, d.org, "n1")
	d.vmA = d.createVM(bobTok, d.org, vmBody("a", userOwner(), node)).ID
	d.vmB = d.createVM(carolTok, d.org, vmBody("b", userOwner(), node)).ID
	d.paths, d.forms = nil, nil
	return d
}

func (d *dashQueryEnv) query(tok, body string) *httptest.ResponseRecorder {
	return d.send("POST", dashboardsPath(d.org)+"/query", tok, body)
}

func (d *dashQueryEnv) rangeBody(q string) string {
	to := time.Now().UTC()
	return fmt.Sprintf(`{"query":%q,"from":%q,"to":%q}`, q, to.Add(-time.Hour).Format(time.RFC3339), to.Format(time.RFC3339))
}

func (d *dashQueryEnv) upstream() ([]string, []url.Values) {
	d.mu.Lock()
	defer d.mu.Unlock()
	return slices.Clone(d.paths), slices.Clone(d.forms)
}

func (d *dashQueryEnv) grantNodeMetrics() {
	role := d.createRole(d.aliceTok, d.org, `{"name":"nodes","rules":[{"effect":"allow","object":"org/nodes/*","action":"node:metrics"}]}`)
	d.bind(d.aliceTok, d.org, authz.UserSubject(d.bob.ID), role)
}

func checkScope(t *testing.T, form url.Values, org uuid.UUID, filters ...string) {
	t.Helper()
	if got := form["extra_label"]; len(got) != 1 || got[0] != "observatory_org="+org.String() {
		t.Fatalf("extra_label: %q", got)
	}
	if got := form["extra_filters[]"]; !slices.Equal(got, filters) {
		t.Fatalf("extra_filters[]: %q, want %q", got, filters)
	}
	if _, ok := form["extra_filters"]; ok {
		t.Fatalf("extra_filters: %v", form)
	}
}

type rangeResultTest struct {
	T      []int64
	Series []struct {
		Labels map[string]string
		Values []*float64
	}
}

func TestDashboardQueryAdminOrgOnly(t *testing.T) {
	d := newDashQueryEnv(t)
	rec := d.query(d.aliceTok, d.rangeBody("sum(up)"))
	if rec.Code != 200 {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}
	paths, forms := d.upstream()
	if len(paths) != 1 || paths[0] != "/api/v1/query_range" || forms[0].Get("query") != "sum(up)" {
		t.Fatalf("upstream %v %v", paths, forms)
	}
	checkScope(t, forms[0], d.org)
	var res rangeResultTest
	decode(t, rec, &res)
	if !slices.Equal(res.T, []int64{1000, 1015}) || len(res.Series) != 3 {
		t.Fatalf("result %s", rec.Body.String())
	}
	if a := res.Series[0]; a.Labels["observatory_vm"] != d.vmA || *a.Values[0] != 1 || a.Values[1] != nil {
		t.Fatalf("series a %s", rec.Body.String())
	}

	rec = d.query(d.aliceTok, `{"query":"up","time":"2026-01-01T00:00:00Z"}`)
	if rec.Code != 200 {
		t.Fatalf("instant %d %s", rec.Code, rec.Body.String())
	}
	paths, forms = d.upstream()
	if paths[1] != "/api/v1/query" || forms[1].Get("time") != strconv.FormatInt(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC).Unix(), 10) {
		t.Fatalf("instant upstream %v %v", paths, forms[1])
	}
	checkScope(t, forms[1], d.org)
	var inst struct {
		Series []struct {
			Labels map[string]string
			Value  *float64
		}
	}
	decode(t, rec, &inst)
	if len(inst.Series) != 2 || *inst.Series[0].Value != 1.5 {
		t.Fatalf("instant result %s", rec.Body.String())
	}
}

func TestDashboardQueryMemberFilters(t *testing.T) {
	d := newDashQueryEnv(t)
	rec := d.query(d.bobTok, d.rangeBody("up"))
	if rec.Code != 200 {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}
	_, forms := d.upstream()
	if forms[0].Get("timeout") != "30s" {
		t.Fatalf("timeout: %v", forms[0])
	}
	vmFilter := `{observatory_vm=~"` + d.vmA + `"}`
	checkScope(t, forms[0], d.org, vmFilter)
	if strings.Contains(rec.Body.String(), d.vmB) {
		t.Fatalf("series of another VM returned: %s", rec.Body.String())
	}
	var res rangeResultTest
	decode(t, rec, &res)
	if len(res.Series) != 2 {
		t.Fatalf("own and unlabelled series expected: %s", rec.Body.String())
	}

	if rec := d.query(d.bobTok, `{"query":"up"}`); rec.Code != 200 || strings.Contains(rec.Body.String(), d.vmB) {
		t.Fatalf("instant: %d %s", rec.Code, rec.Body.String())
	}

	d.grantNodeMetrics()
	if rec := d.query(d.bobTok, d.rangeBody("up")); rec.Code != 200 {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}
	_, forms = d.upstream()
	checkScope(t, forms[2], d.org, vmFilter, `{observatory_vm=""}`)
}

func TestDashboardQueryEmptyScope(t *testing.T) {
	d := newDashQueryEnv(t)
	rec := d.query(d.daveTok, d.rangeBody(`{__name__=~".+"}`))
	if rec.Code != 200 || strings.TrimSpace(rec.Body.String()) != `{"t":[],"series":[]}` {
		t.Fatalf("range: %d %s", rec.Code, rec.Body.String())
	}
	rec = d.query(d.daveTok, `{"query":"up"}`)
	if rec.Code != 200 || strings.TrimSpace(rec.Body.String()) != `{"series":[]}` {
		t.Fatalf("instant: %d %s", rec.Code, rec.Body.String())
	}
	rec = d.do("GET", dashboardsPath(d.org)+"/metric-names", d.daveTok)
	if rec.Code != 200 || strings.TrimSpace(rec.Body.String()) != `{"names":[]}` {
		t.Fatalf("names: %d %s", rec.Code, rec.Body.String())
	}
	if paths, _ := d.upstream(); len(paths) != 0 {
		t.Fatalf("store queried for an empty scope: %v", paths)
	}
}

func TestDashboardQueryStepClamp(t *testing.T) {
	d := newDashQueryEnv(t)
	to := time.Now().UTC().Truncate(time.Second)
	from := to.Add(-31 * 24 * time.Hour)
	for i, body := range []string{
		fmt.Sprintf(`{"query":"up","from":%q,"to":%q,"step":1}`, from.Format(time.RFC3339), to.Format(time.RFC3339)),
		fmt.Sprintf(`{"query":"up","from":%q,"to":%q}`, from.Format(time.RFC3339), to.Format(time.RFC3339)),
	} {
		if rec := d.query(d.aliceTok, body); rec.Code != 200 {
			t.Fatalf("%d %s", rec.Code, rec.Body.String())
		}
		_, forms := d.upstream()
		step, err := time.ParseDuration(forms[i].Get("step"))
		start, _ := strconv.ParseInt(forms[i].Get("start"), 10, 64)
		end, _ := strconv.ParseInt(forms[i].Get("end"), 10, 64)
		if err != nil || start != from.Unix() || end != to.Unix() {
			t.Fatalf("forwarded %v", forms[i])
		}
		// VictoriaMetrics may align start down to a step multiple, adding a point.
		if points := (end-start)/int64(step.Seconds()) + 2; points > 11000 {
			t.Fatalf("step %s gives %d points", step, points)
		}
	}
	for _, body := range []string{
		`{"query":"up","from":"2026-01-02T00:00:00Z","to":"2026-01-01T00:00:00Z"}`,
		`{"query":"up","from":"2025-01-01T00:00:00Z","to":"2026-01-01T00:00:00Z"}`,
		`{"query":"up","from":"2026-01-01T00:00:00Z"}`,
		`{"query":"up","from":"2026-01-01T00:00:00Z","to":"2026-01-01T01:00:00Z","step":-1}`,
		`{"query":""}`,
		`{"query":"` + strings.Repeat("a", 4097) + `"}`,
	} {
		if rec := d.query(d.aliceTok, body); rec.Code != 400 {
			t.Fatalf("%.80s: %d %s", body, rec.Code, rec.Body.String())
		}
	}
}

func TestDashboardQueryRateLimit(t *testing.T) {
	d := newDashQueryEnv(t)
	now := time.Now()
	d.api.now = func() time.Time { return now }
	for i := range 40 {
		if rec := d.query(d.bobTok, `{"query":"up"}`); rec.Code != 200 {
			t.Fatalf("request %d: %d", i, rec.Code)
		}
	}
	if rec := d.query(d.bobTok, `{"query":"up"}`); rec.Code != 429 {
		t.Fatalf("over burst: %d", rec.Code)
	}
	if rec := d.do("GET", dashboardsPath(d.org)+"/metric-names", d.bobTok); rec.Code != 429 {
		t.Fatalf("names over burst: %d", rec.Code)
	}
	if rec := d.query(d.carolTok, `{"query":"up"}`); rec.Code != 200 {
		t.Fatalf("other user limited: %d", rec.Code)
	}
	now = now.Add(time.Second)
	for i := range 10 {
		if rec := d.query(d.bobTok, `{"query":"up"}`); rec.Code != 200 {
			t.Fatalf("refill %d: %d", i, rec.Code)
		}
	}
	if rec := d.query(d.bobTok, `{"query":"up"}`); rec.Code != 429 {
		t.Fatalf("over rate: %d", rec.Code)
	}
}

func TestDashboardQueryTimeout(t *testing.T) {
	d := newDashQueryEnv(t)
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// The server notices the hub hanging up only once the body is consumed.
		_, _ = io.ReadAll(r.Body)
		select {
		case <-r.Context().Done():
		case <-time.After(5 * time.Second):
		}
	}))
	t.Cleanup(slow.Close)
	d.api.telemetry = &telemetry.Client{MetricsURL: slow.URL}
	old := dashboardQueryTimeout
	dashboardQueryTimeout = 100 * time.Millisecond
	t.Cleanup(func() { dashboardQueryTimeout = old })
	start := time.Now()
	rec := d.query(d.aliceTok, `{"query":"up"}`)
	if rec.Code != 502 || time.Since(start) > 2*time.Second || strings.Contains(rec.Body.String(), slow.URL) {
		t.Fatalf("%d after %s: %s", rec.Code, time.Since(start), rec.Body.String())
	}
}

func TestDashboardQueryErrors(t *testing.T) {
	d := newDashQueryEnv(t)
	d.status, d.reply = 400, `{"status":"error","errorType":"400","error":"error when executing query=\"sum(\" (see http://victoriametrics:8428/api/v1/query): unexpected token"}`
	rec := d.query(d.aliceTok, `{"query":"sum("}`)
	var e struct{ Error string }
	decode(t, rec, &e)
	if rec.Code != 400 || !strings.Contains(e.Error, `query="sum("`) || strings.Contains(e.Error, "victoriametrics:8428") || strings.Contains(e.Error, "http") {
		t.Fatalf("bad query: %d %s", rec.Code, rec.Body.String())
	}
	d.status, d.reply = 503, "upstream http://victoriametrics:8428 down"
	rec = d.query(d.aliceTok, `{"query":"up"}`)
	decode(t, rec, &e)
	if rec.Code != 502 || e.Error != "telemetry store unavailable" {
		t.Fatalf("store error: %d %s", rec.Code, rec.Body.String())
	}
	d.status, d.reply = 302, `<a href="http://victoriametrics:8428/elsewhere">Found</a>`
	if rec := d.query(d.aliceTok, `{"query":"up"}`); rec.Code != 502 || strings.Contains(rec.Body.String(), "victoriametrics") {
		t.Fatalf("redirect: %d %s", rec.Code, rec.Body.String())
	}
	d.api.telemetry = nil
	for _, rec := range []*httptest.ResponseRecorder{d.query(d.aliceTok, `{"query":"up"}`), d.do("GET", dashboardsPath(d.org)+"/metric-names", d.aliceTok)} {
		decode(t, rec, &e)
		if rec.Code != 404 || e.Error != "telemetry is off" {
			t.Fatalf("telemetry off: %d %s", rec.Code, rec.Body.String())
		}
	}
}

func TestMetricNamesScoped(t *testing.T) {
	d := newDashQueryEnv(t)
	rec := d.do("GET", dashboardsPath(d.org)+"/metric-names?match="+url.QueryEscape(`{job="shop"}`), d.bobTok)
	if rec.Code != 200 || strings.TrimSpace(rec.Body.String()) != `{"names":["orders_total","up"]}` {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}
	paths, forms := d.upstream()
	if paths[0] != "/api/v1/label/__name__/values" || forms[0].Get("match[]") != `{job="shop"}` {
		t.Fatalf("upstream %v %v", paths, forms)
	}
	checkScope(t, forms[0], d.org, `{observatory_vm=~"`+d.vmA+`"}`)
	if forms[0].Get("timeout") != "30s" {
		t.Fatalf("timeout: %v", forms[0])
	}

	if rec := d.do("GET", dashboardsPath(d.org)+"/metric-names", d.aliceTok); rec.Code != 200 {
		t.Fatalf("admin: %d", rec.Code)
	}
	_, forms = d.upstream()
	checkScope(t, forms[1], d.org)
	if _, ok := forms[1]["match[]"]; ok {
		t.Fatalf("empty match forwarded: %v", forms[1])
	}
	if rec := d.do("GET", dashboardsPath(uuid.New())+"/metric-names", d.bobTok); rec.Code != 404 {
		t.Fatalf("foreign org: %d", rec.Code)
	}
}

func TestDashboardQueryScopingLive(t *testing.T) {
	metricsURL := os.Getenv("OBSERVATORY_TEST_METRICS_URL")
	if metricsURL == "" {
		t.Skip("OBSERVATORY_TEST_METRICS_URL not set")
	}
	d := newDashQueryEnv(t)
	d.api.telemetry = &telemetry.Client{MetricsURL: metricsURL}
	run := uuid.NewString()
	ts := time.Now().Add(-time.Minute).UnixMilli()
	seed := fmt.Sprintf("dash_probe_a{observatory_org=%q,observatory_vm=%q,run=%q} 1 %d\n", d.org, d.vmA, run, ts) +
		fmt.Sprintf("dash_probe_b{observatory_org=%q,observatory_vm=%q,run=%q} 2 %d\n", d.org, d.vmB, run, ts) +
		fmt.Sprintf("dash_probe_node{observatory_org=%q,observatory_node=\"n1\",run=%q} 3 %d\n", d.org, run, ts)
	resp, err := http.Post(metricsURL+"/api/v1/import/prometheus", "text/plain", strings.NewReader(seed))
	if err != nil || resp.StatusCode >= 300 {
		t.Fatal("seed", err)
	}
	resp.Body.Close()
	if resp, err = http.Get(metricsURL + "/internal/force_flush"); err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()

	sel := fmt.Sprintf(`{run=%q}`, run)
	instant := func(tok, q string) string {
		t.Helper()
		rec := d.query(tok, fmt.Sprintf(`{"query":%q}`, q))
		if rec.Code != 200 {
			t.Fatalf("%s: %d %s", q, rec.Code, rec.Body.String())
		}
		return rec.Body.String()
	}
	deadline := time.Now().Add(30 * time.Second)
	for !strings.Contains(instant(d.aliceTok, sel), "dash_probe_node") || !strings.Contains(instant(d.aliceTok, sel), d.vmB) {
		if time.Now().After(deadline) {
			t.Fatal("seeded series never visible to the admin")
		}
		time.Sleep(500 * time.Millisecond)
	}

	b := fmt.Sprintf(`observatory_vm=%q`, d.vmB)
	leaks := func(body string, node bool) bool {
		return strings.Contains(body, d.vmB) || strings.Contains(body, "dash_probe_b") || !node && strings.Contains(body, "dash_probe_node")
	}
	attacks := []string{
		fmt.Sprintf(`{__name__=~".+",run=%q}`, run),
		fmt.Sprintf(`{__name__=~".+",%s}`, b),
		fmt.Sprintf(`label_replace({run=%q,%s}, "x", "$1", "observatory_vm", "(.*)")`, run, b),
		fmt.Sprintf(`label_replace({run=%q}, "leak", "$1", "__name__", "(.*)")`, run),
		fmt.Sprintf(`count by (observatory_vm, __name__) ({run=%q})`, run),
		fmt.Sprintf(`max_over_time({run=%q}[10m:30s])`, run),
		fmt.Sprintf(`max_over_time(dash_probe_b{%s}[10m:30s])`, b),
		fmt.Sprintf(`absent(dash_probe_b{%s})`, b),
		fmt.Sprintf(`sum(dash_probe_b{run=%q}) or vector(0)`, run),
	}
	for _, node := range []bool{false, true} {
		if node {
			d.grantNodeMetrics()
		}
		for _, q := range attacks {
			if body := instant(d.bobTok, q); leaks(body, node) {
				t.Fatalf("node=%v %s leaks: %s", node, q, body)
			}
		}
		to := time.Now().UTC()
		rec := d.query(d.bobTok, fmt.Sprintf(`{"query":%q,"from":%q,"to":%q,"step":15}`, attacks[0], to.Add(-10*time.Minute).Format(time.RFC3339), to.Format(time.RFC3339)))
		if rec.Code != 200 || leaks(rec.Body.String(), node) || !strings.Contains(rec.Body.String(), "dash_probe_a") {
			t.Fatalf("node=%v range: %d %s", node, rec.Code, rec.Body.String())
		}
		// Multiple extra_filters[] are OR-ed: with node access both the own VM and node series show.
		own := instant(d.bobTok, attacks[0])
		if !strings.Contains(own, "dash_probe_a") || strings.Contains(own, "dash_probe_node") != node {
			t.Fatalf("node=%v own series: %s", node, own)
		}
		rec = d.do("GET", dashboardsPath(d.org)+"/metric-names?match="+url.QueryEscape(sel), d.bobTok)
		var names struct{ Names []string }
		decode(t, rec, &names)
		want := []string{"dash_probe_a"}
		if node {
			want = append(want, "dash_probe_node")
		}
		if rec.Code != 200 || !slices.Equal(names.Names, want) {
			t.Fatalf("node=%v names: %d %s", node, rec.Code, rec.Body.String())
		}
		rec = d.do("GET", dashboardsPath(d.org)+"/metric-names", d.bobTok)
		names.Names = nil
		decode(t, rec, &names)
		if rec.Code != 200 || !slices.Contains(names.Names, "dash_probe_a") || slices.Contains(names.Names, "dash_probe_b") || slices.Contains(names.Names, "dash_probe_node") != node {
			t.Fatalf("node=%v names without match: %d %s", node, rec.Code, rec.Body.String())
		}
	}
	var res struct{ Series []json.RawMessage }
	if err := json.Unmarshal([]byte(instant(d.aliceTok, sel)), &res); err != nil || len(res.Series) != 3 {
		t.Fatalf("admin sees all: %v %d", err, len(res.Series))
	}
}

package telemetry_test

import (
	"context"
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

var (
	exOrg   = uuid.MustParse("11111111-1111-1111-1111-111111111111")
	exVM    = uuid.MustParse("33333333-3333-3333-3333-333333333333")
	exNode  = uuid.MustParse("22222222-2222-2222-2222-222222222222")
	exImage = uuid.MustParse("44444444-4444-4444-4444-444444444444")
	exBuild = uuid.MustParse("55555555-5555-5555-5555-555555555555")
)

// fakeLogs records every form it receives and answers with reply(form).
func fakeLogs(t *testing.T, reply func(url.Values) string) (*telemetry.Client, func() []url.Values) {
	t.Helper()
	var mu sync.Mutex
	var forms []url.Values
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		if r.URL.Path != "/select/logsql/query" {
			t.Errorf("path %s", r.URL.Path)
		}
		mu.Lock()
		forms = append(forms, r.PostForm)
		mu.Unlock()
		_, _ = w.Write([]byte(reply(r.PostForm)))
	}))
	t.Cleanup(srv.Close)
	return &telemetry.Client{LogsURL: srv.URL}, func() []url.Values {
		mu.Lock()
		defer mu.Unlock()
		return slices.Clone(forms)
	}
}

func orgFilter(org uuid.UUID) string { return `observatory.org:="` + org.String() + `"` }

func TestLogGroupsQueries(t *testing.T) {
	deadVM := uuid.New()
	c, forms := fakeLogs(t, func(f url.Values) string {
		switch {
		case strings.Contains(f.Get("query"), "by (observatory.vm)"):
			return `{"observatory.vm":"` + exVM.String() + `","last":"2026-11-20T10:00:00Z","streams":"3","name":"{\"observatory.vm_name\":\"web\"}"}` + "\n" +
				`{"observatory.vm":"` + deadVM.String() + `","last":"2026-11-19T10:00:00Z","streams":"1","name":"{\"observatory.vm_name\":\"old\"}"}` + "\n"
		case strings.Contains(f.Get("query"), "by (observatory.node)"):
			return `{"observatory.node":"` + exNode.String() + `","last":"2026-11-20T11:00:00Z","streams":"2","name":"{\"observatory.node_name\":\"n1\"}"}` + "\n" +
				`{"observatory.node":"not-a-uuid","last":"2026-11-20T11:00:00Z","streams":"1","name":"{}"}` + "\n"
		default:
			return `{"observatory.image":"` + exImage.String() + `","last":"2026-11-20T09:00:00Z","streams":"4","name":"{\"observatory.image_name\":\"base\"}"}` + "\n"
		}
	})
	since := time.Date(2026, 11, 7, 0, 0, 0, 0, time.UTC)
	groups, err := c.LogGroups(context.Background(), exOrg, since)
	if err != nil {
		t.Fatal(err)
	}
	got := forms()
	if len(got) != 3 {
		t.Fatalf("%d queries", len(got))
	}
	want := []struct{ filter, query string }{
		{orgFilter(exOrg) + ` observatory.vm:*`, `* | stats by (observatory.vm) max(_time) last, count_uniq(observatory.stream) streams, row_max(_time, observatory.vm_name) name`},
		{orgFilter(exOrg) + ` observatory.node:* observatory.vm:=""`, `* | stats by (observatory.node) max(_time) last, count_uniq(source) streams, row_max(_time, observatory.node_name) name`},
		{orgFilter(exOrg) + ` observatory.image:*`, `* | stats by (observatory.image) max(_time) last, count_uniq(observatory.build) streams, row_max(_time, observatory.image_name) name`},
	}
	for i, w := range want {
		if f := got[i]["extra_filters"]; len(f) != 1 || f[0] != w.filter {
			t.Errorf("query %d extra_filters %v, want %q", i, f, w.filter)
		}
		if q := got[i].Get("query"); q != w.query {
			t.Errorf("query %d: %q, want %q", i, q, w.query)
		}
		if s := got[i].Get("start"); s != "2026-11-07T00:00:00Z" {
			t.Errorf("query %d start %q", i, s)
		}
	}
	at := func(s string) time.Time { v, _ := time.Parse(time.RFC3339, s); return v }
	wantGroups := []telemetry.LogGroup{
		{Kind: telemetry.KindNode, ID: exNode, Name: "n1", Streams: 2, LastAt: at("2026-11-20T11:00:00Z")},
		{Kind: telemetry.KindVM, ID: exVM, Name: "web", Streams: 3, LastAt: at("2026-11-20T10:00:00Z")},
		{Kind: telemetry.KindImage, ID: exImage, Name: "base", Streams: 4, LastAt: at("2026-11-20T09:00:00Z")},
		{Kind: telemetry.KindVM, ID: deadVM, Name: "old", Streams: 1, LastAt: at("2026-11-19T10:00:00Z")},
	}
	if !slices.Equal(groups, wantGroups) {
		t.Fatalf("groups\n got %+v\nwant %+v", groups, wantGroups)
	}
}

func TestLogGroupStreams(t *testing.T) {
	cases := []struct {
		g             telemetry.GroupRef
		filter, field string
	}{
		{telemetry.GroupRef{Kind: telemetry.KindVM, ID: exVM}, orgFilter(exOrg) + ` observatory.vm:="` + exVM.String() + `"`, "observatory.stream"},
		{telemetry.GroupRef{Kind: telemetry.KindNode, ID: exNode}, orgFilter(exOrg) + ` observatory.node:="` + exNode.String() + `" observatory.vm:=""`, "source"},
		{telemetry.GroupRef{Kind: telemetry.KindImage, ID: exImage}, orgFilter(exOrg) + ` observatory.image:="` + exImage.String() + `"`, "observatory.build"},
	}
	for _, tc := range cases {
		c, forms := fakeLogs(t, func(url.Values) string {
			return `{"` + tc.field + `":"a","last":"2026-11-20T10:00:00Z"}` + "\n" + `{"` + tc.field + `":"b","last":"2026-11-20T12:00:00Z"}` + "\n"
		})
		streams, err := c.LogGroupStreams(context.Background(), exOrg, tc.g, time.Now().Add(-time.Hour))
		if err != nil {
			t.Fatal(err)
		}
		f := forms()[0]
		if got := f["extra_filters"]; len(got) != 1 || got[0] != tc.filter {
			t.Errorf("%s: extra_filters %v, want %q", tc.g.Kind, got, tc.filter)
		}
		if q, want := f.Get("query"), `* | stats by (`+tc.field+`) max(_time) last`; q != want {
			t.Errorf("%s: query %q, want %q", tc.g.Kind, q, want)
		}
		if len(streams) != 2 || streams[0].Name != "b" || streams[1].Name != "a" || streams[0].LastAt.Hour() != 12 {
			t.Errorf("%s: streams %+v", tc.g.Kind, streams)
		}
	}
}

func TestGroupLogQueryImage(t *testing.T) {
	c, forms := fakeLogs(t, func(url.Values) string { return "" })
	q := c.GroupLogQuery(exOrg, telemetry.GroupRef{Kind: telemetry.KindImage, ID: exImage}, exBuild.String())
	if q.Image != exImage || q.Build != exBuild || q.Node != uuid.Nil || q.VM != uuid.Nil {
		t.Fatalf("query %+v", q)
	}
	if _, err := c.Logs(context.Background(), q); err != nil {
		t.Fatal(err)
	}
	f := forms()[0]
	if got, want := f.Get("extra_filters"), orgFilter(exOrg)+` observatory.image:="`+exImage.String()+`"`; got != want {
		t.Fatalf("extra_filters %q, want %q", got, want)
	}
	if got, want := f.Get("query"), `"observatory.build":="`+exBuild.String()+`"`; got != want {
		t.Fatalf("query %q, want %q", got, want)
	}

	vm := c.GroupLogQuery(exOrg, telemetry.GroupRef{Kind: telemetry.KindVM, ID: exVM}, "console")
	node := c.GroupLogQuery(exOrg, telemetry.GroupRef{Kind: telemetry.KindNode, ID: exNode}, "firecracker")
	if vm.VM != exVM || vm.Stream != "console" || node.Node != exNode || node.Source != "firecracker" || vm.Org != exOrg || node.Org != exOrg {
		t.Fatalf("vm %+v node %+v", vm, node)
	}
	if _, err := c.Logs(context.Background(), telemetry.LogQuery{Org: exOrg, Image: exImage, VM: exVM}); err == nil {
		t.Fatal("two scopes accepted")
	}
}

func TestExplorerQueryScope(t *testing.T) {
	vm2 := uuid.New()
	c, forms := fakeLogs(t, func(url.Values) string { return `{"_msg":"x","_time":"2026-11-20T10:00:00Z"}` + "\n" })
	from, to := time.Date(2026, 11, 20, 0, 0, 0, 0, time.UTC), time.Date(2026, 11, 21, 0, 0, 0, 0, time.UTC)
	groups := []telemetry.GroupRef{{Kind: telemetry.KindVM, ID: exVM}, {Kind: telemetry.KindImage, ID: exImage}, {Kind: telemetry.KindNode, ID: exNode}, {Kind: telemetry.KindVM, ID: vm2}}
	if _, err := c.ExplorerQuery(context.Background(), exOrg, groups, `error | stats count()`, from, to, 50); err != nil {
		t.Fatal(err)
	}
	if _, err := c.ExplorerQuery(context.Background(), exOrg, nil, `*`, from, to, 0); err != nil {
		t.Fatal(err)
	}
	got := forms()
	wantScoped := orgFilter(exOrg) + ` (observatory.vm:in("` + exVM.String() + `","` + vm2.String() + `") or (observatory.node:in("` + exNode.String() + `") observatory.vm:="") or observatory.image:in("` + exImage.String() + `"))`
	if f := got[0]["extra_filters"]; len(f) != 1 || f[0] != wantScoped {
		t.Errorf("extra_filters %v\nwant %q", f, wantScoped)
	}
	if got[0].Get("query") != `error | stats count()` || got[0].Get("limit") != "50" || got[0].Get("start") != "2026-11-20T00:00:00Z" || got[0].Get("end") != "2026-11-21T00:00:00Z" {
		t.Errorf("form %v", got[0])
	}
	if f := got[1]["extra_filters"]; len(f) != 1 || f[0] != orgFilter(exOrg) {
		t.Errorf("unscoped extra_filters %v", f)
	}
	if got[1].Get("limit") != "1000" {
		t.Errorf("default limit %q", got[1].Get("limit"))
	}
}

func TestExplorerQueryStatsDetection(t *testing.T) {
	reply := ""
	c, _ := fakeLogs(t, func(url.Values) string { return reply })
	reply = `{"level":"error","c":"3"}` + "\n" + `{"level":"info","c":"9","extra":"x"}` + "\n"
	res, err := c.ExplorerQuery(context.Background(), exOrg, nil, "* | stats by (level) count() c", time.Now().Add(-time.Hour), time.Now(), 10)
	if err != nil {
		t.Fatal(err)
	}
	if !res.Stats || !slices.Equal(res.Columns, []string{"level", "c", "extra"}) || len(res.Rows) != 2 || res.Rows[1]["c"] != "9" {
		t.Fatalf("stats result %+v", res)
	}
	reply = `{"_time":"2026-11-20T10:00:00Z","_msg":"hello","level":"info"}` + "\n"
	res, err = c.ExplorerQuery(context.Background(), exOrg, nil, "*", time.Now().Add(-time.Hour), time.Now(), 10)
	if err != nil {
		t.Fatal(err)
	}
	if res.Stats || len(res.Rows) != 1 || res.Rows[0]["_msg"] != "hello" {
		t.Fatalf("rows result %+v", res)
	}
	reply = `{"_time":"2026-11-20T10:00:00Z","_msg":"a"}` + "\n" + `{"_time":"2026-11-20T10:02:00Z","_msg":"c"}` + "\n" + `{"_time":"2026-11-20T10:01:00Z","_msg":"b"}` + "\n"
	res, err = c.ExplorerQuery(context.Background(), exOrg, nil, "*", time.Now().Add(-time.Hour), time.Now(), 10)
	if err != nil || res.Rows[0]["_msg"] != "c" || res.Rows[1]["_msg"] != "b" || res.Rows[2]["_msg"] != "a" {
		t.Fatalf("not newest first: %+v, %v", res, err)
	}
}

func TestExplorerQueryScopingLive(t *testing.T) {
	logsURL := os.Getenv("OBSERVATORY_TEST_LOGS_URL")
	if logsURL == "" {
		t.Skip("OBSERVATORY_TEST_LOGS_URL not set")
	}
	run := strings.ReplaceAll(uuid.NewString(), "-", "")[:12]
	orgA, orgB := uuid.New(), uuid.New()
	mA, mB, shared := "exa"+run, "exb"+run, "exsh"+run
	client := &telemetry.Client{LogsURL: logsURL}
	writeProbeLogs(t, client, orgA, mA, run, shared, "", telemetry.NodeLogStreamFields)
	writeProbeLogs(t, client, orgB, mB, run, shared, "", telemetry.NodeLogStreamFields)
	from, to := time.Now().Add(-time.Hour), time.Now().Add(5*time.Minute)
	query := func(org uuid.UUID, q string) telemetry.QueryResult {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		res, err := client.ExplorerQuery(ctx, org, nil, q, from, to, 1000)
		if err != nil {
			t.Fatalf("%q: %v", q, err)
		}
		return res
	}
	waitFor(t, "explorer logs", func() bool {
		return len(query(orgA, "probe:="+run).Rows) == 3 && len(query(orgB, "probe:="+run).Rows) == 3
	})
	for _, q := range []string{
		`observatory.org:="` + orgB.String() + `"`,
		`* | union (observatory.org:="` + orgB.String() + `")`,
		`* | stats by (observatory.org) count()`,
	} {
		for _, row := range query(orgA, q).Rows {
			for k, v := range row {
				if strings.Contains(v, orgB.String()) || strings.Contains(v, mB) {
					t.Errorf("%q: org B leaked in %s=%q", q, k, v)
				}
			}
		}
	}
	stats := query(orgA, "probe:="+run+" | stats by (observatory.org) count() c")
	if !stats.Stats || len(stats.Rows) != 1 || stats.Rows[0]["observatory.org"] != orgA.String() || stats.Rows[0]["c"] != "3" {
		t.Fatalf("org A stats %+v", stats)
	}
	groups, err := client.LogGroups(context.Background(), orgA, from)
	if err != nil || len(groups) != 1 || groups[0].Kind != telemetry.KindNode || groups[0].Name != "vlprobe" || groups[0].Streams != 0 {
		t.Fatalf("org A groups %+v %v", groups, err)
	}
}

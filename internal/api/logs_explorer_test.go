package api

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/asterism-dev/observatory/internal/store"
	"github.com/asterism-dev/observatory/internal/telemetry"
)

type logsExplorerEnv struct {
	*env
	org                                           uuid.UUID
	admin                                         store.User
	adminTok, memberTok, platformTok, outsiderTok string
	node                                          string
	mu                                            sync.Mutex
	forms                                         []url.Values
	answer                                        func(path string, form url.Values) string
}

// newLogsExplorerEnv: root is the platform admin and only a member of the org; alice owns it, carol is an admin.
func newLogsExplorerEnv(t *testing.T) *logsExplorerEnv {
	t.Helper()
	le := &logsExplorerEnv{env: newEnv(t)}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		le.mu.Lock()
		le.forms = append(le.forms, r.PostForm)
		answer := le.answer
		le.mu.Unlock()
		if answer != nil {
			_, _ = io.WriteString(w, answer(r.URL.Path, r.PostForm))
		}
	}))
	t.Cleanup(srv.Close)
	le.api.telemetry = &telemetry.Client{MetricsURL: srv.URL, LogsURL: srv.URL}
	root, rootTok := le.user("root")
	_, ownerTok := le.user("alice")
	bob, bobTok := le.user("bob")
	le.admin, le.adminTok = le.user("carol")
	_, le.outsiderTok = le.user("dave")
	le.platformTok, le.memberTok = rootTok, bobTok
	le.org = le.createOrg(ownerTok, "Acme")
	le.addMember(le.org, root, store.RoleMember)
	le.addMember(le.org, bob, store.RoleMember)
	le.addMember(le.org, le.admin, store.RoleAdmin)
	le.node = le.createNode(ownerTok, le.org, "n1").Node.ID
	return le
}

func (le *logsExplorerEnv) path(suffix string) string {
	return "/api/orgs/" + le.org.String() + "/logs" + suffix
}

func (le *logsExplorerEnv) received() []url.Values {
	le.mu.Lock()
	defer le.mu.Unlock()
	return append([]url.Values(nil), le.forms...)
}

func (le *logsExplorerEnv) query(tok, body string) *httptest.ResponseRecorder {
	return le.send("POST", le.path("/query"), tok, body)
}

func (le *logsExplorerEnv) endpoints() []struct{ method, path, body string } {
	g := "/groups/node/" + le.node
	return []struct{ method, path, body string }{
		{"GET", le.path("/groups"), ""},
		{"GET", le.path(g + "/streams"), ""},
		{"GET", le.path(g + "/entries"), ""},
		{"GET", le.path(g + "/entries/tail"), ""},
		{"POST", le.path("/query"), `{"query":"*"}`},
	}
}

func TestLogsNeedPermission(t *testing.T) {
	le := newLogsExplorerEnv(t)
	for _, c := range le.endpoints() {
		for tok, want := range map[string]int{le.memberTok: 403, le.platformTok: 403, le.outsiderTok: 404, le.adminTok: 200} {
			if rec := le.send(c.method, c.path, tok, c.body); rec.Code != want {
				t.Errorf("%s %s: got %d, want %d: %s", c.method, c.path, rec.Code, want, rec.Body.String())
			}
		}
	}
}

func TestLogsTelemetryOff(t *testing.T) {
	le := newLogsExplorerEnv(t)
	le.api.telemetry = nil
	for _, c := range le.endpoints() {
		if rec := le.send(c.method, c.path, le.adminTok, c.body); rec.Code != 404 {
			t.Errorf("%s %s: %d", c.method, c.path, rec.Code)
		}
	}
}

func TestLogsBadGroupRef(t *testing.T) {
	le := newLogsExplorerEnv(t)
	id := uuid.NewString()
	for _, p := range []string{
		"/groups?kind=disk",
		"/groups/disk/" + id + "/streams",
		"/groups/vm/not-a-uuid/streams",
		"/groups/vm/" + strings.ToUpper(id) + "/entries",
		"/groups/vm/" + id + "/entries?stream=bogus",
		"/groups/node/" + id + "/entries?stream=service",
		"/groups/image/" + id + "/entries?stream=not-a-build",
		"/groups/image/" + id + "/entries/tail?stream=not-a-build",
	} {
		if rec := le.do("GET", le.path(p), le.adminTok); rec.Code != 400 {
			t.Errorf("%s: %d", p, rec.Code)
		}
	}
	for _, body := range []string{`{"query":"*","groups":[{"kind":"disk","id":"` + id + `"}]}`, `{"query":"*","groups":[{"kind":"vm","id":"x"}]}`} {
		if rec := le.query(le.adminTok, body); rec.Code != 400 {
			t.Errorf("%s: %d", body, rec.Code)
		}
	}
	if got := le.received(); len(got) != 0 {
		t.Fatalf("bad refs reached the store: %v", got)
	}
	for _, p := range []string{"/groups/vm/" + id + "/entries?stream=app", "/groups/node/" + id + "/entries?stream=firecracker", "/groups/image/" + id + "/entries?stream=" + id} {
		if rec := le.do("GET", le.path(p), le.adminTok); rec.Code != 200 {
			t.Errorf("%s: %d", p, rec.Code)
		}
	}
}

func nodeGroupsAnswer(rows ...string) func(string, url.Values) string {
	return func(_ string, form url.Values) string {
		if strings.Contains(form.Get("query"), "by (observatory.node)") {
			return strings.Join(rows, "\n")
		}
		return ""
	}
}

func nodeGroupRow(id, name, last string) string {
	return `{"observatory.node":"` + id + `","last":"` + last + `","streams":"2","name":"{\"observatory.node_name\":\"` + name + `\"}"}`
}

type logGroupTest struct {
	Kind    string
	ID      uuid.UUID
	Name    string
	Deleted bool
	Streams int
	LastAt  time.Time
}

func TestLogsGroupsDeletedAndReusedName(t *testing.T) {
	le := newLogsExplorerEnv(t)
	old := uuid.NewString()
	le.answer = nodeGroupsAnswer(nodeGroupRow(le.node, "n1", "2026-11-21T10:00:00Z"), nodeGroupRow(old, "n1", "2026-11-20T10:00:00Z"))
	if rec := le.send("PATCH", nodesPath(le.org)+"/"+le.node, le.adminTok, `{"name":"n2"}`); rec.Code != 204 {
		t.Fatalf("rename: %d %s", rec.Code, rec.Body.String())
	}
	rec := le.do("GET", le.path("/groups?kind=node"), le.adminTok)
	if rec.Code != 200 {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}
	var got []logGroupTest
	decode(t, rec, &got)
	if len(got) != 2 {
		t.Fatalf("%+v", got)
	}
	live, gone := got[0], got[1]
	if live.ID.String() != le.node || live.Deleted || live.Name != "n2" || live.Kind != "node" || live.Streams != 2 || live.LastAt.IsZero() {
		t.Errorf("live: %+v", live)
	}
	if gone.ID.String() != old || !gone.Deleted || gone.Name != "n1" {
		t.Errorf("deleted: %+v", gone)
	}
	if rec := le.do("GET", le.path("/groups?kind=vm"), le.adminTok); rec.Body.String() != "[]\n" {
		t.Errorf("vm filter: %s", rec.Body.String())
	}
}

func TestLogsGroupsCached(t *testing.T) {
	le := newLogsExplorerEnv(t)
	le.answer = nodeGroupsAnswer(nodeGroupRow(le.node, "n1", "2026-11-21T10:00:00Z"))
	for range 2 {
		if rec := le.do("GET", le.path("/groups"), le.adminTok); rec.Code != 200 {
			t.Fatalf("%d", rec.Code)
		}
	}
	first := len(le.received())
	if first == 0 {
		t.Fatal("no upstream round")
	}
	if rec := le.do("GET", le.path("/groups?kind=node"), le.adminTok); rec.Code != 200 {
		t.Fatalf("%d", rec.Code)
	}
	if got := len(le.received()); got != first {
		t.Fatalf("cached list re-queried: %d then %d", first, got)
	}
	start := le.received()[0].Get("start")
	if at, err := time.Parse(time.RFC3339Nano, start); err != nil || time.Since(at) < 14*24*time.Hour-time.Minute || time.Since(at) > 14*24*time.Hour+time.Minute {
		t.Fatalf("start %q", start)
	}
}

func TestLogsQueryIgnoresClientScope(t *testing.T) {
	le := newLogsExplorerEnv(t)
	vm := uuid.NewString()
	le.answer = func(string, url.Values) string { return `{"_time":"2026-11-21T10:00:00Z","_msg":"boom"}` }
	body := `{"query":"error","extra_filters":"observatory.org:=\"x\"","extra_stream_filters":"{a=\"b\"}","tenant":"7",` +
		`"groups":[{"kind":"vm","id":"` + vm + `","extra_filters":"y"}]}`
	rec := le.query(le.adminTok, body)
	if rec.Code != 200 {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}
	var res struct {
		Columns []string
		Rows    []map[string]string
		Stats   bool
	}
	decode(t, rec, &res)
	if len(res.Rows) != 1 || res.Rows[0]["_msg"] != "boom" || res.Stats || len(res.Columns) != 2 {
		t.Fatalf("%+v", res)
	}
	got := le.received()
	if len(got) != 1 {
		t.Fatalf("%v", got)
	}
	f := got[0]
	ef := f["extra_filters"]
	if len(ef) != 1 || !strings.HasPrefix(ef[0], `observatory.org:="`+le.org.String()+`"`) || !strings.Contains(ef[0], vm) || strings.Contains(ef[0], `"x"`) {
		t.Fatalf("extra_filters %q", ef)
	}
	for k := range f {
		if k != "query" && k != "extra_filters" && k != "start" && k != "end" && k != "limit" && k != "timeout" {
			t.Errorf("unexpected upstream param %q", k)
		}
	}
	if f.Get("query") != "error" {
		t.Fatalf("query %q", f.Get("query"))
	}
}

func TestLogsQueryDenied(t *testing.T) {
	le := newLogsExplorerEnv(t)
	defer func(l []string) { telemetry.LeakingLogsQL = l }(telemetry.LeakingLogsQL)
	telemetry.LeakingLogsQL = []string{"union"}
	if rec := le.query(le.adminTok, `{"query":"a | union (b)"}`); rec.Code != 400 {
		t.Fatalf("%d", rec.Code)
	}
	if got := le.received(); len(got) != 0 {
		t.Fatalf("denied query reached the store: %v", got)
	}
}

func TestLogsRangeClamp(t *testing.T) {
	le := newLogsExplorerEnv(t)
	now := time.Now().UTC()
	old, nowS := now.Add(-30*24*time.Hour).Format(time.RFC3339), now.Format(time.RFC3339)
	nearRetention := func(s string) bool {
		at, err := time.Parse(time.RFC3339Nano, s)
		return err == nil && now.Sub(at) > 14*24*time.Hour-time.Minute && now.Sub(at) < 14*24*time.Hour+time.Minute
	}
	if rec := le.query(le.adminTok, `{"query":"*","from":"`+old+`","to":"`+nowS+`"}`); rec.Code != 200 {
		t.Fatalf("query: %d %s", rec.Code, rec.Body.String())
	}
	if start := le.received()[0].Get("start"); !nearRetention(start) {
		t.Fatalf("query start %q", start)
	}
	if rec := le.do("GET", le.path("/groups/node/"+le.node+"/entries?from="+old+"&to="+nowS), le.adminTok); rec.Code != 200 {
		t.Fatalf("entries: %d %s", rec.Code, rec.Body.String())
	}
	q := le.received()[1].Get("query")
	i := strings.Index(q, "_time:[")
	if i < 0 || !nearRetention(strings.SplitN(q[i+len("_time:["):], ",", 2)[0]) {
		t.Fatalf("entries query %q", q)
	}
	for _, body := range []string{`{"query":"*","from":"` + nowS + `","to":"` + nowS + `"}`, `{"query":"*","from":"` + nowS + `","to":"` + old + `"}`} {
		if rec := le.query(le.adminTok, body); rec.Code != 400 {
			t.Errorf("%s: %d", body, rec.Code)
		}
	}
	if rec := le.do("GET", le.path("/groups/node/"+le.node+"/entries?from="+nowS+"&to="+old), le.adminTok); rec.Code != 400 {
		t.Errorf("entries from >= to: %d", rec.Code)
	}
}

func TestLogsQueryLimits(t *testing.T) {
	le := newLogsExplorerEnv(t)
	for body, want := range map[string]string{`{"query":"*","limit":5000}`: "1000", `{"query":"*"}`: "1000", `{"query":"*","limit":10}`: "10"} {
		n := len(le.received())
		if rec := le.query(le.adminTok, body); rec.Code != 200 {
			t.Fatalf("%s: %d", body, rec.Code)
		}
		if got := le.received()[n].Get("limit"); got != want {
			t.Errorf("%s: limit %q", body, got)
		}
	}
	big := `{"query":"` + strings.Repeat("a", 64<<10) + `"}`
	if rec := le.query(le.adminTok, big); rec.Code != 413 {
		t.Fatalf("big body: %d", rec.Code)
	}
}

func TestLogsPerUserLimits(t *testing.T) {
	le := newLogsExplorerEnv(t)
	var releases []func()
	for range 3 {
		release, ok := le.api.logsQueries.acquire(le.admin.ID)
		if !ok {
			t.Fatal("slot")
		}
		releases = append(releases, release)
	}
	if rec := le.query(le.adminTok, `{"query":"*"}`); rec.Code != 429 {
		t.Fatalf("4th query: %d", rec.Code)
	}
	if rec := le.query(le.memberTok, `{"query":"*"}`); rec.Code != 403 {
		t.Fatalf("other user: %d", rec.Code)
	}
	releases[0]()
	if rec := le.query(le.adminTok, `{"query":"*"}`); rec.Code != 200 {
		t.Fatalf("after release: %d", rec.Code)
	}
	for range 5 {
		if _, ok := le.api.logsTails.acquire(le.admin.ID); !ok {
			t.Fatal("tail slot")
		}
	}
	if rec := le.do("GET", le.path("/groups/node/"+le.node+"/entries/tail"), le.adminTok); rec.Code != 429 {
		t.Fatalf("6th tail: %d", rec.Code)
	}
}

func TestLogsHubWideCap(t *testing.T) {
	le := newLogsExplorerEnv(t)
	defer func(d time.Duration) { logsHubWait = d }(logsHubWait)
	logsHubWait = 20 * time.Millisecond
	for range cap(le.api.logsHub) {
		le.api.logsHub <- struct{}{}
	}
	for _, p := range []string{"/groups", "/groups/node/" + le.node + "/streams", "/groups/node/" + le.node + "/entries"} {
		if rec := le.do("GET", le.path(p), le.adminTok); rec.Code != 429 {
			t.Errorf("%s: %d", p, rec.Code)
		}
	}
	if rec := le.query(le.adminTok, `{"query":"*"}`); rec.Code != 429 {
		t.Errorf("query: %d", rec.Code)
	}
	if rec := le.do("GET", nodesPath(le.org)+"/"+le.node+"/logs", le.adminTok); rec.Code != 200 {
		t.Errorf("detail page shares the cap: %d", rec.Code)
	}
	if got := len(le.received()); got != 1 {
		t.Fatalf("upstream calls while full: %d", got)
	}
	<-le.api.logsHub
	if rec := le.query(le.adminTok, `{"query":"*"}`); rec.Code != 200 {
		t.Fatalf("after a slot frees: %d", rec.Code)
	}
}

func TestLogsImageStreamsBuildInfo(t *testing.T) {
	le := newLogsExplorerEnv(t)
	ctx := context.Background()
	img := le.createImage(le.adminTok, le.org, `{"name":"web","scope":"org"}`)
	b, err := le.store.CreateImageBuild(ctx, store.ImageBuild{ImageID: img, Revision: 1, StartedBy: le.admin.ID})
	if err != nil {
		t.Fatal(err)
	}
	gone := uuid.NewString()
	le.answer = func(string, url.Values) string {
		return `{"observatory.build":"` + b.ID.String() + `","last":"2026-11-21T10:00:00Z"}` + "\n" + `{"observatory.build":"` + gone + `","last":"2026-11-20T10:00:00Z"}`
	}
	rec := le.do("GET", le.path("/groups/image/"+img.String()+"/streams"), le.adminTok)
	if rec.Code != 200 {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}
	var got []struct {
		Name   string
		LastAt time.Time
		Build  *struct {
			StartedAt *time.Time
			Status    string
		}
	}
	decode(t, rec, &got)
	if len(got) != 2 || got[0].Name != b.ID.String() || got[0].Build == nil || got[0].Build.Status != b.State || got[1].Build != nil {
		t.Fatalf("%s", rec.Body.String())
	}
}

package api

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"runtime"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	collogspb "go.opentelemetry.io/proto/otlp/collector/logs/v1"
	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	"google.golang.org/protobuf/proto"

	"github.com/asterism-dev/observatory/internal/store"
	"github.com/asterism-dev/observatory/internal/telemetry"
)

type buildLogStore struct {
	mu     sync.Mutex
	reqs   []*collogspb.ExportLogsServiceRequest
	fields []string
}

func (f *buildLogStore) handler(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	req := &collogspb.ExportLogsServiceRequest{}
	if proto.Unmarshal(body, req) != nil {
		w.WriteHeader(400)
		return
	}
	f.mu.Lock()
	f.reqs = append(f.reqs, req)
	f.fields = append(f.fields, r.Header.Get("VL-Stream-Fields"))
	f.mu.Unlock()
}

type copiedLine struct {
	body  string
	attrs map[string]string
}

func (f *buildLogStore) lines() []copiedLine {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []copiedLine
	for _, req := range f.reqs {
		for _, rl := range req.ResourceLogs {
			attrs := map[string]string{}
			for _, kv := range rl.Resource.Attributes {
				attrs[kv.Key] = kv.Value.GetStringValue()
			}
			for _, sl := range rl.ScopeLogs {
				for _, r := range sl.LogRecords {
					out = append(out, copiedLine{r.Body.GetStringValue(), attrs})
				}
			}
		}
	}
	return out
}

// buildLogEnv returns an env whose build logs go to a fake store; flush stops the writer and waits for it.
func buildLogEnv(t *testing.T) (*buildEnv, *buildLogStore, func()) {
	t.Helper()
	fake := &buildLogStore{}
	srv := httptest.NewServer(http.HandlerFunc(fake.handler))
	t.Cleanup(srv.Close)
	w := telemetry.NewLogWriter(&telemetry.Client{LogsURL: srv.URL}, telemetry.BuildLogStreamFields, 10000)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { w.Run(ctx); close(done) }()
	stop := func() {
		cancel()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Fatal("log writer did not stop")
		}
	}
	t.Cleanup(cancel)
	return newEnv(t).withBuildLogs(w), fake, stop
}

func (e *buildEnv) postLog(b uuid.UUID, secret, body string) {
	e.t.Helper()
	if rec := e.agentDo("POST", "/api/builds/"+b.String()+"/log", secret, body); rec.Code != 204 {
		e.t.Fatalf("log: %d %s", rec.Code, rec.Body.String())
	}
}

func bodies(lines []copiedLine) []string {
	var out []string
	for _, l := range lines {
		out = append(out, l.body)
	}
	return out
}

func TestBuildLogCopied(t *testing.T) {
	e, fake, stop := buildLogEnv(t)
	_, tok := e.user("alice")
	org := e.createOrg(tok, "Acme")
	img := e.recipeImage(tok, org, "tools", "")
	b, secret := e.running(tok, org, img)
	e.postLog(b.ID, secret, "a\nb\nc")
	e.postLog(b.ID, secret, "d\r\n")
	stop()

	lines := fake.lines()
	if got := bodies(lines); !slices.Equal(got, []string{"a", "b", "cd"}) {
		t.Fatalf("bodies = %v", got)
	}
	want := map[string]string{
		"observatory.org": org.String(), "observatory.image": img.String(), "observatory.image_name": "tools",
		"observatory.image_scope": "org", "observatory.build": b.ID.String(), "observatory.stream": "build",
		"service.name": "observatory-builder",
	}
	for k, v := range want {
		if lines[0].attrs[k] != v {
			t.Fatalf("attr %s = %q, want %q", k, lines[0].attrs[k], v)
		}
	}
	if fake.fields[0] != telemetry.BuildLogStreamFields {
		t.Fatalf("stream fields %q", fake.fields[0])
	}
}

func TestBuildRecordsHaveNoNodeOrVM(t *testing.T) {
	e, fake, stop := buildLogEnv(t)
	_, tok := e.user("alice")
	org := e.createOrg(tok, "Acme")
	b, secret := e.running(tok, org, e.recipeImage(tok, org, "tools", ""))
	e.postLog(b.ID, secret, "x\n")
	stop()
	lines := fake.lines()
	if len(lines) != 1 {
		t.Fatalf("lines = %v", lines)
	}
	for k := range lines[0].attrs {
		if k == "observatory.node" || k == "observatory.vm" {
			t.Fatalf("build record has %s", k)
		}
	}
}

func TestBuildLogPartialFlushedOnFinish(t *testing.T) {
	e, fake, stop := buildLogEnv(t)
	_, tok := e.user("alice")
	org := e.createOrg(tok, "Acme")
	b, secret := e.running(tok, org, e.recipeImage(tok, org, "tools", ""))
	e.postLog(b.ID, secret, "one\ntail")
	if rec := e.agentDo("POST", "/api/builds/"+b.ID.String()+"/result", secret, `{"error":"boom"}`); rec.Code != 204 {
		t.Fatalf("result: %d %s", rec.Code, rec.Body.String())
	}
	stop()
	if got := bodies(fake.lines()); !slices.Equal(got, []string{"one", "tail"}) {
		t.Fatalf("bodies = %v", got)
	}
}

func TestBuildLogPrivateNotCopied(t *testing.T) {
	e, fake, stop := buildLogEnv(t)
	_, tok := e.user("alice")
	org := e.createOrg(tok, "Acme")
	img := e.createImage(tok, org, `{"name":"mine","scope":"user"}`)
	if rec := e.putRecipe(tok, org, img, recipeTar(t, dockerfile), ""); rec.Code != 200 {
		t.Fatalf("recipe: %d", rec.Code)
	}
	b, secret := e.running(tok, org, img)
	e.postLog(b.ID, secret, "secret\ntail")
	e.agentDo("POST", "/api/builds/"+b.ID.String()+"/result", secret, `{"error":"boom"}`)
	stop()
	if lines := fake.lines(); len(lines) != 0 {
		t.Fatalf("private build copied: %v", lines)
	}
}

func TestBuildLogTeamImageCopied(t *testing.T) {
	e, fake, stop := buildLogEnv(t)
	_, tok := e.user("alice")
	org := e.createOrg(tok, "Acme")
	team := e.createTeam(org, "core")
	img := e.createImage(tok, org, `{"name":"teamimg","scope":"team","teamId":"`+team.String()+`"}`)
	if rec := e.putRecipe(tok, org, img, recipeTar(t, dockerfile), ""); rec.Code != 200 {
		t.Fatalf("recipe: %d", rec.Code)
	}
	b, secret := e.running(tok, org, img)
	e.postLog(b.ID, secret, "hi\n")
	stop()
	lines := fake.lines()
	if len(lines) != 1 || lines[0].attrs["observatory.image_scope"] != "team" {
		t.Fatalf("lines = %v", lines)
	}
}

func TestBuildLogCopiesOnlyStoredBytes(t *testing.T) {
	e, fake, stop := buildLogEnv(t)
	_, tok := e.user("alice")
	org := e.createOrg(tok, "Acme")
	b, secret := e.running(tok, org, e.recipeImage(tok, org, "tools", ""))
	fill := make([]byte, 1<<20)
	for range store.BuildLogLiveCap/len(fill) - 1 {
		if _, err := e.store.AppendBuildLog(context.Background(), b.ID, fill); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := e.store.AppendBuildLog(context.Background(), b.ID, fill[:len(fill)-6]); err != nil {
		t.Fatal(err)
	}
	e.postLog(b.ID, secret, "one\ntwo\nthree\n")
	e.postLog(b.ID, secret, "four\n")
	e.agentDo("POST", "/api/builds/"+b.ID.String()+"/result", secret, `{"error":"boom"}`)
	stop()
	if got := bodies(fake.lines()); !slices.Equal(got, []string{"one", "tw"}) {
		t.Fatalf("bodies = %v", got)
	}
}

func TestBuildLogStopsAfterRescopeToPrivate(t *testing.T) {
	old := copyScopeRefresh
	copyScopeRefresh = 0
	t.Cleanup(func() { copyScopeRefresh = old })
	e, fake, stop := buildLogEnv(t)
	_, tok := e.user("alice")
	org := e.createOrg(tok, "Acme")
	img := e.recipeImage(tok, org, "tools", "")
	b, secret := e.running(tok, org, img)
	e.postLog(b.ID, secret, "public\npart")
	e.exec("UPDATE images SET scope = 'user', user_id = created_by WHERE id = $1", img)
	e.postLog(b.ID, secret, "ial\nprivate\n")
	e.agentDo("POST", "/api/builds/"+b.ID.String()+"/result", secret, `{"error":"boom"}`)
	stop()
	if got := bodies(fake.lines()); !slices.Equal(got, []string{"public"}) {
		t.Fatalf("bodies = %v", got)
	}
}

func TestBuildLogTelemetryOff(t *testing.T) {
	e := newEnv(t).withBuilds()
	_, tok := e.user("alice")
	org := e.createOrg(tok, "Acme")
	b, secret := e.running(tok, org, e.recipeImage(tok, org, "tools", ""))
	e.postLog(b.ID, secret, "x\ny")
	if rec := e.agentDo("POST", "/api/builds/"+b.ID.String()+"/result", secret, `{"error":"boom"}`); rec.Code != 204 {
		t.Fatalf("result: %d", rec.Code)
	}
	rec := e.do("GET", buildsPath(org, e.build(b.ID).ImageID)+"/"+b.ID.String()+"/log", tok)
	if rec.Code != 200 || rec.Body.String() != "x\ny" {
		t.Fatalf("log: %d %q", rec.Code, rec.Body.String())
	}
}

func TestBuildLogNewlineFloodBounded(t *testing.T) {
	w := telemetry.NewLogWriter(&telemetry.Client{LogsURL: "http://127.0.0.1:1"}, "a", copyMaxLines)
	c := newBuildLogCopier(w, nil)
	b := store.ImageBuild{ID: uuid.New()}
	now := time.Now()
	c.builds[b.ID] = &copiedBuild{resource: []*commonpb.KeyValue{attr("k", "v")}, checked: now, seen: now}
	flood := make([]byte, 8<<20)
	for i := range flood {
		flood[i] = '\n'
	}
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	c.chunk(context.Background(), b, flood)
	runtime.ReadMemStats(&after)
	if n := w.Queued(); n != copyMaxLines {
		t.Fatalf("queued = %d", n)
	}
	if d := after.TotalAlloc - before.TotalAlloc; d > 40<<20 {
		t.Fatalf("allocated %d bytes", d)
	}
}

func TestLastLines(t *testing.T) {
	for _, c := range []struct {
		in   string
		n    int
		want string
	}{{"a\nb\nc", 2, "b\nc"}, {"a\nb\nc", 1, "c"}, {"a\nb\nc", 3, "a\nb\nc"}, {"a\nb\nc", 9, "a\nb\nc"}, {"", 2, ""}} {
		if got := lastLines(c.in, c.n); got != c.want {
			t.Fatalf("lastLines(%q,%d) = %q, want %q", c.in, c.n, got, c.want)
		}
	}
}

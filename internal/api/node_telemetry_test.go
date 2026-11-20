package api

import (
	"bufio"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/asterism-dev/observatory/internal/store"
	"github.com/asterism-dev/observatory/internal/telemetry"
)

type telemetryEnv struct {
	*env
	org         uuid.UUID
	node        string
	ownerTok    string
	memberTok   string
	lastLogForm url.Values
}

// newTelemetryEnv wires a fake store; a metrics response of "" makes the store answer 500.
func newTelemetryEnv(t *testing.T, metrics string) *telemetryEnv {
	t.Helper()
	te := &telemetryEnv{env: newEnv(t)}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		switch r.URL.Path {
		case "/api/v1/query_range":
			if metrics == "" {
				http.Error(w, "down", http.StatusInternalServerError)
				return
			}
			_, _ = w.Write([]byte(metrics))
		case "/select/logsql/query":
			te.lastLogForm = r.PostForm
			_, _ = w.Write([]byte(`{"_time":"2026-01-01T00:00:00Z","_msg":"hi","severity_text":"INFO","source":"firecracker"}` + "\n"))
		case "/select/logsql/tail":
			_, _ = w.Write([]byte(`{"_time":"2026-01-01T00:00:00Z","_msg":"tailed","severity_text":"WARN","source":"observatory-node"}` + "\n"))
		}
	}))
	t.Cleanup(srv.Close)
	te.api.telemetry = &telemetry.Client{MetricsURL: srv.URL, LogsURL: srv.URL}
	_, te.ownerTok = te.user("alice")
	bob, bobTok := te.user("bob")
	te.memberTok = bobTok
	te.org = te.createOrg(te.ownerTok, "Acme")
	te.addMember(te.org, bob, store.RoleMember)
	te.node = te.createNode(te.ownerTok, te.org, "n1").Node.ID
	return te
}

func (te *telemetryEnv) path(suffix string) string { return nodesPath(te.org) + "/" + te.node + suffix }

const cpuSeries = `{"data":{"result":[{"metric":{"state":"user"},"values":[[1000,"1.5"],[1010,"2"]]}]}}`

func TestNodeUsageForbiddenForMember(t *testing.T) {
	te := newTelemetryEnv(t, cpuSeries)
	if rec := te.do("GET", te.path("/usage"), te.memberTok); rec.Code != 403 {
		t.Fatalf("member: %d", rec.Code)
	}
}

func TestNodeUsageAdmin(t *testing.T) {
	te := newTelemetryEnv(t, cpuSeries)
	rec := te.do("GET", te.path("/usage"), te.ownerTok)
	if rec.Code != 200 {
		t.Fatalf("status %d %s", rec.Code, rec.Body.String())
	}
	var u telemetry.Usage
	decode(t, rec, &u)
	if len(u.T) != 2 || len(u.Charts["cpu"]) != 1 || u.Charts["cpu"][0].Name != "user" {
		t.Fatalf("%+v", u)
	}
}

func TestNodeUsageEmpty(t *testing.T) {
	te := newTelemetryEnv(t, `{"data":{"result":[]}}`)
	rec := te.do("GET", te.path("/usage"), te.ownerTok)
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), `"t":[]`) {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}
}

func TestNodeUsageStoreDown(t *testing.T) {
	te := newTelemetryEnv(t, "")
	rec := te.do("GET", te.path("/usage"), te.ownerTok)
	if rec.Code != 502 || !strings.Contains(rec.Body.String(), "telemetry store unavailable") {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}
}

func TestNodeUsageDisabled(t *testing.T) {
	te := newTelemetryEnv(t, cpuSeries)
	te.api.telemetry = nil
	for _, s := range []string{"/usage", "/logs", "/logs/tail"} {
		if rec := te.do("GET", te.path(s), te.ownerTok); rec.Code != 404 {
			t.Fatalf("%s: %d", s, rec.Code)
		}
	}
}

func TestNodeLogsParams(t *testing.T) {
	te := newTelemetryEnv(t, cpuSeries)
	rec := te.do("GET", te.path("/logs?q=boom&level=warn&source=firecracker&field=vm:a:b&field=k:v&limit=5"), te.ownerTok)
	if rec.Code != 200 {
		t.Fatalf("status %d %s", rec.Code, rec.Body.String())
	}
	var got struct{ Lines []telemetry.LogLine }
	decode(t, rec, &got)
	if len(got.Lines) != 1 || got.Lines[0].Msg != "hi" {
		t.Fatalf("%+v", got)
	}
	q := te.lastLogForm.Get("query")
	for _, want := range []string{`"vm":="a:b"`, `"k":="v"`, `"severity_text":="WARN"`, `source:="firecracker"`, `*:"boom"`} {
		if !strings.Contains(q, want) {
			t.Fatalf("query %q lacks %s", q, want)
		}
	}
	if te.lastLogForm.Get("limit") != "5" {
		t.Fatalf("limit %q", te.lastLogForm.Get("limit"))
	}
	now := time.Now().UTC()
	tooLong := "from=" + now.Add(-32*24*time.Hour).Format(time.RFC3339) + "&to=" + now.Format(time.RFC3339)
	for _, bad := range []string{"from=yesterday", tooLong, "level=trace", "field=:x", "limit=0", "limit=2001"} {
		if rec := te.do("GET", te.path("/logs?"+bad), te.ownerTok); rec.Code != 400 {
			t.Fatalf("%s: %d", bad, rec.Code)
		}
	}
}

func TestNodeLogsTailSSE(t *testing.T) {
	te := newTelemetryEnv(t, cpuSeries)
	srv := httptest.NewServer(te.h)
	t.Cleanup(srv.Close)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "GET", srv.URL+te.path("/logs/tail"), nil)
	req.Header.Set("Authorization", "Bearer "+te.ownerTok)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 || resp.Header.Get("Content-Type") != "text/event-stream" {
		t.Fatalf("%d %s", resp.StatusCode, resp.Header.Get("Content-Type"))
	}
	line, err := bufio.NewReader(resp.Body).ReadString('\n')
	if err != nil || !strings.HasPrefix(line, "data: ") {
		t.Fatalf("%q %v", line, err)
	}
	var l telemetry.LogLine
	if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &l); err != nil || l.Msg != "tailed" || l.Level != "warn" {
		t.Fatalf("%+v %v", l, err)
	}
}

func TestNodeJSONCanTelemetry(t *testing.T) {
	te := newTelemetryEnv(t, cpuSeries)
	can := func(tok string) bool {
		rec := te.do("GET", te.path(""), tok)
		var j struct{ CanTelemetry bool }
		decode(t, rec, &j)
		return j.CanTelemetry
	}
	if !can(te.ownerTok) || can(te.memberTok) {
		t.Fatal("admin must be true, member false")
	}
	te.api.telemetry = nil
	if can(te.ownerTok) {
		t.Fatal("client nil must be false")
	}
}

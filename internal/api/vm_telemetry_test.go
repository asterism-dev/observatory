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

	"github.com/asterism-dev/observatory/internal/authz"
	"github.com/asterism-dev/observatory/internal/store"
	"github.com/asterism-dev/observatory/internal/telemetry"
)

type vmTelemetryEnv struct {
	*env
	org                          uuid.UUID
	vm                           string
	ownerTok, adminTok, otherTok string
	other                        store.User
	lastLogForm                  url.Values
}

// newVMTelemetryEnv: bob (member) owns the VM, alice owns the org, carol is another member.
func newVMTelemetryEnv(t *testing.T) *vmTelemetryEnv {
	t.Helper()
	te := &vmTelemetryEnv{env: newEnv(t)}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		q := r.PostForm.Get("query")
		switch r.URL.Path {
		case "/api/v1/query_range":
			_, _ = w.Write([]byte(cpuSeries))
		case "/api/v1/query":
			val := "2097152"
			switch {
			case strings.Contains(q, "dropped"):
				_, _ = w.Write([]byte(`{"data":{"result":[{"metric":{"kind":"logs"},"value":[1000,"3"]}]}}`))
				return
			case strings.Contains(q, "timestamp("):
				val = "1700000000"
			case strings.Contains(q, "system_cpu_time"):
				val = "12.5"
			}
			_, _ = w.Write([]byte(`{"data":{"result":[{"metric":{"observatory_vm":"` + te.vm + `"},"value":[1000,"` + val + `"]}]}}`))
		case "/select/logsql/query":
			te.lastLogForm = r.PostForm
			_, _ = w.Write([]byte(`{"_time":"2026-01-01T00:00:00Z","_msg":"hi","severity_text":"INFO","observatory.stream":"egress"}` + "\n"))
		case "/select/logsql/tail":
			_, _ = w.Write([]byte(`{"_time":"2026-01-01T00:00:00Z","_msg":"tailed","severity_text":"WARN","observatory.stream":"app"}` + "\n"))
		}
	}))
	t.Cleanup(srv.Close)
	te.api.telemetry = &telemetry.Client{MetricsURL: srv.URL, LogsURL: srv.URL}
	_, te.adminTok = te.user("alice")
	bob, bobTok := te.user("bob")
	carol, carolTok := te.user("carol")
	te.ownerTok, te.otherTok, te.other = bobTok, carolTok, carol
	te.org = te.createOrg(te.adminTok, "Acme")
	te.addMember(te.org, bob, store.RoleMember)
	te.addMember(te.org, carol, store.RoleMember)
	node := te.enrolledNode(te.adminTok, te.org, "n1")
	te.vm = te.createVM(bobTok, te.org, vmBody("a", userOwner(), node)).ID
	return te
}

func (te *vmTelemetryEnv) path(suffix string) string { return vmsPath(te.org) + "/" + te.vm + suffix }

func TestVMUsageForbidden(t *testing.T) {
	te := newVMTelemetryEnv(t)
	for _, s := range []string{"/usage", "/logs", "/logs/tail", "/processes"} {
		if rec := te.do("GET", te.path(s), te.otherTok); rec.Code != 403 {
			t.Fatalf("%s: %d", s, rec.Code)
		}
	}
}

func TestVMUsageOwner(t *testing.T) {
	te := newVMTelemetryEnv(t)
	rec := te.do("GET", te.path("/usage"), te.ownerTok)
	if rec.Code != 200 {
		t.Fatalf("status %d %s", rec.Code, rec.Body.String())
	}
	var u struct {
		telemetry.Usage
		Dropped map[string]int64
	}
	decode(t, rec, &u)
	if len(u.T) != 2 || len(u.Charts["cpu"]) != 1 {
		t.Fatalf("%+v", u)
	}
	if u.Dropped["logs"] != 3 || !strings.Contains(rec.Body.String(), `"dropped":{"body":0,"logs":3,"series":0}`) {
		t.Fatalf("dropped: %s", rec.Body.String())
	}
	if rec := te.do("GET", te.path("/processes"), te.ownerTok); rec.Code != 501 {
		t.Fatalf("processes: %d", rec.Code)
	}
}

func TestVMUsageAdmin(t *testing.T) {
	te := newVMTelemetryEnv(t)
	if rec := te.do("GET", te.path("/usage"), te.adminTok); rec.Code != 200 {
		t.Fatalf("status %d %s", rec.Code, rec.Body.String())
	}
	te.api.telemetry = nil
	for _, s := range []string{"/usage", "/logs", "/logs/tail"} {
		if rec := te.do("GET", te.path(s), te.adminTok); rec.Code != 404 {
			t.Fatalf("disabled %s: %d", s, rec.Code)
		}
	}
}

func TestVMLogsStreamParam(t *testing.T) {
	te := newVMTelemetryEnv(t)
	if rec := te.do("GET", te.path("/logs?stream=bogus"), te.ownerTok); rec.Code != 400 {
		t.Fatalf("bad stream: %d", rec.Code)
	}
	rec := te.do("GET", te.path("/logs?stream=egress&q=boom&limit=5"), te.ownerTok)
	if rec.Code != 200 {
		t.Fatalf("status %d %s", rec.Code, rec.Body.String())
	}
	var got struct{ Lines []telemetry.LogLine }
	decode(t, rec, &got)
	if len(got.Lines) != 1 || got.Lines[0].Stream != "egress" {
		t.Fatalf("%+v", got)
	}
	q := te.lastLogForm.Get("query")
	for _, want := range []string{`"observatory.stream":="egress"`, `*:"boom"`} {
		if !strings.Contains(q, want) {
			t.Fatalf("query %q lacks %s", q, want)
		}
	}
	if f := te.lastLogForm.Get("extra_filters"); !strings.Contains(f, `observatory.vm:="`+te.vm+`"`) {
		t.Fatalf("filters %q", f)
	}
	if te.lastLogForm.Get("limit") != "5" {
		t.Fatalf("limit %q", te.lastLogForm.Get("limit"))
	}
}

func TestVMLogsTailSSE(t *testing.T) {
	te := newVMTelemetryEnv(t)
	srv := httptest.NewServer(te.h)
	t.Cleanup(srv.Close)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "GET", srv.URL+te.path("/logs/tail?stream=app"), nil)
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
	if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &l); err != nil || l.Msg != "tailed" || l.Stream != "app" {
		t.Fatalf("%+v %v", l, err)
	}
}

type vmUsageTest struct {
	CanTelemetry bool
	Usage        *struct {
		At                                             time.Time
		CPUPercent                                     float64
		MemUsedMB, MemTotalMB, DiskUsedMB, DiskTotalMB int
	}
}

func TestVMJSONUsageFromStore(t *testing.T) {
	te := newVMTelemetryEnv(t)
	var list struct{ VMs []vmUsageTest }
	decode(t, te.do("GET", vmsPath(te.org), te.ownerTok), &list)
	var one vmUsageTest
	decode(t, te.do("GET", te.path(""), te.ownerTok), &one)
	for _, v := range []vmUsageTest{list.VMs[0], one} {
		u := v.Usage
		if !v.CanTelemetry || u == nil || u.CPUPercent != 12.5 || u.MemUsedMB != 2 || u.DiskTotalMB != 2 || !u.At.Equal(time.Unix(1700000000, 0)) {
			t.Fatalf("owner: %+v %+v", v, u)
		}
	}

	// A custom role can grant vm:read without vm:metrics.
	reader := te.createRole(te.adminTok, te.org, `{"name":"vm reader","rules":[{"effect":"allow","object":"org/vms/*","action":"vm:read"}]}`)
	te.bind(te.adminTok, te.org, authz.UserSubject(te.other.ID), reader)
	rec := te.do("GET", vmsPath(te.org), te.otherTok)
	list.VMs = nil
	decode(t, rec, &list)
	if len(list.VMs) != 1 || list.VMs[0].CanTelemetry || list.VMs[0].Usage != nil || strings.Contains(rec.Body.String(), `"usage"`) {
		t.Fatalf("reader: %s", rec.Body.String())
	}
}

func TestVMMetricsRouteGone(t *testing.T) {
	te := newVMTelemetryEnv(t)
	if rec := te.do("GET", te.path("/metrics"), te.ownerTok); rec.Code != 404 {
		t.Fatalf("%d", rec.Code)
	}
}

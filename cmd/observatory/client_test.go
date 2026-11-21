package main

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"reflect"
	"strings"
	"testing"
	"time"

	iamv1 "github.com/asterism-dev/observatory/gen/go/observatory/iam/v1"
)

func TestCallSendsBearerAndMapsErrors(t *testing.T) {
	const infoBody = `{"docsURL":"https://docs","version":"1.0.0","latestVersion":"1.0.0"}`
	h := newFakeHub(t)
	h.on("GET /api/info", 200, infoBody)
	h.on("POST /api/orgs/o1/devices", 400, `{"error":"invalid name"}`)
	c, _, _ := newTestCLI(t, h)

	var info struct{ Version string }
	if err := c.call("GET", "/api/info", nil, &info); err != nil {
		t.Fatal(err)
	}
	if info.Version != "1.0.0" {
		t.Fatalf("decoded %+v", info)
	}
	if got := h.calls("GET /api/info")[0].Auth; got != "Bearer tok-test" {
		t.Fatalf("auth %q", got)
	}

	t.Setenv("OBSERVATORY_TOKEN", "env-tok")
	var raw json.RawMessage
	if err := c.call("GET", "/api/info", nil, &raw); err != nil {
		t.Fatal(err)
	}
	if got := h.calls("GET /api/info")[1].Auth; got != "Bearer env-tok" {
		t.Fatalf("auth %q, want env token", got)
	}
	if string(raw) != infoBody {
		t.Fatalf("raw %s", raw)
	}

	err := c.call("POST", "/api/orgs/o1/devices", map[string]string{"name": ""}, nil)
	var ae *apiError
	if !errors.As(err, &ae) || *ae != (apiError{Status: 400, Msg: "invalid name"}) {
		t.Fatalf("got %#v", err)
	}
	if got := h.calls("POST /api/orgs/o1/devices")[0].Body; got != `{"name":""}` {
		t.Fatalf("body %q", got)
	}

	t.Setenv("OBSERVATORY_TOKEN", "")
	c.cfg.Token = ""
	n := len(h.reqs)
	if err := c.call("GET", "/api/info", nil, nil); exitCode(err) != 3 {
		t.Fatalf("no token: exit %d (%v)", exitCode(err), err)
	}
	if len(h.reqs) != n {
		t.Fatal("request sent without a token")
	}
}

func TestTokenExpiryWarning(t *testing.T) {
	c, _, stderr := newTestCLI(t, nil)
	c.cfg.ExpiresAt = testNow.Add(48 * time.Hour)
	for range 2 {
		if _, err := c.token(); err != nil {
			t.Fatal(err)
		}
	}
	if got := stderr.String(); strings.Count(got, "session expires in 2 days, run observatory login") != 1 {
		t.Fatalf("stderr %q, want the warning once", got)
	}

	c, _, stderr = newTestCLI(t, nil)
	c.cfg.ExpiresAt = testNow.Add(10 * 24 * time.Hour)
	if _, err := c.token(); err != nil || stderr.Len() != 0 {
		t.Fatalf("err %v stderr %q", err, stderr.String())
	}
}

const meTwoOrgsBody = `{"user":{"id":"u1","email":"ada@example.com","name":"Ada Lovelace","platformAdmin":false},"orgs":[` +
	`{"id":"o1","name":"orga","personal":false,"role":"owner"},{"id":"o2","name":"orgc","personal":false,"role":"member"}]}`

func TestOrgSelectionPrecedence(t *testing.T) {
	h := newFakeHub(t)
	h.on(iamRPC+"GetMe", 200, meTwoOrgsBody)
	h.on(iamRPC+"GetOrg", 200, `{"org":{"id":"o1","name":"orga","role":"owner"}}`)

	shows := func(t *testing.T, c *cli, args ...string) string {
		t.Helper()
		before := len(h.calls(iamRPC + "GetOrg"))
		if code := run(c, append([]string{"org", "show"}, args...)); code != 0 {
			t.Fatalf("exit %d", code)
		}
		calls := h.calls(iamRPC + "GetOrg")
		if len(calls) != before+1 {
			t.Fatal("no org fetched")
		}
		var req struct{ Org string }
		if err := json.Unmarshal([]byte(calls[before].Body), &req); err != nil {
			t.Fatal(err)
		}
		return req.Org
	}

	c, _, stderr := newTestCLI(t, h)
	if code := run(c, []string{"org", "show"}); code != 2 {
		t.Fatalf("nothing chosen: exit %d", code)
	}
	for _, s := range []string{"orga", "orgc", "org use"} {
		if !strings.Contains(stderr.String(), s) {
			t.Errorf("stderr %q lacks %q", stderr.String(), s)
		}
	}
	if n := len(h.calls(iamRPC + "GetOrg")); n != 0 {
		t.Fatalf("acted on an org without a choice")
	}

	c, _, _ = newTestCLI(t, h)
	if err := saveConfig(c.cfgDir, config{Hub: h.URL, Token: "tok-test", Org: "o2"}); err != nil {
		t.Fatal(err)
	}
	if got := shows(t, c); got != "o2" {
		t.Errorf("config org: %s", got)
	}
	t.Setenv("OBSERVATORY_ORG", "orga")
	c, _, _ = newTestCLI(t, h)
	t.Setenv("OBSERVATORY_ORG", "orga")
	if err := saveConfig(c.cfgDir, config{Hub: h.URL, Token: "tok-test", Org: "o2"}); err != nil {
		t.Fatal(err)
	}
	if got := shows(t, c); got != "o1" {
		t.Errorf("env beats config: %s", got)
	}
	c, _, _ = newTestCLI(t, h)
	t.Setenv("OBSERVATORY_ORG", "orga")
	if got := shows(t, c, "--org", "o2"); got != "o2" {
		t.Errorf("flag beats env: %s", got)
	}
	c, _, _ = newTestCLI(t, h)
	if code := run(c, []string{"org", "show", "--org", "nope"}); code != 5 {
		t.Errorf("unknown org: exit %d", code)
	}

	h1 := newFakeHub(t)
	meOneOrg(h1)
	c, _, _ = newTestCLI(t, h1)
	if o, err := c.currentOrg(); err != nil || o.GetId() != "o1" {
		t.Errorf("single org: %+v %v", o, err)
	}
	if p, err := c.orgPath("/vms"); err != nil || p != "/api/orgs/o1/vms" {
		t.Errorf("orgPath %q %v", p, err)
	}
	if n := len(h1.calls(iamRPC + "GetMe")); n != 1 {
		t.Errorf("GetMe called %d times, want cached", n)
	}
}

const listEventsPath = "/api/audit/v1/observatory.audit.v1.AuditService/ListEvents"

func TestOrgAuditQuery(t *testing.T) {
	h := newFakeHub(t)
	meOneOrg(h)
	h.onFunc("POST "+listEventsPath, func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Content-Type") != "application/json" {
			http.Error(w, "content type "+r.Header.Get("Content-Type"), http.StatusUnsupportedMediaType)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"events":[{"id":"0190f5a2-7c3b-7d4e-8f60-123456789abc","time":"2026-11-19T13:58:00Z","type":"compute.instances.delete","org":"o1","actor":{"kind":"ACTOR_KIND_USER","id":"u1"},"resource":"vm/v1","result":"RESULT_DENIED","clientIp":"203.0.113.7","audit":true}],"nextPageToken":"n"}`)
	})
	c, stdout, _ := newTestCLI(t, h)
	if code := run(c, []string{"org", "audit", "--limit", "5", "--action", "compute.instances.delete", "--actor", "u1", "--since", "1h"}); code != 0 {
		t.Fatalf("exit %d", code)
	}
	call := h.calls("POST " + listEventsPath)[0]
	var got map[string]any
	if err := json.Unmarshal([]byte(call.Body), &got); err != nil {
		t.Fatal(err)
	}
	want := map[string]any{"org": "o1", "pageSize": 5.0, "filter": map[string]any{"type": "compute.instances.delete", "actorId": "u1", "from": testNow.Add(-time.Hour).Format(time.RFC3339)}}
	if !reflect.DeepEqual(got, want) || call.Auth != "Bearer tok-test" {
		t.Fatalf("request %s (auth %q), want %v", call.Body, call.Auth, want)
	}
	if out := stdout.String(); !strings.Contains(out, "compute.instances.delete") || !strings.Contains(out, "5 min ago") || !strings.Contains(out, "denied") || !strings.Contains(out, "u1") {
		t.Errorf("stdout %q", out)
	}
}

func TestOrgAuditConnectError(t *testing.T) {
	h := newFakeHub(t)
	meOneOrg(h)
	h.on("POST "+listEventsPath, http.StatusForbidden, `{"code":"permission_denied","message":"permission denied"}`)
	c, _, stderr := newTestCLI(t, h)
	if code := run(c, []string{"org", "audit"}); code != 4 || !strings.Contains(stderr.String(), "permission denied") || strings.Contains(stderr.String(), "{") {
		t.Fatalf("exit %d, stderr %q", code, stderr.String())
	}
}

func TestFormatHelpers(t *testing.T) {
	for _, tc := range []struct {
		d    time.Duration
		want string
	}{{10 * time.Second, "just now"}, {5 * time.Minute, "5 min ago"}, {3 * time.Hour, "3 h ago"}, {48 * time.Hour, "2026-11-17 14:03"}} {
		if got := ago(testNow, testNow.Add(-tc.d)); got != tc.want {
			t.Errorf("ago(%v) = %q, want %q", tc.d, got, tc.want)
		}
	}
	for mb, want := range map[int]string{512: "512 MB", 4096: "4 GB", 1536: "1.5 GB"} {
		if got := gb(mb); got != want {
			t.Errorf("gb(%d) = %q, want %q", mb, got, want)
		}
	}
}

func TestStoredTokenOnlyForItsHub(t *testing.T) {
	a, b := newFakeHub(t), newFakeHub(t)
	c, _, stderr := newTestCLI(t, a)
	if code := run(c, []string{"whoami", "--hub", b.URL}); code != 3 || !strings.Contains(stderr.String(), "not logged in to "+b.URL) {
		t.Fatalf("exit %d, stderr %q", code, stderr.String())
	}
	c, _, _ = newTestCLI(t, a)
	t.Setenv("OBSERVATORY_HUB", b.URL+"/")
	if code := run(c, []string{"whoami"}); code != 3 {
		t.Fatalf("OBSERVATORY_HUB: exit %d", code)
	}
	if len(b.reqs) != 0 {
		t.Fatalf("stored token sent to another hub: %+v", b.reqs)
	}

	b.on(iamRPC+"GetMe", 200, meOneOrgBody)
	c, _, _ = newTestCLI(t, a)
	t.Setenv("OBSERVATORY_HUB", b.URL)
	t.Setenv("OBSERVATORY_TOKEN", "env-tok")
	if code := run(c, []string{"whoami"}); code != 0 || b.calls(iamRPC + "GetMe")[0].Auth != "Bearer env-tok" {
		t.Fatalf("OBSERVATORY_TOKEN is not bound to the stored hub: exit %d", code)
	}
}

func TestRPCErrorMessage(t *testing.T) {
	h := newFakeHub(t)
	h.on(iamRPC+"CreateOrg", 400, `{"code":"invalid_argument","message":"invalid name"}`)
	h.on(iamRPC+"GetTeam", 404, `{"code":"not_found","message":"team not found"}`)
	c, _, _ := newTestCLI(t, h)
	err := c.rpc("CreateOrg", &iamv1.CreateOrgRequest{Name: ""}, &iamv1.CreateOrgResponse{})
	if err == nil || err.Error() != "invalid name" || exitCode(err) != 1 {
		t.Fatalf("got %v (exit %d)", err, exitCode(err))
	}
	if got := h.calls(iamRPC + "CreateOrg")[0]; got.Auth != "Bearer tok-test" || compactJSON(t, got.Body) != `{}` {
		t.Fatalf("request %+v", got)
	}
	if err := c.rpc("GetTeam", &iamv1.GetTeamRequest{Org: "o1", Team: "t9"}, &iamv1.GetTeamResponse{}); err == nil || err.Error() != "team not found" || exitCode(err) != 5 {
		t.Fatalf("got %v (exit %d)", err, exitCode(err))
	}
}

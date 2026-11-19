package main

import (
	"encoding/json"
	"errors"
	"net/url"
	"strings"
	"testing"
	"time"
)

func TestCallSendsBearerAndMapsErrors(t *testing.T) {
	h := newFakeHub(t)
	h.on("GET /api/me", 200, meOneOrgBody)
	h.on("POST /api/orgs", 400, `{"error":"invalid name"}`)
	c, _, _ := newTestCLI(t, h)

	var me meJSON
	if err := c.call("GET", "/api/me", nil, &me); err != nil {
		t.Fatal(err)
	}
	if me.User.Email != "ada@example.com" || len(me.Orgs) != 1 || me.Orgs[0].Role != "owner" {
		t.Fatalf("decoded %+v", me)
	}
	if got := h.calls("GET /api/me")[0].Auth; got != "Bearer tok-test" {
		t.Fatalf("auth %q", got)
	}

	t.Setenv("OBSERVATORY_TOKEN", "env-tok")
	var raw json.RawMessage
	if err := c.call("GET", "/api/me", nil, &raw); err != nil {
		t.Fatal(err)
	}
	if got := h.calls("GET /api/me")[1].Auth; got != "Bearer env-tok" {
		t.Fatalf("auth %q, want env token", got)
	}
	if string(raw) != meOneOrgBody {
		t.Fatalf("raw %s", raw)
	}

	err := c.call("POST", "/api/orgs", map[string]string{"name": ""}, nil)
	var ae *apiError
	if !errors.As(err, &ae) || *ae != (apiError{400, "invalid name"}) {
		t.Fatalf("got %#v", err)
	}
	if got := h.calls("POST /api/orgs")[0].Body; got != `{"name":""}` {
		t.Fatalf("body %q", got)
	}

	t.Setenv("OBSERVATORY_TOKEN", "")
	c.cfg.Token = ""
	n := len(h.reqs)
	if err := c.call("GET", "/api/me", nil, nil); exitCode(err) != 3 {
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
	h.on("GET /api/me", 200, meTwoOrgsBody)
	h.on("GET /api/orgs/o1", 200, `{"id":"o1","name":"orga","personal":false,"role":"owner","permissions":[]}`)
	h.on("GET /api/orgs/o2", 200, `{"id":"o2","name":"orgc","personal":false,"role":"member","permissions":[]}`)

	shows := func(t *testing.T, c *cli, args ...string) string {
		t.Helper()
		before := len(h.calls("GET /api/orgs/o1")) + 2*len(h.calls("GET /api/orgs/o2"))
		if code := run(c, append([]string{"org", "show"}, args...)); code != 0 {
			t.Fatalf("exit %d", code)
		}
		switch len(h.calls("GET /api/orgs/o1")) + 2*len(h.calls("GET /api/orgs/o2")) - before {
		case 1:
			return "o1"
		case 2:
			return "o2"
		}
		t.Fatal("no org fetched")
		return ""
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
	if n := len(h.calls("GET /api/orgs/o1")) + len(h.calls("GET /api/orgs/o2")); n != 0 {
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
	if o, err := c.currentOrg(); err != nil || o.ID != "o1" {
		t.Errorf("single org: %+v %v", o, err)
	}
	if p, err := c.orgPath("/vms"); err != nil || p != "/api/orgs/o1/vms" {
		t.Errorf("orgPath %q %v", p, err)
	}
	if n := len(h1.calls("GET /api/me")); n != 1 {
		t.Errorf("/api/me fetched %d times, want cached", n)
	}
}

func TestOrgAuditQuery(t *testing.T) {
	h := newFakeHub(t)
	meOneOrg(h)
	h.on("GET /api/orgs/o1/audit", 200, `{"entries":[{"id":7,"at":"2026-11-19T13:58:00Z","actorKind":"user","actorId":"u1","orgId":"o1","action":"vm.delete","resource":"vm/v1","result":"allowed","ip":"","userAgent":"","details":null}],"next":null}`)
	c, stdout, _ := newTestCLI(t, h)
	if code := run(c, []string{"org", "audit", "--limit", "5", "--action", "vm.delete", "--since", "1h"}); code != 0 {
		t.Fatalf("exit %d", code)
	}
	q, err := url.ParseQuery(h.calls("GET /api/orgs/o1/audit")[0].Query)
	if err != nil {
		t.Fatal(err)
	}
	want := url.Values{"limit": {"5"}, "action": {"vm.delete"}, "from": {testNow.Add(-time.Hour).Format(time.RFC3339)}}
	if q.Encode() != want.Encode() {
		t.Fatalf("query %v, want %v", q, want)
	}
	if !strings.Contains(stdout.String(), "vm.delete") || !strings.Contains(stdout.String(), "5 min ago") {
		t.Errorf("stdout %q", stdout.String())
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

	b.on("GET /api/me", 200, meOneOrgBody)
	c, _, _ = newTestCLI(t, a)
	t.Setenv("OBSERVATORY_HUB", b.URL)
	t.Setenv("OBSERVATORY_TOKEN", "env-tok")
	if code := run(c, []string{"whoami"}); code != 0 || b.calls("GET /api/me")[0].Auth != "Bearer env-tok" {
		t.Fatalf("OBSERVATORY_TOKEN is not bound to the stored hub: exit %d", code)
	}
}

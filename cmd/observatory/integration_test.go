package main

import (
	"strings"
	"testing"
)

const (
	igGitHub    = `{"id":"i1","kind":"github","source":"pat","scope":"user","mode":"proxy","account":"ada","scopes":"repo","providerExpiresAt":"2026-12-30T14:03:00Z","status":"ok","lastError":"","wasInVm":false,"updatedAt":"2026-11-19T13:50:00Z","canEdit":true,"canDelete":true}`
	igClaude    = `{"id":"i2","kind":"claude","source":"oauth","scope":"team","teamId":"t1","teamName":"backend","mode":"vm","account":"ada@example.test","scopes":"","providerExpiresAt":null,"status":"ok","lastError":"","wasInVm":true,"updatedAt":"2026-11-19T11:00:00Z","canEdit":true,"canDelete":true}`
	igLinear    = `{"id":"i3","kind":"linear","source":"api-key","scope":"org","mode":"proxy","account":"","scopes":"","providerExpiresAt":null,"status":"expired","lastError":"token rejected","wasInVm":false,"updatedAt":"2026-11-10T09:00:00Z","canEdit":true,"canDelete":true}`
	igBody      = `{"integrations":[` + igGitHub + `,` + igClaude + `,` + igLinear + `],"writableScopes":[]}`
	igRoute     = "/api/orgs/o1/integrations"
	dummyGitHub = "ghp_dummy0000000000000000"
)

func igHub(t *testing.T) (*fakeHub, *cli, func() string, func() string) {
	h := newFakeHub(t)
	meOneOrg(h)
	h.on("GET "+igRoute, 200, igBody)
	h.on("GET /api/orgs/o1/teams", 200, teamsBody)
	c, out, errb := newTestCLI(t, h)
	return h, c, out.String, errb.String
}

func TestIntegrationListGolden(t *testing.T) {
	_, c, out, errb := igHub(t)
	if code := run(c, []string{"integration", "list"}); code != 0 {
		t.Fatal(code, errb())
	}
	golden(t, "integration-list.golden", out())
}

func TestIntegrationConnectReadsTokenFromStdin(t *testing.T) {
	h, c, out, errb := igHub(t)
	h.on("POST "+igRoute, 201, `{"id":"i9","account":"ada"}`)
	if code := run(c, []string{"integration", "connect", "github", "--token", "x"}); code != 2 {
		t.Fatal(code, errb())
	}
	c.stdin = strings.NewReader(dummyGitHub + "\n")
	if code := run(c, []string{"integration", "connect", "github"}); code != 0 {
		t.Fatal(code, errb())
	}
	b := body(t, h.calls("POST " + igRoute)[0])
	if b["token"] != dummyGitHub || b["kind"] != "github" || b["source"] != "pat" || b["mode"] != "proxy" || b["scope"] != "user" {
		t.Errorf("body %v", b)
	}
	if out() != "Connected github as ada (private, proxy).\n" {
		t.Errorf("stdout %q", out())
	}
	h.on("POST "+igRoute, 400, `{"error":"verification failed: provider rejected the token"}`)
	c.stdin = strings.NewReader(dummyGitHub + "\n")
	if code := run(c, []string{"integration", "connect", "github", "--json"}); code != 1 {
		t.Fatal(code, errb())
	}
	if strings.Contains(out(), dummyGitHub) || strings.Contains(errb(), dummyGitHub) {
		t.Errorf("token echoed: stdout %q stderr %q", out(), errb())
	}
}

func TestClaudeSignInFlow(t *testing.T) {
	h, c, out, errb := igHub(t)
	h.on("POST "+igRoute+"/claude/sign-in", 200, `{"url":"https://example.test/authorize?state=st1","state":"st1"}`)
	h.on("POST "+igRoute+"/claude/sign-in/finish", 201, `{"id":"i9","account":"ada@example.test"}`)
	c.stdin = strings.NewReader("code123#st1\n")
	if code := run(c, []string{"integration", "sign-in", "claude"}); code != 0 {
		t.Fatal(code, errb())
	}
	if got, want := h.calls("POST " + igRoute + "/claude/sign-in")[0].Body, `{"scope":"user","mode":"proxy"}`; got != want {
		t.Errorf("start body %s", got)
	}
	if got, want := h.calls("POST " + igRoute + "/claude/sign-in/finish")[0].Body, `{"state":"st1","code":"code123#st1"}`; got != want {
		t.Errorf("finish body %s", got)
	}
	if !strings.Contains(errb(), "Open this URL, sign in, then paste the code shown:\nhttps://example.test/authorize?state=st1\n") {
		t.Errorf("stderr %q", errb())
	}
	if out() != "Connected claude as ada@example.test (private, proxy).\n" {
		t.Errorf("stdout %q", out())
	}
	if strings.Contains(out()+errb(), "code123") {
		t.Error("code echoed")
	}
	c.stdin = strings.NewReader("code123#st1\n")
	if code := run(c, []string{"integration", "sign-in", "claude", "--team", "backend", "--replace"}); code != 0 {
		t.Fatal(code, errb())
	}
	if b := body(t, h.calls("POST " + igRoute + "/claude/sign-in")[1]); b["replaceId"] != "i2" {
		t.Errorf("replace body %v", b)
	}
}

func TestIntegrationReplace(t *testing.T) {
	h, c, out, errb := igHub(t)
	h.on("PUT "+igRoute+"/i1/token", 200, `{"account":"ada"}`)
	c.stdin = strings.NewReader(dummyGitHub)
	if code := run(c, []string{"integration", "replace", "github"}); code != 0 {
		t.Fatal(code, errb())
	}
	if got, want := h.calls("PUT " + igRoute + "/i1/token")[0].Body, `{"token":"`+dummyGitHub+`","source":"pat"}`; got != want {
		t.Errorf("body %s", got)
	}
	if strings.Contains(out()+errb(), dummyGitHub) {
		t.Error("token echoed")
	}
}

func TestIntegrationModeAndDisconnect(t *testing.T) {
	h, c, out, errb := igHub(t)
	h.on("PATCH "+igRoute+"/i2", 204, "")
	h.on("DELETE "+igRoute+"/i2", 204, "")
	if code := run(c, []string{"integration", "mode", "claude", "proxy", "--team", "backend"}); code != 0 {
		t.Fatal(code, errb())
	}
	if got := h.calls("PATCH " + igRoute + "/i2")[0].Body; got != `{"mode":"proxy"}` {
		t.Errorf("body %s", got)
	}
	if !strings.Contains(errb(), rotateHint) || out() != "Set claude (team backend) to proxy.\n" {
		t.Errorf("stdout %q stderr %q", out(), errb())
	}
	if code := run(c, []string{"integration", "disconnect", "claude", "--team", "backend"}); code != 2 {
		t.Fatal(code, errb())
	}
	if len(h.calls("DELETE "+igRoute+"/i2")) != 0 {
		t.Fatal("DELETE without confirmation")
	}
	if code := run(c, []string{"integration", "disconnect", "claude", "--team", "backend", "--yes"}); code != 0 {
		t.Fatal(code, errb())
	}
	if len(h.calls("DELETE "+igRoute+"/i2")) != 1 || strings.Count(errb(), rotateHint) != 2 {
		t.Errorf("no DELETE or hint: %q", errb())
	}
	if code := run(c, []string{"integration", "disconnect", "github", "--scope", "org", "--yes"}); code != 5 {
		t.Fatal(code, errb())
	}
}

func TestIntegrationUnknownKind(t *testing.T) {
	h := newFakeHub(t)
	c, _, errb := newTestCLI(t, h)
	c.stdin = strings.NewReader(dummyGitHub)
	if code := run(c, []string{"integration", "connect", "slack"}); code != 2 {
		t.Fatal(code, errb.String())
	}
	if code := run(c, []string{"integration", "sign-in", "github"}); code != 2 {
		t.Fatal(code, errb.String())
	}
	if len(h.reqs) != 0 {
		t.Fatalf("requests sent: %v", h.reqs)
	}
}

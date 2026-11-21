package main

import (
	"encoding/json"
	"strings"
	"testing"
)

const (
	varFoo    = `{"id":"e1","name":"FOO","kind":"env","scope":"org","value":"bar-plain","hosts":[],"updatedAt":"2026-11-19T13:50:00Z","updatedBy":{"id":"u1","name":"Ada Lovelace"},"canEdit":true,"canDelete":true}`
	varToken  = `{"id":"e2","name":"TOKEN","kind":"secret","scope":"user","hosts":[],"updatedAt":"2026-11-19T11:00:00Z","updatedBy":{"id":"u1","name":"Ada Lovelace"},"canEdit":true,"canDelete":true}`
	varGH     = `{"id":"e3","name":"GH_TOKEN","kind":"secret","scope":"team","teamId":"t1","teamName":"backend","hosts":["api.github.com","github.com"],"updatedAt":"2026-11-10T09:00:00Z","updatedBy":{"id":"u2","name":"Grace Hopper"},"canEdit":true,"canDelete":true}`
	varsBody  = `{"vars":[` + varFoo + `,` + varToken + `,` + varGH + `],"writableScopes":[{"scope":"org"},{"scope":"user"}]}`
	envRoute  = "/api/orgs/o1/env"
	teamsBody = `{"teams":[{"id":"t1","name":"backend"}]}`
)

func varHub(t *testing.T, vars string) (*fakeHub, *cli, func() string, func() string) {
	h := newFakeHub(t)
	meOneOrg(h)
	h.on("GET "+envRoute, 200, vars)
	h.on(iamRPC+"ListTeams", 200, teamsBody)
	c, out, errb := newTestCLI(t, h)
	return h, c, out.String, errb.String
}

func body(t *testing.T, r recorded) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal([]byte(r.Body), &m); err != nil {
		t.Fatal(err)
	}
	return m
}

func TestVarListGolden(t *testing.T) {
	_, c, out, errb := varHub(t, varsBody)
	if code := run(c, []string{"var", "list"}); code != 0 {
		t.Fatal(code, errb())
	}
	golden(t, "var-list.golden", out())
	if strings.Contains(out(), "e2") {
		t.Error("IDs in list")
	}
}

func TestVarListEscapesControlCharacters(t *testing.T) {
	evil := strings.Replace(varFoo, `"value":"bar-plain"`, `"value":"\u001b]0;pwned\u0007\u001b[31mred"`, 1)
	_, c, out, errb := varHub(t, `{"vars":[`+evil+`]}`)
	if code := run(c, []string{"var", "list"}); code != 0 {
		t.Fatal(code, errb())
	}
	if strings.ContainsAny(out(), "\x1b\x07") || !strings.Contains(out(), "?]0;pwned??[31mred") {
		t.Errorf("table %q", out())
	}
	if code := run(c, []string{"var", "list", "--json"}); code != 0 || !strings.Contains(out(), `\u001b[31mred`) {
		t.Errorf("--json must stay unchanged: %q", out())
	}
}

func TestVarListFilter(t *testing.T) {
	_, c, out, errb := varHub(t, varsBody)
	if code := run(c, []string{"var", "list", "--team", "backend"}); code != 0 {
		t.Fatal(code, errb())
	}
	if !strings.Contains(out(), "GH_TOKEN") || strings.Contains(out(), "FOO") || strings.Contains(out(), "\nTOKEN ") {
		t.Errorf("filtered list %q", out())
	}
}

func TestVarSetSecretOnlyFromStdin(t *testing.T) {
	h, c, _, errb := varHub(t, `{"vars":[],"writableScopes":[]}`)
	h.on("POST "+envRoute, 201, `{"id":"e9"}`)
	if code := run(c, []string{"var", "set", "TOKEN", "--secret", "--value", "x"}); code != 2 {
		t.Fatal(code, errb())
	}
	if len(h.reqs) != 0 {
		t.Fatalf("requests sent: %v", h.reqs)
	}
	c.stdin = strings.NewReader("s3cret-dummy\n")
	if code := run(c, []string{"var", "set", "TOKEN", "--secret"}); code != 0 {
		t.Fatal(code, errb())
	}
	b := body(t, h.calls("POST " + envRoute)[0])
	if b["value"] != "s3cret-dummy" || b["kind"] != "secret" || b["scope"] != "user" || b["name"] != "TOKEN" {
		t.Errorf("body %v", b)
	}
}

func TestVarSetUpsert(t *testing.T) {
	h, c, out, errb := varHub(t, varsBody)
	h.on("PATCH "+envRoute+"/e1", 204, "")
	if code := run(c, []string{"var", "set", "FOO", "--scope", "org", "--value", "baz"}); code != 0 {
		t.Fatal(code, errb())
	}
	if got, want := h.calls("PATCH " + envRoute + "/e1")[0].Body, `{"value":"baz","kind":"env"}`; got != want {
		t.Errorf("body %s", got)
	}
	if out() != "Set FOO (org).\n" {
		t.Errorf("stdout %q", out())
	}
	if code := run(c, []string{"var", "set", "FOO", "--scope", "org", "--value", "baz", "--host", "a.example", "--host", "b.example"}); code != 0 {
		t.Fatal(code, errb())
	}
	if got, want := h.calls("PATCH " + envRoute + "/e1")[1].Body, `{"value":"baz","kind":"env","hosts":["a.example","b.example"]}`; got != want {
		t.Errorf("body %s", got)
	}
}

func TestVarSetRefusesPlainOverSecret(t *testing.T) {
	h, c, _, errb := varHub(t, varsBody)
	if code := run(c, []string{"var", "set", "TOKEN", "--value", "x"}); code != 2 {
		t.Fatal(code, errb())
	}
	if len(h.calls("PATCH "+envRoute+"/e2")) != 0 {
		t.Error("PATCH sent")
	}
}

func TestSecretNeverEchoed(t *testing.T) {
	h, c, out, errb := varHub(t, `{"vars":[],"writableScopes":[]}`)
	h.on("POST "+envRoute, 201, `{"id":"e9"}`)
	c.stdin = strings.NewReader("s3cret-dummy\n")
	if code := run(c, []string{"var", "set", "TOKEN", "--secret"}); code != 0 {
		t.Fatal(code, errb())
	}
	h.on("POST "+envRoute, 400, `{"error":"invalid name"}`)
	c.stdin = strings.NewReader("s3cret-dummy\n")
	if code := run(c, []string{"var", "set", "bad-name", "--secret"}); code != 1 {
		t.Fatal(code, errb())
	}
	c.stdin = strings.NewReader("s3cret-dummy\n")
	if code := run(c, []string{"var", "set", "TOKEN2", "--secret", "--json"}); code != 1 {
		t.Fatal(code, errb())
	}
	if strings.Contains(out(), "s3cret-dummy") || strings.Contains(errb(), "s3cret-dummy") {
		t.Errorf("secret echoed: stdout %q stderr %q", out(), errb())
	}
}

func TestVarMoveBody(t *testing.T) {
	h, c, out, errb := varHub(t, varsBody)
	h.on("POST "+envRoute+"/e1/move", 204, "")
	if code := run(c, []string{"var", "move", "FOO", "--scope", "org", "--to-team", "backend"}); code != 0 {
		t.Fatal(code, errb())
	}
	if got, want := h.calls("POST " + envRoute + "/e1/move")[0].Body, `{"scope":"team","teamId":"t1"}`; got != want {
		t.Errorf("body %s", got)
	}
	if code := run(c, []string{"var", "move", "FOO", "--scope", "org", "--to", "private"}); code != 0 {
		t.Fatal(code, errb())
	}
	if got, want := h.calls("POST " + envRoute + "/e1/move")[1].Body, `{"scope":"user"}`; got != want {
		t.Errorf("body %s", got)
	}
	if !strings.Contains(out(), "Moved FOO to team backend.") {
		t.Errorf("stdout %q", out())
	}
	if code := run(c, []string{"var", "move", "FOO", "--scope", "org"}); code != 2 {
		t.Fatal(code, errb())
	}
}

func TestVarDeleteConfirms(t *testing.T) {
	h, c, _, errb := varHub(t, varsBody)
	h.on("DELETE "+envRoute+"/e2", 204, "")
	if code := run(c, []string{"var", "delete", "TOKEN"}); code != 2 {
		t.Fatal(code, errb())
	}
	if len(h.calls("DELETE "+envRoute+"/e2")) != 0 {
		t.Fatal("DELETE without confirmation")
	}
	if code := run(c, []string{"var", "delete", "TOKEN", "--yes"}); code != 0 {
		t.Fatal(code, errb())
	}
	if len(h.calls("DELETE "+envRoute+"/e2")) != 1 {
		t.Error("no DELETE")
	}
	if code := run(c, []string{"var", "delete", "TOKEN", "--scope", "org", "--yes"}); code != 5 {
		t.Fatal(code, errb())
	}
}

func TestVarTeamScope(t *testing.T) {
	h, c, out, errb := varHub(t, varsBody)
	h.on("POST "+envRoute, 201, `{"id":"e9"}`)
	if code := run(c, []string{"var", "set", "REGION", "--team", "backend", "--value", "eu"}); code != 0 {
		t.Fatal(code, errb())
	}
	b := body(t, h.calls("POST " + envRoute)[0])
	if b["scope"] != "team" || b["teamId"] != "t1" || b["kind"] != "env" || b["value"] != "eu" {
		t.Errorf("body %v", b)
	}
	if out() != "Set REGION (team backend).\n" {
		t.Errorf("stdout %q", out())
	}
	h.on("PATCH "+envRoute+"/e3", 204, "")
	c.stdin = strings.NewReader("s3cret-dummy")
	if code := run(c, []string{"var", "set", "GH_TOKEN", "--team", "backend", "--secret"}); code != 0 {
		t.Fatal(code, errb())
	}
	if len(h.calls("PATCH "+envRoute+"/e3")) != 1 {
		t.Error("team var not patched")
	}
}

package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const (
	memAda   = `{"id":"u1","email":"ada@example.com","name":"Ada Lovelace","role":"owner","joinedAt":"2026-10-01T09:00:00Z"}`
	memGrace = `{"id":"u2","email":"grace@example.com","name":"Grace Hopper","role":"member","joinedAt":"2026-11-02T10:30:00Z"}`
	teamBack = `{"id":"t1","name":"backend","memberCount":2,"createdAt":"2026-10-05T08:00:00Z"}`
	roleCust = `{"id":"r1","name":"viewer","description":"read only","rules":[{"effect":"allow","object":"org/vms","action":"vm:read"}],"parents":[]}`
	roleBase = `{"key":"member","name":"Member","builtin":true,"rules":[]}`
)

func orgHub(t *testing.T) (*fakeHub, *cli, func() string, func() string) {
	h := newFakeHub(t)
	meOneOrg(h)
	h.on("GET /api/orgs/o1/members", 200, `[`+memAda+`,`+memGrace+`]`)
	h.on("GET /api/orgs/o1/teams", 200, `[`+teamBack+`]`)
	h.on("GET /api/orgs/o1/roles", 200, `[`+roleBase+`,`+roleCust+`]`)
	c, out, errb := newTestCLI(t, h)
	return h, c, out.String, errb.String
}

func mustRun(t *testing.T, c *cli, errb func() string, want int, args ...string) {
	t.Helper()
	if code := run(c, args); code != want {
		t.Fatalf("%v: exit %d, stderr %s", args, code, errb())
	}
}

func TestTeamListGolden(t *testing.T) {
	_, c, out, errb := orgHub(t)
	mustRun(t, c, errb, 0, "team", "list")
	golden(t, "team-list.golden", out())
}

func TestMemberListGolden(t *testing.T) {
	_, c, out, errb := orgHub(t)
	mustRun(t, c, errb, 0, "member", "list")
	golden(t, "member-list.golden", out())
}

func TestTeamAddMemberByEmail(t *testing.T) {
	h, c, _, errb := orgHub(t)
	h.on("PUT /api/orgs/o1/teams/t1/members/u1", 204, "")
	mustRun(t, c, errb, 0, "team", "add", "backend", "ADA@example.com", "--admin")
	if got := h.calls("PUT /api/orgs/o1/teams/t1/members/u1")[0].Body; got != `{"admin":true}` {
		t.Errorf("body %s", got)
	}
}

func TestTeamRemoveConfirms(t *testing.T) {
	h, c, _, errb := orgHub(t)
	h.on("DELETE /api/orgs/o1/teams/t1/members/u2", 204, "")
	mustRun(t, c, errb, 2, "team", "remove", "backend", "Grace Hopper")
	mustRun(t, c, errb, 0, "team", "remove", "backend", "grace@example.com", "-y")
}

func TestMemberRoleBody(t *testing.T) {
	h, c, _, errb := orgHub(t)
	h.on("PATCH /api/orgs/o1/members/u2", 204, "")
	mustRun(t, c, errb, 2, "member", "role", "grace@example.com", "boss")
	mustRun(t, c, errb, 0, "member", "role", "grace@example.com", "admin")
	if got := h.calls("PATCH /api/orgs/o1/members/u2")[0].Body; got != `{"role":"admin"}` {
		t.Errorf("body %s", got)
	}
}

func TestMemberRemoveConfirms(t *testing.T) {
	h, c, _, errb := orgHub(t)
	h.on("DELETE /api/orgs/o1/members/u2", 204, "")
	mustRun(t, c, errb, 2, "member", "remove", "u2")
	if len(h.calls("DELETE /api/orgs/o1/members/u2")) != 0 {
		t.Fatal("DELETE without confirmation")
	}
	mustRun(t, c, errb, 0, "member", "remove", "u2", "--yes")
}

func TestInviteCreateAndAccept(t *testing.T) {
	h, c, out, errb := orgHub(t)
	h.on("POST /api/orgs/o1/invites", 201, `{"id":"i1","token":"tok123","url":"https://hub/invite/tok123","expiresAt":"2026-11-26T00:00:00Z"}`)
	mustRun(t, c, errb, 0, "invite", "create", "--role", "admin")
	if out() != "https://hub/invite/tok123\n" || h.calls("POST /api/orgs/o1/invites")[0].Body != `{"role":"admin"}` {
		t.Errorf("stdout %q body %s", out(), h.calls("POST /api/orgs/o1/invites")[0].Body)
	}
}

func TestInviteAcceptFromURL(t *testing.T) {
	h, c, out, errb := orgHub(t)
	h.on("POST /api/invites/accept", 200, `{"orgId":"o1","role":"member"}`)
	c.stdin = strings.NewReader("https://hub/invite/tok123\n")
	mustRun(t, c, errb, 0, "invite", "accept")
	if got := h.calls("POST /api/invites/accept")[0].Body; got != `{"token":"tok123"}` {
		t.Errorf("body %s", got)
	}
	if out() != "Joined orga.\n" {
		t.Errorf("stdout %q", out())
	}
}

func TestInviteAcceptRejectsArgv(t *testing.T) {
	h, c, _, errb := orgHub(t)
	mustRun(t, c, errb, 2, "invite", "accept", "tok123")
	if len(h.calls("POST /api/invites/accept")) != 0 {
		t.Fatal("request sent")
	}
}

func TestInviteAcceptFromStdin(t *testing.T) {
	h, c, _, errb := orgHub(t)
	h.on("POST /api/invites/accept", 200, `{"orgId":"o1","role":"member"}`)
	c.stdin = strings.NewReader("tok456\n")
	mustRun(t, c, errb, 0, "invite", "accept")
	if got := h.calls("POST /api/invites/accept")[0].Body; got != `{"token":"tok456"}` {
		t.Errorf("body %s", got)
	}
}

func TestRoleCreateFromFile(t *testing.T) {
	h, c, _, errb := orgHub(t)
	h.on("POST /api/orgs/o1/roles", 201, roleCust)
	dir := t.TempDir()
	good, bad := filepath.Join(dir, "good.json"), filepath.Join(dir, "bad.json")
	const body = `{"name":"viewer","description":"","rules":[{"effect":"allow","object":"org/vms","action":"vm:read"}],"parents":[]}`
	os.WriteFile(good, []byte(body+"\n"), 0o600)
	os.WriteFile(bad, []byte(`{"name":`), 0o600)
	mustRun(t, c, errb, 2, "role", "create", "--file", bad)
	if len(h.calls("POST /api/orgs/o1/roles")) != 0 {
		t.Fatal("request sent for invalid JSON")
	}
	mustRun(t, c, errb, 0, "role", "create", "--file", good)
	if got := h.calls("POST /api/orgs/o1/roles")[0].Body; got != body {
		t.Errorf("body %s", got)
	}
}

func TestRoleUpdateAndDelete(t *testing.T) {
	h, c, _, errb := orgHub(t)
	h.on("PUT /api/orgs/o1/roles/r1", 200, roleCust)
	h.on("DELETE /api/orgs/o1/roles/r1", 204, "")
	f := filepath.Join(t.TempDir(), "r.json")
	os.WriteFile(f, []byte(`{"name":"viewer"}`), 0o600)
	mustRun(t, c, errb, 0, "role", "update", "viewer", "--file", f)
	mustRun(t, c, errb, 2, "role", "delete", "viewer")
	mustRun(t, c, errb, 2, "role", "delete", "member", "--yes")
	mustRun(t, c, errb, 0, "role", "delete", "viewer", "--yes")
}

func TestBindingSubjectResolution(t *testing.T) {
	h, c, _, errb := orgHub(t)
	h.on("POST /api/orgs/o1/bindings", 201, `{"id":"b1","subject":"user:u1","roleId":"r1","createdAt":"2026-11-19T14:00:00Z"}`)
	mustRun(t, c, errb, 0, "binding", "add", "user:ada@example.com", "viewer")
	mustRun(t, c, errb, 0, "binding", "add", "team:backend", "viewer")
	calls := h.calls("POST /api/orgs/o1/bindings")
	if calls[0].Body != `{"roleId":"r1","subject":"user:u1"}` || calls[1].Body != `{"roleId":"r1","subject":"team:t1"}` {
		t.Errorf("bodies %s %s", calls[0].Body, calls[1].Body)
	}
}

const limitsBody = `{"org":{"vcpu":8,"ramMb":16384,"diskGb":100,"vmCount":10,"snapshotCount":null},"teams":{"t1":{"vcpu":4,"ramMb":null,"diskGb":null,"vmCount":null,"snapshotCount":null}},"members":{}}`

func TestLimitSetKeepsOtherValues(t *testing.T) {
	h, c, _, errb := orgHub(t)
	h.on("GET /api/orgs/o1/limits", 200, limitsBody)
	h.on("PUT /api/orgs/o1/limits/team:t1", 204, "")
	mustRun(t, c, errb, 2, "limit", "set", "org", "--vcpu", "16")
	mustRun(t, c, errb, 0, "limit", "set", "team:backend", "--vms", "3")
	if got := h.calls("PUT /api/orgs/o1/limits/team:t1")[0].Body; got != `{"vcpu":4,"ramMb":null,"diskGb":null,"vmCount":3,"snapshotCount":null,"imageStorageGb":null}` {
		t.Errorf("body %s", got)
	}
	if len(h.calls("PUT /api/admin/orgs/o1/limits")) != 0 {
		t.Error("admin route called")
	}
	mustRun(t, c, errb, 2, "limit", "set", "team:backend", "--vcpu", "-1")
}

func TestLimitShow(t *testing.T) {
	h, c, out, errb := orgHub(t)
	h.on("GET /api/orgs/o1/limits", 200, limitsBody)
	mustRun(t, c, errb, 0, "limit", "show")
	if !strings.Contains(out(), "team:t1 (backend)") || !strings.Contains(out(), "16 GB") {
		t.Errorf("stdout %s", out())
	}
}

func TestSessionRevokeCurrentClearsToken(t *testing.T) {
	h, c, _, errb := orgHub(t)
	h.on("GET /api/me/sessions", 200, `[{"id":"s1","kind":"cli","userAgent":"observatory","ip":"10.0.0.1","lastUsedAt":"2026-11-19T14:00:00Z","expiresAt":"2026-12-19T14:00:00Z","current":true},{"id":"s2","kind":"web","userAgent":"Firefox","ip":"10.0.0.2","lastUsedAt":"2026-11-18T14:00:00Z","expiresAt":"2026-12-18T14:00:00Z","current":false}]`)
	h.on("DELETE /api/me/sessions/s1", 204, "")
	h.on("DELETE /api/me/sessions/s2", 204, "")
	mustRun(t, c, errb, 2, "session", "revoke", "s1")
	mustRun(t, c, errb, 0, "session", "revoke", "s2", "-y")
	if c.cfg.Token != "tok-test" {
		t.Fatal("token cleared by revoking another session")
	}
	mustRun(t, c, errb, 5, "session", "revoke", "nope", "-y")
	mustRun(t, c, errb, 0, "session", "revoke", "s1", "-y")
	cfg, err := loadConfig(c.cfgDir)
	if err != nil || cfg.Token != "" || c.cfg.Token != "" {
		t.Errorf("token not cleared: %+v %v", cfg, err)
	}
}

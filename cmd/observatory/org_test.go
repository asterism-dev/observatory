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
	roleCust = `{"id":"r1","name":"viewer","description":"read only","rules":[{"effect":"allow","object":"org/vms","action":"compute.instances.get"}],"parents":[]}`
	roleBase = `{"key":"member","name":"Member","builtin":true,"rules":[]}`
)

func orgHub(t *testing.T) (*fakeHub, *cli, func() string, func() string) {
	h := newFakeHub(t)
	meOneOrg(h)
	h.on(iamRPC+"ListMembers", 200, `{"members":[`+memAda+`,`+memGrace+`]}`)
	h.on(iamRPC+"ListTeams", 200, `{"teams":[`+teamBack+`]}`)
	h.on(iamRPC+"ListRoles", 200, `{"roles":[`+roleBase+`,`+roleCust+`]}`)
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
	h.on(iamRPC+"SetTeamMember", 200, `{}`)
	mustRun(t, c, errb, 0, "team", "add", "backend", "ADA@example.com", "--admin")
	mustRun(t, c, errb, 0, "team", "add", "backend", "grace@example.com")
	calls := h.calls(iamRPC + "SetTeamMember")
	// admin is sent even when false, since the server refuses a missing one.
	if a, b := compactJSON(t, calls[0].Body), compactJSON(t, calls[1].Body); a != `{"org":"o1","team":"t1","user":"u1","admin":true}` || b != `{"org":"o1","team":"t1","user":"u2","admin":false}` {
		t.Errorf("bodies %s %s", a, b)
	}
}

func TestTeamRemoveConfirms(t *testing.T) {
	h, c, _, errb := orgHub(t)
	h.on(iamRPC+"RemoveTeamMember", 200, `{}`)
	h.on(iamRPC+"LeaveTeam", 200, `{}`)
	mustRun(t, c, errb, 2, "team", "remove", "backend", "Grace Hopper")
	mustRun(t, c, errb, 0, "team", "remove", "backend", "grace@example.com", "-y")
	if got := compactJSON(t, h.calls(iamRPC + "RemoveTeamMember")[0].Body); got != `{"org":"o1","team":"t1","user":"u2"}` {
		t.Errorf("body %s", got)
	}
	mustRun(t, c, errb, 0, "team", "remove", "backend", "ada@example.com", "-y")
	if got := h.calls(iamRPC + "LeaveTeam"); len(got) != 1 || compactJSON(t, got[0].Body) != `{"org":"o1","team":"t1"}` {
		t.Errorf("removing yourself: %+v", got)
	}
}

func TestMemberRoleBody(t *testing.T) {
	h, c, _, errb := orgHub(t)
	h.on(iamRPC+"UpdateMember", 200, `{}`)
	mustRun(t, c, errb, 2, "member", "role", "grace@example.com", "boss")
	mustRun(t, c, errb, 0, "member", "role", "grace@example.com", "admin")
	if got := compactJSON(t, h.calls(iamRPC + "UpdateMember")[0].Body); got != `{"org":"o1","user":"u2","role":"admin"}` {
		t.Errorf("body %s", got)
	}
}

func TestMemberRemoveConfirms(t *testing.T) {
	h, c, _, errb := orgHub(t)
	h.on(iamRPC+"DeleteMember", 200, `{}`)
	h.on(iamRPC+"LeaveOrg", 200, `{}`)
	mustRun(t, c, errb, 2, "member", "remove", "u2")
	if len(h.calls(iamRPC+"DeleteMember")) != 0 {
		t.Fatal("DeleteMember without confirmation")
	}
	mustRun(t, c, errb, 0, "member", "remove", "u2", "--yes")
	if got := compactJSON(t, h.calls(iamRPC + "DeleteMember")[0].Body); got != `{"org":"o1","user":"u2"}` {
		t.Errorf("body %s", got)
	}
	mustRun(t, c, errb, 0, "member", "remove", "u1", "--yes")
	if got := h.calls(iamRPC + "LeaveOrg"); len(got) != 1 || compactJSON(t, got[0].Body) != `{"orgId":"o1"}` {
		t.Errorf("removing yourself: %+v", got)
	}
}

func TestInviteCreate(t *testing.T) {
	h, c, out, errb := orgHub(t)
	h.on(iamRPC+"CreateInvite", 200, `{"invite":{"id":"i1","role":"admin","expiresAt":"2026-11-26T00:00:00Z"},"token":"tok123","url":"https://hub/invite/tok123"}`)
	mustRun(t, c, errb, 0, "invite", "create", "--role", "admin")
	if body := compactJSON(t, h.calls(iamRPC + "CreateInvite")[0].Body); out() != "https://hub/invite/tok123\n" || body != `{"org":"o1","role":"admin"}` {
		t.Errorf("stdout %q body %s", out(), body)
	}
}

func TestInviteAcceptRejectsArgv(t *testing.T) {
	h, c, _, errb := orgHub(t)
	mustRun(t, c, errb, 2, "invite", "accept", "tok123")
	if len(h.calls(iamRPC+"AcceptInvite")) != 0 {
		t.Fatal("request sent")
	}
}

func TestInviteAcceptFromStdin(t *testing.T) {
	h, c, _, errb := orgHub(t)
	h.on(iamRPC+"AcceptInvite", 200, `{"orgId":"o1","role":"member"}`)
	c.stdin = strings.NewReader("tok456\n")
	mustRun(t, c, errb, 0, "invite", "accept")
	if got := compactJSON(t, h.calls(iamRPC + "AcceptInvite")[0].Body); got != `{"token":"tok456"}` {
		t.Errorf("body %s", got)
	}
}

func TestRoleCreateFromFile(t *testing.T) {
	h, c, _, errb := orgHub(t)
	h.on(iamRPC+"CreateRole", 200, `{"role":`+roleCust+`}`)
	dir := t.TempDir()
	good, bad := filepath.Join(dir, "good.json"), filepath.Join(dir, "bad.json")
	// The output of role show --json, with fields the request does not have.
	const file = `{"id":"r9","name":"viewer","description":"","builtin":false,"rules":[{"effect":"allow","object":"org/vms","action":"compute.instances.get"}],"parents":[]}`
	os.WriteFile(good, []byte(file+"\n"), 0o600)
	os.WriteFile(bad, []byte(`{"name":`), 0o600)
	mustRun(t, c, errb, 2, "role", "create", "--file", bad)
	if len(h.calls(iamRPC+"CreateRole")) != 0 {
		t.Fatal("request sent for invalid JSON")
	}
	mustRun(t, c, errb, 0, "role", "create", "--file", good)
	if got := compactJSON(t, h.calls(iamRPC + "CreateRole")[0].Body); got != `{"org":"o1","name":"viewer","rules":[{"effect":"allow","object":"org/vms","action":"compute.instances.get"}]}` {
		t.Errorf("body %s", got)
	}
}

func TestRoleUpdateAndDelete(t *testing.T) {
	h, c, _, errb := orgHub(t)
	h.on(iamRPC+"UpdateRole", 200, `{"role":`+roleCust+`}`)
	h.on(iamRPC+"DeleteRole", 200, `{}`)
	f := filepath.Join(t.TempDir(), "r.json")
	os.WriteFile(f, []byte(`{"name":"viewer"}`), 0o600)
	mustRun(t, c, errb, 0, "role", "update", "viewer", "--file", f)
	if got := compactJSON(t, h.calls(iamRPC + "UpdateRole")[0].Body); got != `{"org":"o1","role":"r1","name":"viewer"}` {
		t.Errorf("body %s", got)
	}
	mustRun(t, c, errb, 2, "role", "delete", "viewer")
	mustRun(t, c, errb, 2, "role", "delete", "member", "--yes")
	mustRun(t, c, errb, 0, "role", "delete", "viewer", "--yes")
	if got := compactJSON(t, h.calls(iamRPC + "DeleteRole")[0].Body); got != `{"org":"o1","role":"r1"}` {
		t.Errorf("body %s", got)
	}
}

func TestBindingSubjectResolution(t *testing.T) {
	h, c, _, errb := orgHub(t)
	h.on(iamRPC+"CreateRoleBinding", 200, `{"binding":{"id":"b1","subject":"user:u1","roleId":"r1","createdAt":"2026-11-19T14:00:00Z"}}`)
	mustRun(t, c, errb, 0, "binding", "add", "user:ada@example.com", "viewer")
	mustRun(t, c, errb, 0, "binding", "add", "team:backend", "viewer")
	calls := h.calls(iamRPC + "CreateRoleBinding")
	if a, b := compactJSON(t, calls[0].Body), compactJSON(t, calls[1].Body); a != `{"org":"o1","subject":"user:u1","roleId":"r1"}` || b != `{"org":"o1","subject":"team:t1","roleId":"r1"}` {
		t.Errorf("bodies %s %s", a, b)
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
	h.on(iamRPC+"ListSessions", 200, `{"sessions":[{"id":"s1","kind":"cli","userAgent":"observatory","ip":"10.0.0.1","lastUsedAt":"2026-11-19T14:00:00Z","expiresAt":"2026-12-19T14:00:00Z","current":true},{"id":"s2","kind":"web","userAgent":"Firefox","ip":"10.0.0.2","lastUsedAt":"2026-11-18T14:00:00Z","expiresAt":"2026-12-18T14:00:00Z"}]}`)
	h.on(iamRPC+"RevokeSession", 200, `{}`)
	mustRun(t, c, errb, 2, "session", "revoke", "s1")
	mustRun(t, c, errb, 0, "session", "revoke", "s2", "-y")
	if c.cfg.Token != "tok-test" || compactJSON(t, h.calls(iamRPC + "RevokeSession")[0].Body) != `{"session":"s2"}` {
		t.Fatal("token cleared by revoking another session")
	}
	mustRun(t, c, errb, 5, "session", "revoke", "nope", "-y")
	mustRun(t, c, errb, 0, "session", "revoke", "s1", "-y")
	cfg, err := loadConfig(c.cfgDir)
	if err != nil || cfg.Token != "" || c.cfg.Token != "" {
		t.Errorf("token not cleared: %+v %v", cfg, err)
	}
}

func TestMemberListRPC(t *testing.T) {
	h, c, out, errb := orgHub(t)
	mustRun(t, c, errb, 0, "member", "list", "--json")
	if got := compactJSON(t, h.calls(iamRPC + "ListMembers")[0].Body); got != `{"org":"o1"}` {
		t.Errorf("body %s", got)
	}
	if got := compactJSON(t, out()); got != `{"members":[`+memAda+`,`+memGrace+`]}` {
		t.Errorf("stdout %s", got)
	}
}

func TestInviteAcceptRPC(t *testing.T) {
	h, c, out, errb := orgHub(t)
	h.on(iamRPC+"AcceptInvite", 200, `{"orgId":"o1","role":"member"}`)
	c.stdin = strings.NewReader("https://hub/invite/tok123\n")
	mustRun(t, c, errb, 0, "invite", "accept")
	if got := compactJSON(t, h.calls(iamRPC + "AcceptInvite")[0].Body); got != `{"token":"tok123"}` {
		t.Errorf("body %s", got)
	}
	if out() != "Joined orga.\n" {
		t.Errorf("stdout %q", out())
	}
}

func TestRoleCatalogShowsUnknown(t *testing.T) {
	h, c, out, errb := orgHub(t)
	h.on(iamRPC+"ListCatalog", 200, `{"entries":[{"action":"compute.instances.get","object":"org/vms/*","write":false,"service":"hub"}],"unknownActions":["compute.instances.fork"]}`)
	mustRun(t, c, errb, 0, "role", "catalog")
	want := "ACTION                  OBJECT\ncompute.instances.get   org/vms/*\ncompute.instances.fork  (unknown)\n"
	if out() != want {
		t.Errorf("stdout:\n%s\nwant:\n%s", out(), want)
	}
}

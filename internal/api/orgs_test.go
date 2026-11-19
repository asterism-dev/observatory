package api

import (
	"context"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/asterism-dev/observatory/internal/store"
)

func (e *env) exec(sql string, args ...any) {
	e.t.Helper()
	if _, err := e.pool.Exec(context.Background(), sql, args...); err != nil {
		e.t.Fatal(err)
	}
}

func (e *env) reload() {
	e.t.Helper()
	if err := e.z.Reload(context.Background()); err != nil {
		e.t.Fatal(err)
	}
}

// createOrg creates an org via the API and returns its ID.
func (e *env) createOrg(token, name string) uuid.UUID {
	e.t.Helper()
	rec := e.send("POST", "/api/orgs", token, `{"name":"`+name+`"}`)
	if rec.Code != 201 {
		e.t.Fatalf("create org: %d %s", rec.Code, rec.Body.String())
	}
	var got struct{ ID uuid.UUID }
	decode(e.t, rec, &got)
	return got.ID
}

func (e *env) addMember(org uuid.UUID, u store.User, role store.Role) {
	e.t.Helper()
	e.exec(`INSERT INTO memberships (org_id, user_id, role) VALUES ($1, $2, $3)`, org, u.ID, role)
	e.reload()
}

func (e *env) personalOrg(u store.User) uuid.UUID {
	e.t.Helper()
	ms, err := e.store.Memberships(context.Background(), u.ID)
	if err != nil || len(ms) == 0 || !ms[0].Org.Personal {
		e.t.Fatalf("personal org: %+v %v", ms, err)
	}
	return ms[0].Org.ID
}

func TestCreateOrgMakesCallerOwner(t *testing.T) {
	e := newEnv(t)
	u, tok := e.user("alice")
	rec := e.send("POST", "/api/orgs", tok, `{"name":"  Acme  "}`)
	if rec.Code != 201 {
		t.Fatalf("create: %d %s", rec.Code, rec.Body.String())
	}
	var got struct {
		ID, Name, Role string
		Personal       bool
	}
	decode(t, rec, &got)
	if got.Name != "Acme" || got.Role != "owner" || got.Personal || got.ID == "" {
		t.Fatalf("org: %s", rec.Body.String())
	}
	es := e.audit(store.ActionOrgCreate)
	if len(es) != 1 || es[0].ActorID != u.ID.String() || es[0].OrgID == nil || es[0].OrgID.String() != got.ID || es[0].Resource != "org/"+got.ID {
		t.Fatalf("audit: %+v", es)
	}
	// The new membership is visible to the enforcer without a manual reload.
	if rec := e.do("GET", "/api/orgs/"+got.ID+"/audit", tok); rec.Code != 200 {
		t.Fatalf("owner audit read: %d %s", rec.Code, rec.Body.String())
	}

	for _, body := range []string{`{"name":"   "}`, `{"name":"` + strings.Repeat("x", 65) + `"}`, `{"name":"a\u0000b"}`, `{"name":"a\nb"}`, `{}`, `nope`} {
		if rec := e.send("POST", "/api/orgs", tok, body); rec.Code != 400 {
			t.Fatalf("%s: %d", body, rec.Code)
		}
	}
	if rec := e.send("POST", "/api/orgs", tok, `{"name":"`+strings.Repeat("ä", 64)+`"}`); rec.Code != 201 {
		t.Fatalf("64 runes: %d", rec.Code)
	}
}

func TestOrgPermissionsByRole(t *testing.T) {
	e := newEnv(t)
	_, ownerTok := e.user("alice")
	admin, adminTok := e.user("bob")
	member, memberTok := e.user("carol")
	org := e.createOrg(ownerTok, "Acme")
	e.addMember(org, admin, store.RoleAdmin)
	e.addMember(org, member, store.RoleMember)

	memberPerms := []string{"disk:create", "env:list", "env:use", "image:list", "image:use", "integration:list", "integration:use", "limit:read", "member:read", "network:create", "network:read", "network:use", "node:read", "node:use", "role:read", "team:leave", "team:read", "vm:create"}
	adminPerms := []string{"audit:read", "device:delete", "device:list", "disk:create", "disk:delete", "disk:read", "disk:update", "env:delete", "env:list", "env:set", "env:use", "image:build", "image:default", "image:edit", "image:list", "image:use", "integration:delete", "integration:list", "integration:set", "integration:use", "limit:read", "limit:set", "member:invite", "member:read", "member:remove",
		"network:create", "network:delete", "network:read", "network:use", "node:create", "node:delete", "node:read", "node:update", "node:use", "org:update",
		"role:bind", "role:read", "role:write", "team:create", "team:delete", "team:leave", "team:manage-members", "team:read", "team:update",
		"vm:create", "vm:delete", "vm:disks", "vm:network", "vm:power", "vm:read", "vm:restore", "vm:snapshot"}
	ownerPerms := append(slices.Clone(adminPerms), "member:set-role")
	slices.Sort(ownerPerms)

	for _, c := range []struct {
		tok, role string
		want      []string
	}{{ownerTok, "owner", ownerPerms}, {adminTok, "admin", adminPerms}, {memberTok, "member", memberPerms}} {
		rec := e.do("GET", "/api/orgs/"+org.String(), c.tok)
		if rec.Code != 200 {
			t.Fatalf("%s: %d %s", c.role, rec.Code, rec.Body.String())
		}
		var got struct {
			ID, Name, Role string
			Personal       bool
			Permissions    []string
		}
		decode(t, rec, &got)
		if got.ID != org.String() || got.Name != "Acme" || got.Role != c.role || got.Personal || !slices.Equal(got.Permissions, c.want) {
			t.Fatalf("%s: %s", c.role, rec.Body.String())
		}
	}

	// A team admin may manage only their own team, which doesn't cover org/teams/*.
	var team uuid.UUID
	if err := e.pool.QueryRow(context.Background(), `INSERT INTO teams (org_id, name) VALUES ($1, 't') RETURNING id`, org).Scan(&team); err != nil {
		t.Fatal(err)
	}
	e.exec(`INSERT INTO team_members (team_id, user_id, admin) VALUES ($1, $2, true)`, team, member.ID)
	e.reload()
	var got struct{ Permissions []string }
	decode(t, e.do("GET", "/api/orgs/"+org.String(), memberTok), &got)
	if !slices.Equal(got.Permissions, memberPerms) {
		t.Fatalf("team admin: %v", got.Permissions)
	}

	// Denying everything leaves an empty list, not null.
	var role uuid.UUID
	if err := e.pool.QueryRow(context.Background(), `INSERT INTO roles (org_id, name) VALUES ($1, 'deny-all') RETURNING id`, org).Scan(&role); err != nil {
		t.Fatal(err)
	}
	e.exec(`INSERT INTO role_rules (role_id, position, effect, object, action) VALUES ($1, 0, 'deny', 'org/*', '*')`, role)
	e.exec(`INSERT INTO role_bindings (org_id, subject, role_id) VALUES ($1, $2, $3)`, org, "user:"+member.ID.String(), role)
	e.reload()
	if rec := e.do("GET", "/api/orgs/"+org.String(), memberTok); !strings.Contains(rec.Body.String(), `"permissions":[]`) {
		t.Fatalf("deny all: %s", rec.Body.String())
	}
}

func TestOrgNotMember404(t *testing.T) {
	e := newEnv(t)
	platformAdmin, adminTok := e.user("alice")
	if !platformAdmin.PlatformAdmin {
		t.Fatal("first user should be platform admin")
	}
	_, bobTok := e.user("bob")
	org := e.createOrg(bobTok, "Bob Inc")
	for _, path := range []string{"/api/orgs/" + org.String(), "/api/orgs/" + org.String() + "/audit"} {
		if rec := e.do("GET", path, adminTok); rec.Code != 404 || !strings.Contains(rec.Body.String(), `"not found"`) {
			t.Fatalf("%s: %d %s", path, rec.Code, rec.Body.String())
		}
	}
	if rec := e.send("PATCH", "/api/orgs/"+org.String(), adminTok, `{"name":"x"}`); rec.Code != 404 {
		t.Fatalf("patch: %d", rec.Code)
	}
	for _, id := range []string{"nope", strings.ToUpper(org.String()), "{" + org.String() + "}", uuid.NewString()} {
		if rec := e.do("GET", "/api/orgs/"+id, bobTok); rec.Code != 404 {
			t.Fatalf("%s: %d", id, rec.Code)
		}
	}
}

func TestRenameOrg(t *testing.T) {
	e := newEnv(t)
	owner, ownerTok := e.user("alice")
	admin, adminTok := e.user("bob")
	member, memberTok := e.user("carol")
	org := e.createOrg(ownerTok, "Acme")
	e.addMember(org, admin, store.RoleAdmin)
	e.addMember(org, member, store.RoleMember)

	if rec := e.send("PATCH", "/api/orgs/"+org.String(), adminTok, `{"name":" Acme Inc "}`); rec.Code != 200 || !strings.Contains(rec.Body.String(), `"name":"Acme Inc"`) {
		t.Fatalf("admin rename: %d %s", rec.Code, rec.Body.String())
	}
	es := e.audit("org:update")
	if len(es) != 1 || es[0].ActorID != admin.ID.String() || *es[0].OrgID != org || es[0].Result != store.ResultAllowed || es[0].Details["name"] != "Acme Inc" {
		t.Fatalf("audit: %+v", es)
	}

	if rec := e.send("PATCH", "/api/orgs/"+org.String(), memberTok, `{"name":"Hacked"}`); rec.Code != 403 || !strings.Contains(rec.Body.String(), `"forbidden"`) {
		t.Fatalf("member rename: %d %s", rec.Code, rec.Body.String())
	}
	es = e.audit("org:update")
	if len(es) != 2 || es[0].Result != store.ResultDenied || es[0].ActorID != member.ID.String() || *es[0].OrgID != org || es[0].Resource != "org/settings" {
		t.Fatalf("denied audit: %+v", es)
	}
	var got struct{ Name string }
	decode(t, e.do("GET", "/api/orgs/"+org.String(), ownerTok), &got)
	if got.Name != "Acme Inc" {
		t.Fatalf("name after denied rename: %q", got.Name)
	}

	if rec := e.send("PATCH", "/api/orgs/"+org.String(), adminTok, `{"name":""}`); rec.Code != 400 {
		t.Fatalf("empty name: %d", rec.Code)
	}
	if rec := e.send("PATCH", "/api/orgs/"+e.personalOrg(owner).String(), ownerTok, `{"name":"x"}`); rec.Code != 409 {
		t.Fatalf("personal rename: %d %s", rec.Code, rec.Body.String())
	}
}

func TestOrgAuditScoped(t *testing.T) {
	e := newEnv(t)
	_, ownerTok := e.user("alice")
	member, memberTok := e.user("bob")
	org := e.createOrg(ownerTok, "Acme")
	other := e.createOrg(ownerTok, "Other")
	e.addMember(org, member, store.RoleMember)
	if rec := e.send("PATCH", "/api/orgs/"+other.String(), ownerTok, `{"name":"Other 2"}`); rec.Code != 200 {
		t.Fatal(rec.Code)
	}

	rec := e.do("GET", "/api/orgs/"+org.String()+"/audit?org="+other.String(), ownerTok)
	if rec.Code != 200 {
		t.Fatalf("owner: %d %s", rec.Code, rec.Body.String())
	}
	var page struct {
		Entries []struct {
			OrgID  string
			Action string
		}
	}
	decode(t, rec, &page)
	if len(page.Entries) == 0 {
		t.Fatalf("no entries: %s", rec.Body.String())
	}
	for _, en := range page.Entries {
		if en.OrgID != org.String() {
			t.Fatalf("foreign row: %s", rec.Body.String())
		}
	}
	if rec := e.do("GET", "/api/orgs/"+org.String()+"/audit?limit=x", ownerTok); rec.Code != 400 {
		t.Fatalf("bad filter: %d", rec.Code)
	}

	if rec := e.do("GET", "/api/orgs/"+org.String()+"/audit", memberTok); rec.Code != 403 {
		t.Fatalf("member: %d", rec.Code)
	}
	es := e.audit("audit:read")
	if len(es) != 1 || es[0].Result != store.ResultDenied || es[0].ActorID != member.ID.String() || *es[0].OrgID != org || es[0].Resource != "org/audit" {
		t.Fatalf("denied audit: %+v", es)
	}
}

func TestPermissionChangeIsImmediate(t *testing.T) {
	e := newEnv(t)
	_, ownerTok := e.user("alice")
	member, memberTok := e.user("bob")
	org := e.createOrg(ownerTok, "Acme")
	e.addMember(org, member, store.RoleMember)
	path := "/api/orgs/" + org.String() + "/audit"
	if rec := e.do("GET", path, memberTok); rec.Code != 403 {
		t.Fatalf("before: %d", rec.Code)
	}
	var role uuid.UUID
	if err := e.pool.QueryRow(context.Background(), `INSERT INTO roles (org_id, name) VALUES ($1, 'auditor') RETURNING id`, org).Scan(&role); err != nil {
		t.Fatal(err)
	}
	e.exec(`INSERT INTO role_rules (role_id, position, effect, object, action) VALUES ($1, 0, 'allow', 'org/audit', 'audit:read')`, role)
	e.exec(`INSERT INTO role_bindings (org_id, subject, role_id) VALUES ($1, $2, $3)`, org, "user:"+member.ID.String(), role)
	e.reload()
	if rec := e.do("GET", path, memberTok); rec.Code != 200 {
		t.Fatalf("after: %d %s", rec.Code, rec.Body.String())
	}
}

func TestCommitRejectsInvalidPolicy(t *testing.T) {
	e := newEnv(t)
	u, tok := e.user("alice")
	org := e.createOrg(tok, "Acme")
	var role uuid.UUID
	if err := e.pool.QueryRow(context.Background(), `INSERT INTO roles (org_id, name) VALUES ($1, 'r') RETURNING id`, org).Scan(&role); err != nil {
		t.Fatal(err)
	}
	e.reload()
	before := e.z.Enforcer()
	// A poison row committed behind the API's back: any write that would commit it must fail.
	e.exec(`INSERT INTO role_bindings (org_id, subject, role_id) VALUES ($1, 'bogus', $2)`, org, role)

	h := &api{store: e.store, z: e.z}
	rec := httptest.NewRecorder()
	ok := h.commit(rec, httptest.NewRequest("POST", "/", nil), func(tx *store.Store) error {
		return tx.Audit(context.Background(), store.AuditEntry{ActorKind: store.ActorUser, ActorID: u.ID.String(), Action: "test:poison", Result: store.ResultAllowed})
	})
	if ok || rec.Code != 400 || !strings.Contains(rec.Body.String(), `"invalid policy"`) || strings.Contains(rec.Body.String(), "bogus") {
		t.Fatalf("commit: %v %d %s", ok, rec.Code, rec.Body.String())
	}
	if len(e.audit("test:poison")) != 0 {
		t.Fatal("audit row of rolled back tx stored")
	}
	if e.z.Enforcer() != before {
		t.Fatal("enforcer swapped")
	}
}

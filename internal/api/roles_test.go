package api

import (
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/asterism-dev/observatory/internal/authz"
	"github.com/asterism-dev/observatory/internal/store"
)

func rolesPath(org uuid.UUID) string { return "/api/orgs/" + org.String() + "/roles" }

func rolePath(org, role uuid.UUID) string { return rolesPath(org) + "/" + role.String() }

func bindingsPath(org uuid.UUID) string { return "/api/orgs/" + org.String() + "/bindings" }

func (e *env) createRole(tok string, org uuid.UUID, body string) uuid.UUID {
	e.t.Helper()
	rec := e.send("POST", rolesPath(org), tok, body)
	if rec.Code != 201 {
		e.t.Fatalf("create role: %d %s", rec.Code, rec.Body.String())
	}
	var got struct{ ID uuid.UUID }
	decode(e.t, rec, &got)
	return got.ID
}

func (e *env) bind(tok string, org uuid.UUID, subject string, role uuid.UUID) uuid.UUID {
	e.t.Helper()
	rec := e.send("POST", bindingsPath(org), tok, `{"subject":"`+subject+`","roleId":"`+role.String()+`"}`)
	if rec.Code != 201 {
		e.t.Fatalf("bind: %d %s", rec.Code, rec.Body.String())
	}
	var got struct{ ID uuid.UUID }
	decode(e.t, rec, &got)
	return got.ID
}

func (e *env) roleCount(org uuid.UUID) int {
	e.t.Helper()
	var n int
	if err := e.pool.QueryRow(e.t.Context(), `SELECT count(*) FROM roles WHERE org_id = $1`, org).Scan(&n); err != nil {
		e.t.Fatal(err)
	}
	return n
}

func (e *env) bindingCount(org uuid.UUID) int {
	e.t.Helper()
	var n int
	if err := e.pool.QueryRow(e.t.Context(), `SELECT count(*) FROM role_bindings WHERE org_id = $1`, org).Scan(&n); err != nil {
		e.t.Fatal(err)
	}
	return n
}

type roleJSONT struct {
	ID          uuid.UUID
	Key, Name   string
	Description string
	Builtin     bool
	Rules       []struct{ Effect, Object, Action string }
	Parents     []string
}

func TestRoleCRUDAndAudit(t *testing.T) {
	e := newEnv(t)
	owner, ownerTok := e.user("alice")
	member, memberTok := e.user("bob")
	org := e.createOrg(ownerTok, "Acme")
	e.addMember(org, member, store.RoleMember)

	var catalog []struct{ Action, Object string }
	rec := e.do("GET", "/api/orgs/"+org.String()+"/catalog", memberTok)
	decode(t, rec, &catalog)
	if rec.Code != 200 || len(catalog) != len(authz.Catalog) || catalog[0].Action != "iam.orgs.update" || catalog[0].Object != "org/settings" {
		t.Fatalf("catalog: %d %s", rec.Code, rec.Body.String())
	}

	if rec := e.send("POST", rolesPath(org), memberTok, `{"name":"x"}`); rec.Code != 403 {
		t.Fatalf("member create: %d", rec.Code)
	}
	for _, body := range []string{
		`{"name":""}`,
		`{"name":"x","description":"a\u0000b"}`,
		`{"name":"x","rules":[{"effect":"allow","object":"teams","action":"iam.teams.list"}]}`,
		`{"name":"x","rules":[{"effect":"maybe","object":"org/teams","action":"iam.teams.list"}]}`,
		`{"name":"x","rules":[{"effect":"allow","object":"org/teams","action":"nope.things.*"}]}`,
		`{"name":"x","parents":["owner"]}`,
		`{"name":"x","rules":[` + strings.Repeat(`{"effect":"allow","object":"org/roles","action":"iam.roles.list"},`, 200) + `{"effect":"allow","object":"org/roles","action":"iam.roles.list"}]}`,
		`nope`,
	} {
		if rec := e.send("POST", rolesPath(org), ownerTok, body); rec.Code != 400 {
			t.Fatalf("%s: %d %s", body, rec.Code, rec.Body.String())
		}
	}
	if e.roleCount(org) != 0 {
		t.Fatal("invalid role stored")
	}

	rec = e.send("POST", rolesPath(org), ownerTok, `{"name":" team lead ","description":"leads","rules":[{"effect":"allow","object":"org/teams/*","action":"iam.teams.*"}],"parents":["member"]}`)
	var created roleJSONT
	decode(t, rec, &created)
	if rec.Code != 201 || created.Name != "team lead" || created.Description != "leads" || len(created.Parents) != 1 || created.Parents[0] != "member" {
		t.Fatalf("create: %d %s", rec.Code, rec.Body.String())
	}
	// Wildcard actions are stored expanded so future catalog actions aren't granted implicitly.
	if len(created.Rules) != len(authz.ExpandActions("iam.teams.*")) || created.Rules[0].Action != "iam.teams.list" || created.Rules[0].Object != "org/teams/*" {
		t.Fatalf("rules not expanded: %+v", created.Rules)
	}
	if rec := e.send("POST", rolesPath(org), ownerTok, `{"name":"team lead"}`); rec.Code != 409 {
		t.Fatalf("duplicate: %d", rec.Code)
	}
	if es := e.audit(store.ActionRoleCreate); len(es) != 1 || es[0].Result != store.ResultAllowed || es[0].ActorID != owner.ID.String() || es[0].Resource != "org/roles/"+created.ID.String() {
		t.Fatalf("create audit: %+v", es)
	}

	var list []roleJSONT
	rec = e.do("GET", rolesPath(org), memberTok)
	decode(t, rec, &list)
	if rec.Code != 200 || len(list) != 4 || !list[0].Builtin || list[0].Key != "owner" || len(list[0].Rules) == 0 || list[3].ID != created.ID || list[3].Builtin {
		t.Fatalf("list: %d %s", rec.Code, rec.Body.String())
	}

	if rec := e.send("PUT", rolePath(org, created.ID), ownerTok, `{"name":"lead","rules":[{"effect":"deny","object":"org/teams/*","action":"iam.teams.delete"}],"parents":[]}`); rec.Code != 204 {
		t.Fatalf("update: %d %s", rec.Code, rec.Body.String())
	}
	rec = e.do("GET", rolesPath(org), ownerTok)
	decode(t, rec, &list)
	if got := list[3]; got.Name != "lead" || got.Description != "" || len(got.Rules) != 1 || got.Rules[0].Effect != "deny" || got.Parents == nil || len(got.Parents) != 0 {
		t.Fatalf("after update: %s", rec.Body.String())
	}
	if es := e.audit(store.ActionRoleUpdate); len(es) != 1 || es[0].Result != store.ResultAllowed || es[0].Details["name"] != "lead" {
		t.Fatalf("update audit: %+v", es)
	}
	if rec := e.send("PUT", rolePath(org, uuid.New()), ownerTok, `{"name":"y"}`); rec.Code != 404 {
		t.Fatalf("update unknown: %d", rec.Code)
	}
	if rec := e.do("DELETE", rolePath(org, created.ID), memberTok); rec.Code != 403 {
		t.Fatalf("member delete: %d", rec.Code)
	}
	if rec := e.do("DELETE", rolePath(org, created.ID), ownerTok); rec.Code != 204 {
		t.Fatalf("delete: %d %s", rec.Code, rec.Body.String())
	}
	if rec := e.do("DELETE", rolePath(org, created.ID), ownerTok); rec.Code != 404 {
		t.Fatalf("delete again: %d", rec.Code)
	}
	if es := e.audit(store.ActionRoleDelete); len(es) != 1 || es[0].Result != store.ResultAllowed {
		t.Fatalf("delete audit: %+v", es)
	}
}

func TestRoleParentCycleRejected(t *testing.T) {
	e := newEnv(t)
	_, ownerTok := e.user("alice")
	org := e.createOrg(ownerTok, "Acme")
	a := e.createRole(ownerTok, org, `{"name":"a"}`)
	b := e.createRole(ownerTok, org, `{"name":"b","parents":["`+a.String()+`"]}`)
	c := e.createRole(ownerTok, org, `{"name":"c","parents":["`+b.String()+`"]}`)
	for _, parent := range []uuid.UUID{a, b, c} {
		if rec := e.send("PUT", rolePath(org, a), ownerTok, `{"name":"a","parents":["`+parent.String()+`"]}`); rec.Code != 409 {
			t.Fatalf("parent %s: %d %s", parent, rec.Code, rec.Body.String())
		}
	}
	if rec := e.send("PUT", rolePath(org, c), ownerTok, `{"name":"c","parents":["`+a.String()+`","member"]}`); rec.Code != 204 {
		t.Fatalf("non-cyclic: %d %s", rec.Code, rec.Body.String())
	}
}

func TestRoleParentFromOtherOrgRejected(t *testing.T) {
	e := newEnv(t)
	_, ownerTok := e.user("alice")
	_, otherTok := e.user("mallory")
	org := e.createOrg(ownerTok, "Acme")
	orgB := e.createOrg(otherTok, "Evil")
	foreign := e.createRole(otherTok, orgB, `{"name":"x"}`)
	mine := e.createRole(ownerTok, org, `{"name":"mine"}`)

	rec := e.send("POST", rolesPath(org), ownerTok, `{"name":"y","parents":["`+foreign.String()+`"]}`)
	if rec.Code != 400 || strings.Contains(rec.Body.String(), orgB.String()) {
		t.Fatalf("create: %d %s", rec.Code, rec.Body.String())
	}
	if rec := e.send("PUT", rolePath(org, mine), ownerTok, `{"name":"mine","parents":["`+foreign.String()+`"]}`); rec.Code != 400 {
		t.Fatalf("update: %d", rec.Code)
	}
	if rec := e.send("PUT", rolePath(org, foreign), ownerTok, `{"name":"hijack"}`); rec.Code != 404 {
		t.Fatalf("update foreign role: %d", rec.Code)
	}
	if rec := e.do("DELETE", rolePath(org, foreign), ownerTok); rec.Code != 404 {
		t.Fatalf("delete foreign role: %d", rec.Code)
	}
	if e.roleCount(org) != 1 || e.roleCount(orgB) != 1 {
		t.Fatal("roles changed")
	}
}

func TestAdminCannotCreateEscalatingRole(t *testing.T) {
	e := newEnv(t)
	_, ownerTok := e.user("alice")
	admin, adminTok := e.user("bob")
	org := e.createOrg(ownerTok, "Acme")
	e.addMember(org, admin, store.RoleAdmin)
	harmless := e.createRole(ownerTok, org, `{"name":"harmless"}`)
	promoter := e.createRole(ownerTok, org, `{"name":"promoter","rules":[{"effect":"allow","object":"org/members","action":"iam.members.setRole"}]}`)

	rec := e.send("POST", rolesPath(org), adminTok, `{"name":"esc","rules":[{"effect":"allow","object":"org/members","action":"iam.members.*"}]}`)
	if rec.Code != 403 || !strings.Contains(rec.Body.String(), "iam.members.setRole") {
		t.Fatalf("create: %d %s", rec.Code, rec.Body.String())
	}
	if e.roleCount(org) != 2 {
		t.Fatal("escalating role stored")
	}
	es := e.audit(store.ActionRoleCreate)
	if len(es) == 0 || es[0].Result != store.ResultDenied || es[0].ActorID != admin.ID.String() || !strings.Contains(es[0].Details["error"].(string), "iam.members.setRole@org/members") {
		t.Fatalf("denied audit: %+v", es)
	}
	if rec := e.send("PUT", rolePath(org, harmless), adminTok, `{"name":"harmless","rules":[{"effect":"allow","object":"org/members","action":"iam.members.setRole"}]}`); rec.Code != 403 {
		t.Fatalf("update: %d", rec.Code)
	}
	if es := e.audit(store.ActionRoleUpdate); len(es) != 1 || es[0].Result != store.ResultDenied {
		t.Fatalf("update audit: %+v", es)
	}
	if rec := e.send("POST", rolesPath(org), adminTok, `{"name":"via parent","parents":["`+promoter.String()+`"]}`); rec.Code != 403 {
		t.Fatalf("via parent: %d", rec.Code)
	}
	if rec := e.send("POST", rolesPath(org), adminTok, `{"name":"ok","rules":[{"effect":"allow","object":"org/teams/*","action":"iam.teams.*"}],"parents":["admin"]}`); rec.Code != 201 {
		t.Fatalf("admin within own rights: %d %s", rec.Code, rec.Body.String())
	}
}

func TestMemberCannotWriteDeny(t *testing.T) {
	e := newEnv(t)
	_, ownerTok := e.user("alice")
	member, memberTok := e.user("bob")
	org := e.createOrg(ownerTok, "Acme")
	e.addMember(org, member, store.RoleMember)
	writer := e.createRole(ownerTok, org, `{"name":"writer","rules":[{"effect":"allow","object":"org/roles","action":"iam.roles.*"}]}`)
	e.bind(ownerTok, org, authz.UserSubject(member.ID), writer)
	denying := e.createRole(ownerTok, org, `{"name":"denying","rules":[{"effect":"deny","object":"org/audit","action":"audit.events.list"}]}`)

	if rec := e.send("POST", rolesPath(org), memberTok, `{"name":"d","rules":[{"effect":"deny","object":"org/teams/*","action":"iam.teams.delete"}]}`); rec.Code != 403 {
		t.Fatalf("deny: %d %s", rec.Code, rec.Body.String())
	}
	if es := e.audit(store.ActionRoleCreate); es[0].Result != store.ResultDenied || es[0].ActorID != member.ID.String() {
		t.Fatalf("denied audit: %+v", es)
	}
	// Removing a deny is writing one too.
	if rec := e.send("PUT", rolePath(org, denying), memberTok, `{"name":"denying"}`); rec.Code != 403 {
		t.Fatalf("lift deny: %d", rec.Code)
	}
	if rec := e.do("DELETE", rolePath(org, denying), memberTok); rec.Code != 403 {
		t.Fatalf("delete deny role: %d", rec.Code)
	}
	if rec := e.send("POST", rolesPath(org), memberTok, `{"name":"reader","rules":[{"effect":"allow","object":"org/roles","action":"iam.roles.list"}]}`); rec.Code != 201 {
		t.Fatalf("allow within rights: %d %s", rec.Code, rec.Body.String())
	}
}

func TestBindRoleAppliesImmediately(t *testing.T) {
	e := newEnv(t)
	_, ownerTok := e.user("alice")
	member, memberTok := e.user("bob")
	org := e.createOrg(ownerTok, "Acme")
	e.addMember(org, member, store.RoleMember)
	auditor := e.createRole(ownerTok, org, `{"name":"auditor","rules":[{"effect":"allow","object":"org/audit","action":"audit.events.list"}]}`)

	if e.canReadAudit(member.ID, org) {
		t.Fatal("member reads the audit trail before the bind")
	}
	binding := e.bind(ownerTok, org, authz.UserSubject(member.ID), auditor)
	if !e.canReadAudit(member.ID, org) {
		t.Fatal("bind not applied")
	}
	var list []struct {
		ID      uuid.UUID
		Subject string
		RoleID  uuid.UUID
	}
	rec := e.do("GET", bindingsPath(org), ownerTok)
	decode(t, rec, &list)
	if rec.Code != 200 || len(list) != 1 || list[0].ID != binding || list[0].Subject != authz.UserSubject(member.ID) || list[0].RoleID != auditor {
		t.Fatalf("bindings: %d %s", rec.Code, rec.Body.String())
	}
	if es := e.audit(store.ActionRoleBind); len(es) != 1 || es[0].Result != store.ResultAllowed || es[0].Details["subject"] != authz.UserSubject(member.ID) {
		t.Fatalf("bind audit: %+v", es)
	}
	if rec := e.do("GET", bindingsPath(org), memberTok); rec.Code != 403 {
		t.Fatalf("member lists bindings: %d", rec.Code)
	}

	if rec := e.do("DELETE", bindingsPath(org)+"/"+binding.String(), ownerTok); rec.Code != 204 {
		t.Fatalf("unbind: %d %s", rec.Code, rec.Body.String())
	}
	if e.canReadAudit(member.ID, org) {
		t.Fatal("unbind not applied")
	}
	if rec := e.do("DELETE", bindingsPath(org)+"/"+binding.String(), ownerTok); rec.Code != 404 {
		t.Fatalf("unbind again: %d", rec.Code)
	}
	if es := e.audit(store.ActionRoleUnbind); len(es) != 1 || es[0].Result != store.ResultAllowed {
		t.Fatalf("unbind audit: %+v", es)
	}

	team := e.createTeam(ownerTok, org, "core")
	if rec := e.send("PUT", teamMemberPath(org, team, member), ownerTok, `{"admin":false}`); rec.Code != 204 {
		t.Fatalf("team member: %d", rec.Code)
	}
	e.bind(ownerTok, org, authz.TeamSubject(team), auditor)
	if !e.canReadAudit(member.ID, org) {
		t.Fatal("team binding not applied")
	}
}

func TestBindingEscalation(t *testing.T) {
	e := newEnv(t)
	_, ownerTok := e.user("alice")
	admin, adminTok := e.user("bob")
	org := e.createOrg(ownerTok, "Acme")
	e.addMember(org, admin, store.RoleAdmin)
	promoter := e.createRole(ownerTok, org, `{"name":"promoter","rules":[{"effect":"allow","object":"org/members","action":"iam.members.setRole"}]}`)

	rec := e.send("POST", bindingsPath(org), adminTok, `{"subject":"`+authz.UserSubject(admin.ID)+`","roleId":"`+promoter.String()+`"}`)
	if rec.Code != 403 || !strings.Contains(rec.Body.String(), "iam.members.setRole") {
		t.Fatalf("bind: %d %s", rec.Code, rec.Body.String())
	}
	if e.bindingCount(org) != 0 {
		t.Fatal("binding stored")
	}
	es := e.audit(store.ActionRoleBind)
	if len(es) != 1 || es[0].Result != store.ResultDenied || es[0].ActorID != admin.ID.String() || !strings.Contains(es[0].Details["error"].(string), "iam.members.setRole") {
		t.Fatalf("denied audit: %+v", es)
	}
}

func TestDenyRuleRestrictsAdmin(t *testing.T) {
	e := newEnv(t)
	_, ownerTok := e.user("alice")
	admin, adminTok := e.user("bob")
	org := e.createOrg(ownerTok, "Acme")
	e.addMember(org, admin, store.RoleAdmin)
	team := e.createTeam(ownerTok, org, "core")
	noDelete := e.createRole(ownerTok, org, `{"name":"no delete","rules":[{"effect":"deny","object":"org/teams/*","action":"iam.teams.delete"}]}`)
	wrapper := e.createRole(ownerTok, org, `{"name":"wrapper","parents":["`+noDelete.String()+`"]}`)
	binding := e.bind(ownerTok, org, authz.UserSubject(admin.ID), wrapper)

	if rec := e.do("DELETE", teamPath(org, team), adminTok); rec.Code != 403 {
		t.Fatalf("team delete: %d", rec.Code)
	}
	// The admin may not lift a deny that binds them, by any route.
	before := len(e.audit(store.ActionRoleUnbind))
	if rec := e.do("DELETE", bindingsPath(org)+"/"+binding.String(), adminTok); rec.Code != 403 {
		t.Fatalf("unbind own deny: %d", rec.Code)
	}
	if es := e.audit(store.ActionRoleUnbind); len(es) != before+1 || es[0].Result != store.ResultDenied || es[0].ActorID != admin.ID.String() ||
		!strings.Contains(es[0].Details["error"].(string), "deny iam.teams.delete@org/teams/*") {
		t.Fatalf("unbind denied audit: %+v", es)
	}
	if rec := e.send("PUT", rolePath(org, noDelete), adminTok, `{"name":"no delete"}`); rec.Code != 403 {
		t.Fatalf("edit deny away: %d", rec.Code)
	}
	if rec := e.send("PUT", rolePath(org, wrapper), adminTok, `{"name":"wrapper"}`); rec.Code != 403 {
		t.Fatalf("drop parent: %d", rec.Code)
	}
	if rec := e.do("DELETE", rolePath(org, noDelete), adminTok); rec.Code != 403 {
		t.Fatalf("delete deny role: %d", rec.Code)
	}
	if e.bindingCount(org) != 1 || e.roleCount(org) != 2 {
		t.Fatal("policy changed")
	}
	if rec := e.do("DELETE", bindingsPath(org)+"/"+binding.String(), ownerTok); rec.Code != 204 {
		t.Fatalf("owner unbinds: %d", rec.Code)
	}
	if rec := e.do("DELETE", teamPath(org, team), adminTok); rec.Code != 204 {
		t.Fatalf("team delete after unbind: %d", rec.Code)
	}
}

func TestBindForeignSubject(t *testing.T) {
	e := newEnv(t)
	_, ownerTok := e.user("alice")
	stranger, strangerTok := e.user("mallory")
	org := e.createOrg(ownerTok, "Acme")
	orgB := e.createOrg(strangerTok, "Evil")
	foreignTeam := e.createTeam(strangerTok, orgB, "x")
	foreignRole := e.createRole(strangerTok, orgB, `{"name":"x"}`)
	role := e.createRole(ownerTok, org, `{"name":"r"}`)

	for _, body := range []string{
		`{"subject":"` + authz.UserSubject(stranger.ID) + `","roleId":"` + role.String() + `"}`,
		`{"subject":"` + authz.TeamSubject(foreignTeam) + `","roleId":"` + role.String() + `"}`,
		`{"subject":"user:` + strings.ToUpper(stranger.ID.String()) + `","roleId":"` + role.String() + `"}`,
		`{"subject":"bogus","roleId":"` + role.String() + `"}`,
		`{"subject":"` + authz.TeamSubject(foreignTeam) + `","roleId":"` + foreignRole.String() + `"}`,
	} {
		rec := e.send("POST", bindingsPath(org), ownerTok, body)
		if rec.Code != 400 || strings.Contains(rec.Body.String(), orgB.String()) {
			t.Fatalf("%s: %d %s", body, rec.Code, rec.Body.String())
		}
	}
	if e.bindingCount(org) != 0 || e.bindingCount(orgB) != 0 {
		t.Fatal("binding stored")
	}
	team := e.createTeam(ownerTok, org, "core")
	e.bind(ownerTok, org, authz.TeamSubject(team), role)
	if rec := e.send("POST", bindingsPath(org), ownerTok, `{"subject":"`+authz.TeamSubject(team)+`","roleId":"`+role.String()+`"}`); rec.Code != 409 {
		t.Fatalf("duplicate: %d", rec.Code)
	}
}

func TestWildcardDenyKeptAsWritten(t *testing.T) {
	e := newEnv(t)
	_, ownerTok := e.user("alice")
	admin, adminTok := e.user("bob")
	org := e.createOrg(ownerTok, "Acme")
	e.addMember(org, admin, store.RoleAdmin)
	team := e.createTeam(ownerTok, org, "core")
	rec := e.send("POST", rolesPath(org), ownerTok, `{"name":"no teams","rules":[{"effect":"deny","object":"org/teams/*","action":"iam.teams.*"},{"effect":"allow","object":"org/audit","action":"audit.*"}]}`)
	var created roleJSONT
	decode(t, rec, &created)
	if rec.Code != 201 || len(created.Rules) != 4 || created.Rules[0].Action != "iam.teams.*" || created.Rules[1].Action != "audit.events.list" || created.Rules[3].Action != "audit.settings.update" {
		t.Fatalf("create: %d %s", rec.Code, rec.Body.String())
	}
	e.bind(ownerTok, org, authz.UserSubject(admin.ID), created.ID)
	if rec := e.send("PATCH", teamPath(org, team), adminTok, `{"name":"x"}`); rec.Code != 403 {
		t.Fatalf("rename: %d", rec.Code)
	}
	if rec := e.do("DELETE", teamPath(org, team), adminTok); rec.Code != 403 {
		t.Fatalf("delete: %d", rec.Code)
	}
}

func TestTeamJoinEscalation(t *testing.T) {
	e := newEnv(t)
	_, ownerTok := e.user("alice")
	admin, adminTok := e.user("bob")
	member, _ := e.user("carol")
	lead, leadTok := e.user("dave")
	org := e.createOrg(ownerTok, "Acme")
	e.addMember(org, admin, store.RoleAdmin)
	e.addMember(org, member, store.RoleMember)
	e.addMember(org, lead, store.RoleMember)
	promoter := e.createRole(ownerTok, org, `{"name":"promoter","rules":[{"effect":"allow","object":"org/members","action":"iam.members.setRole"}]}`)
	elevated := e.createTeam(ownerTok, org, "elevated")
	e.bind(ownerTok, org, authz.TeamSubject(elevated), promoter)

	for _, u := range []store.User{admin, member} {
		if rec := e.send("PUT", teamMemberPath(org, elevated, u), adminTok, `{"admin":false}`); rec.Code != 403 {
			t.Fatalf("admin adds %s: %d %s", u.Name, rec.Code, rec.Body.String())
		}
	}
	es := e.audit(store.ActionTeamMemberSet)
	if len(es) != 2 || es[0].Result != store.ResultDenied || es[0].ActorID != admin.ID.String() || !strings.Contains(es[0].Details["error"].(string), "iam.members.setRole@org/members") {
		t.Fatalf("denied audit: %+v", es)
	}
	if ms, _ := e.store.TeamMembers(t.Context(), org, elevated); len(ms) != 0 {
		t.Fatalf("members added: %+v", ms)
	}
	if rec := e.send("PUT", teamMemberPath(org, elevated, member), ownerTok, `{"admin":false}`); rec.Code != 204 {
		t.Fatalf("owner adds: %d %s", rec.Code, rec.Body.String())
	}

	denying := e.createRole(ownerTok, org, `{"name":"denying","rules":[{"effect":"deny","object":"org/audit","action":"audit.events.list"}]}`)
	restricted := e.createTeam(ownerTok, org, "restricted")
	e.bind(ownerTok, org, authz.TeamSubject(restricted), denying)
	if rec := e.send("PUT", teamMemberPath(org, restricted, lead), ownerTok, `{"admin":true}`); rec.Code != 204 {
		t.Fatalf("owner makes lead: %d", rec.Code)
	}
	if rec := e.send("PUT", teamMemberPath(org, restricted, member), leadTok, `{"admin":false}`); rec.Code != 403 {
		t.Fatalf("team admin adds to deny team: %d", rec.Code)
	}
	if rec := e.send("PUT", teamMemberPath(org, restricted, member), adminTok, `{"admin":false}`); rec.Code != 204 {
		t.Fatalf("org admin adds to deny team: %d %s", rec.Code, rec.Body.String())
	}
	if rec := e.do("DELETE", teamMemberPath(org, restricted, member), leadTok); rec.Code != 403 {
		t.Fatalf("team admin removes from deny team: %d", rec.Code)
	}
}

func TestTeamDenyEscape(t *testing.T) {
	e := newEnv(t)
	_, ownerTok := e.user("alice")
	admin, adminTok := e.user("bob")
	member, memberTok := e.user("carol")
	org := e.createOrg(ownerTok, "Acme")
	e.addMember(org, admin, store.RoleAdmin)
	e.addMember(org, member, store.RoleMember)
	denying := e.createRole(ownerTok, org, `{"name":"no audit","rules":[{"effect":"deny","object":"org/audit","action":"audit.events.list"}]}`)
	team := e.createTeam(ownerTok, org, "restricted")
	e.bind(ownerTok, org, authz.TeamSubject(team), denying)
	if rec := e.send("PUT", teamMemberPath(org, team, admin), ownerTok, `{"admin":false}`); rec.Code != 204 {
		t.Fatalf("add admin: %d", rec.Code)
	}
	if e.canReadAudit(admin.ID, org) {
		t.Fatal("deny not applied")
	}

	if rec := e.do("DELETE", teamMemberPath(org, team, admin), adminTok); rec.Code != 403 {
		t.Fatalf("leave deny team: %d %s", rec.Code, rec.Body.String())
	}
	if es := e.audit(store.ActionTeamLeave); len(es) != 1 || es[0].Result != store.ResultDenied || !strings.Contains(es[0].Details["error"].(string), "deny audit.events.list@org/audit") {
		t.Fatalf("leave audit: %+v", es)
	}
	if rec := e.do("DELETE", teamPath(org, team), adminTok); rec.Code != 403 {
		t.Fatalf("delete deny team: %d", rec.Code)
	}
	if es := e.audit(store.ActionTeamDelete); len(es) == 0 || es[0].Result != store.ResultDenied || es[0].ActorID != admin.ID.String() {
		t.Fatalf("delete audit: %+v", es)
	}
	if e.canReadAudit(admin.ID, org) {
		t.Fatal("deny lifted")
	}

	deleter := e.createRole(ownerTok, org, `{"name":"deleter","rules":[{"effect":"allow","object":"org/teams/*","action":"iam.teams.delete"}]}`)
	e.bind(ownerTok, org, authz.UserSubject(member.ID), deleter)
	if rec := e.do("DELETE", teamPath(org, team), memberTok); rec.Code != 403 {
		t.Fatalf("member deletes deny team: %d", rec.Code)
	}
	plain := e.createTeam(ownerTok, org, "plain")
	if rec := e.do("DELETE", teamPath(org, plain), memberTok); rec.Code != 204 {
		t.Fatalf("member deletes plain team: %d", rec.Code)
	}
	if rec := e.do("DELETE", teamPath(org, team), ownerTok); rec.Code != 204 {
		t.Fatalf("owner deletes: %d", rec.Code)
	}
	if !e.canReadAudit(admin.ID, org) {
		t.Fatal("deny kept after owner delete")
	}
}

// Guards must read the policy under the org lock: a deny committed after the last enforcer reload still counts.
func TestGuardsSeeUnreloadedPolicy(t *testing.T) {
	e := newEnv(t)
	_, ownerTok := e.user("alice")
	member, memberTok := e.user("bob")
	org := e.createOrg(ownerTok, "Acme")
	e.addMember(org, member, store.RoleMember)
	writer := e.createRole(ownerTok, org, `{"name":"writer","rules":[{"effect":"allow","object":"org/roles","action":"iam.roles.*"},{"effect":"allow","object":"org/roles","action":"iam.roleBindings.manage"}]}`)
	e.bind(ownerTok, org, authz.UserSubject(member.ID), writer)
	target := e.createRole(ownerTok, org, `{"name":"target"}`)

	e.exec(`INSERT INTO role_rules (role_id, position, effect, object, action) VALUES ($1, 0, 'deny', 'org/audit', 'audit.events.list')`, target)
	if rec := e.send("PUT", rolePath(org, target), memberTok, `{"name":"target"}`); rec.Code != 403 {
		t.Fatalf("update: %d %s", rec.Code, rec.Body.String())
	}
	if es := e.audit(store.ActionRoleUpdate); len(es) != 1 || es[0].Result != store.ResultDenied || es[0].ActorID != member.ID.String() {
		t.Fatalf("update audit: %+v", es)
	}
	if rec := e.do("DELETE", rolePath(org, target), memberTok); rec.Code != 403 {
		t.Fatalf("delete: %d", rec.Code)
	}
	if rec := e.send("POST", bindingsPath(org), memberTok, `{"subject":"`+authz.UserSubject(member.ID)+`","roleId":"`+target.String()+`"}`); rec.Code != 403 {
		t.Fatalf("bind: %d", rec.Code)
	}
	if es := e.audit(store.ActionRoleBind); es[0].Result != store.ResultDenied {
		t.Fatalf("bind audit: %+v", es)
	}
	var n int
	if err := e.pool.QueryRow(t.Context(), `SELECT count(*) FROM role_rules WHERE role_id = $1 AND effect = 'deny'`, target).Scan(&n); err != nil || n != 1 || e.roleCount(org) != 2 {
		t.Fatalf("deny lost: %d %v", n, err)
	}

	var fresh uuid.UUID
	if err := e.pool.QueryRow(t.Context(), `INSERT INTO roles (org_id, name, description) VALUES ($1, 'fresh', '') RETURNING id`, org).Scan(&fresh); err != nil {
		t.Fatal(err)
	}
	if rec := e.send("POST", bindingsPath(org), ownerTok, `{"subject":"`+authz.UserSubject(member.ID)+`","roleId":"`+fresh.String()+`"}`); rec.Code != 201 {
		t.Fatalf("bind unreloaded role: %d %s", rec.Code, rec.Body.String())
	}
}

func TestNonAdminCannotAttachDenyViaParent(t *testing.T) {
	e := newEnv(t)
	_, ownerTok := e.user("alice")
	admin, _ := e.user("bob")
	member, memberTok := e.user("carol")
	org := e.createOrg(ownerTok, "Acme")
	e.addMember(org, admin, store.RoleAdmin)
	e.addMember(org, member, store.RoleMember)
	writer := e.createRole(ownerTok, org, `{"name":"writer","rules":[{"effect":"allow","object":"org/roles","action":"iam.roles.*"}]}`)
	e.bind(ownerTok, org, authz.UserSubject(member.ID), writer)
	harmless := e.createRole(ownerTok, org, `{"name":"harmless"}`)
	e.bind(ownerTok, org, authz.UserSubject(admin.ID), harmless)
	denying := e.createRole(ownerTok, org, `{"name":"denying","rules":[{"effect":"deny","object":"org/audit","action":"audit.events.list"}]}`)
	wrapper := e.createRole(ownerTok, org, `{"name":"wrapper","parents":["`+denying.String()+`"]}`)

	for _, parent := range []uuid.UUID{denying, wrapper} {
		body := `{"name":"harmless","parents":["` + parent.String() + `"]}`
		if rec := e.send("PUT", rolePath(org, harmless), memberTok, body); rec.Code != 403 {
			t.Fatalf("update with deny parent: %d %s", rec.Code, rec.Body.String())
		}
		if rec := e.send("POST", rolesPath(org), memberTok, `{"name":"new","parents":["`+parent.String()+`"]}`); rec.Code != 403 {
			t.Fatalf("create with deny parent: %d %s", rec.Code, rec.Body.String())
		}
	}
	if es := e.audit(store.ActionRoleUpdate); len(es) != 2 || es[0].Result != store.ResultDenied || es[0].ActorID != member.ID.String() {
		t.Fatalf("update audit: %+v", es)
	}
	if es := e.audit(store.ActionRoleCreate); es[0].Result != store.ResultDenied {
		t.Fatalf("create audit: %+v", es)
	}
	if e.roleCount(org) != 4 {
		t.Fatal("role created")
	}
	if !e.canReadAudit(admin.ID, org) {
		t.Fatal("deny reached admin")
	}
}

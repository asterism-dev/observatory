package api

import (
	"context"
	"errors"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/asterism-dev/observatory/internal/authz"
	"github.com/asterism-dev/observatory/internal/store"
)

type envListJSON struct {
	Vars []struct {
		ID, Name, Kind, Scope string
		TeamID                *uuid.UUID
		TeamName              string
		Value                 *string
		Hosts                 []string
		UpdatedBy             struct{ ID, Name string }
		CanEdit, CanDelete    bool
	}
	WritableScopes []struct {
		Scope    string
		TeamID   *uuid.UUID
		TeamName string
	}
}

func envPath(org uuid.UUID) string { return "/api/orgs/" + org.String() + "/env" }

func envVarPath(org uuid.UUID, id string) string { return envPath(org) + "/" + id }

func (e *env) createEnv(tok string, org uuid.UUID, body string) string {
	e.t.Helper()
	rec := e.send("POST", envPath(org), tok, body)
	if rec.Code != 201 {
		e.t.Fatalf("create env %s: %d %s", body, rec.Code, rec.Body.String())
	}
	var got struct{ ID string }
	decode(e.t, rec, &got)
	return got.ID
}

func (e *env) listEnv(tok string, org uuid.UUID) envListJSON {
	e.t.Helper()
	rec := e.do("GET", envPath(org), tok)
	if rec.Code != 200 {
		e.t.Fatalf("list env: %d %s", rec.Code, rec.Body.String())
	}
	var got envListJSON
	decode(e.t, rec, &got)
	return got
}

func (e *env) makeTeamAdmin(ownerTok string, org, team uuid.UUID, u store.User) {
	e.t.Helper()
	if rec := e.send("PUT", teamMemberPath(org, team, u), ownerTok, `{"admin":true}`); rec.Code != 204 {
		e.t.Fatalf("make team admin: %d %s", rec.Code, rec.Body.String())
	}
}

func (e *env) resolve(org uuid.UUID, owner string) map[string]string {
	e.t.Helper()
	got, err := e.store.ResolveEnv(context.Background(), e.z.Enforcer(), org, owner)
	if err != nil {
		e.t.Fatal(err)
	}
	return got
}

func TestEnvPrivateSecretNeverLeaks(t *testing.T) {
	e := newEnv(t)
	_, ownerTok := e.user("alice")
	bob, bobTok := e.user("bob")
	carol, carolTok := e.user("carol")
	org := e.createOrg(ownerTok, "Acme")
	e.addMember(org, bob, store.RoleMember)
	e.addMember(org, carol, store.RoleMember)
	const secret = "hunter2-very-secret"
	var bodies []string
	send := func(method, path, tok, body string) *httptest.ResponseRecorder {
		rec := e.send(method, path, tok, body)
		bodies = append(bodies, rec.Body.String())
		return rec
	}

	rec := send("POST", envPath(org), bobTok, `{"name":"GH_TOKEN","value":"`+secret+`","kind":"secret","scope":"user"}`)
	if rec.Code != 201 {
		t.Fatalf("create: %d %s", rec.Code, rec.Body.String())
	}
	var created struct{ ID string }
	decode(t, rec, &created)
	id := created.ID

	rec = send("GET", envPath(org), bobTok, "")
	var list envListJSON
	decode(t, rec, &list)
	if len(list.Vars) != 1 || list.Vars[0].ID != id || list.Vars[0].Scope != "user" || list.Vars[0].Kind != "secret" ||
		list.Vars[0].Value != nil || !list.Vars[0].CanEdit || !list.Vars[0].CanDelete || list.Vars[0].UpdatedBy.Name != "bob" {
		t.Fatalf("bob list: %s", rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), `"value"`) {
		t.Fatalf("secret has a value field: %s", rec.Body.String())
	}

	for _, tok := range []string{carolTok, ownerTok} {
		decode(t, send("GET", envPath(org), tok, ""), &list)
		if len(list.Vars) != 0 {
			t.Fatalf("other user sees private var: %+v", list.Vars)
		}
		if rec := send("PATCH", envVarPath(org, id), tok, `{"name":"X"}`); rec.Code != 404 {
			t.Fatalf("patch other's private: %d", rec.Code)
		}
		if rec := send("POST", envVarPath(org, id)+"/move", tok, `{"scope":"org"}`); rec.Code != 404 {
			t.Fatalf("move other's private: %d", rec.Code)
		}
		if rec := send("DELETE", envVarPath(org, id), tok, ""); rec.Code != 404 {
			t.Fatalf("delete other's private: %d", rec.Code)
		}
	}
	for _, a := range []string{store.ActionEnvSet, store.ActionEnvDelete, store.ActionEnvMove} {
		for _, es := range e.audit(a) {
			if es.Result == store.ResultDenied {
				t.Fatalf("denial audited for hidden var: %+v", es)
			}
		}
	}

	if rec := send("PATCH", envVarPath(org, id), bobTok, `{"name":"GH_TOKEN2"}`); rec.Code != 204 {
		t.Fatalf("edit without value: %d %s", rec.Code, rec.Body.String())
	}
	if got := e.resolve(org, authz.UserSubject(bob.ID)); got["GH_TOKEN2"] != secret {
		t.Fatalf("value not kept: %v", got)
	}
	if rec := send("PATCH", envVarPath(org, id), bobTok, `{"kind":"env"}`); rec.Code != 400 {
		t.Fatalf("secret to env: %d", rec.Code)
	}
	if rec := send("DELETE", envVarPath(org, id), bobTok, ""); rec.Code != 204 {
		t.Fatalf("delete: %d", rec.Code)
	}

	for _, b := range bodies {
		if strings.Contains(b, secret) {
			t.Fatalf("secret in response: %s", b)
		}
	}
	for _, a := range []string{store.ActionEnvSet, store.ActionEnvDelete} {
		for _, es := range e.audit(a) {
			if strings.Contains(es.Resource, secret) || strings.Contains(strings.Join(mapValues(es.Details), " "), secret) {
				t.Fatalf("secret in audit: %+v", es)
			}
		}
	}
}

func TestEnvOrgScopeAndAudit(t *testing.T) {
	e := newEnv(t)
	owner, ownerTok := e.user("alice")
	bob, bobTok := e.user("bob")
	org := e.createOrg(ownerTok, "Acme")
	e.addMember(org, bob, store.RoleMember)

	if rec := e.send("POST", envPath(org), bobTok, `{"name":"LOG_LEVEL","value":"debug","kind":"env","scope":"org"}`); rec.Code != 403 {
		t.Fatalf("member creates org var: %d", rec.Code)
	}
	if es := e.audit(store.ActionEnvSet); len(es) != 1 || es[0].Result != store.ResultDenied || es[0].Resource != authz.ObjEnv || es[0].ActorID != bob.ID.String() {
		t.Fatalf("denied audit: %+v", es)
	}
	id := e.createEnv(ownerTok, org, `{"name":"LOG_LEVEL","value":"debug","kind":"env","scope":"org"}`)
	es := e.audit(store.ActionEnvSet)
	if len(es) != 2 || es[0].Result != store.ResultAllowed || es[0].Resource != authz.ObjEnv || es[0].ActorID != owner.ID.String() ||
		es[0].Details["name"] != "LOG_LEVEL" || es[0].Details["kind"] != "env" || es[0].Details["scope"] != "org" ||
		es[0].Details["ownerId"] != nil || es[0].Details["created"] != true {
		t.Fatalf("create audit: %+v", es)
	}
	if _, ok := es[0].Details["value"]; ok {
		t.Fatalf("value in audit: %+v", es[0].Details)
	}

	list := e.listEnv(bobTok, org)
	if len(list.Vars) != 1 || list.Vars[0].Value == nil || *list.Vars[0].Value != "debug" || list.Vars[0].CanEdit || list.Vars[0].CanDelete {
		t.Fatalf("member list: %+v", list)
	}
	if rec := e.send("PATCH", envVarPath(org, id), bobTok, `{"value":"info"}`); rec.Code != 403 {
		t.Fatalf("member edits org var: %d", rec.Code)
	}
	if rec := e.do("DELETE", envVarPath(org, id), bobTok); rec.Code != 403 {
		t.Fatalf("member deletes org var: %d", rec.Code)
	}

	if rec := e.send("PATCH", envVarPath(org, id), ownerTok, `{"value":"info","kind":"secret"}`); rec.Code != 204 {
		t.Fatalf("env to secret: %d %s", rec.Code, rec.Body.String())
	}
	if es := e.audit(store.ActionEnvSet); es[0].Details["created"] != false || es[0].Details["kind"] != "secret" {
		t.Fatalf("update audit: %+v", es[0])
	}
	if got := e.resolve(org, authz.UserSubject(bob.ID)); got["LOG_LEVEL"] != "info" {
		t.Fatalf("resolve: %v", got)
	}
	if list := e.listEnv(ownerTok, org); list.Vars[0].Value != nil || list.Vars[0].Kind != "secret" || !list.Vars[0].CanEdit {
		t.Fatalf("after kind change: %+v", list)
	}

	if rec := e.do("DELETE", envVarPath(org, id), ownerTok); rec.Code != 204 {
		t.Fatalf("delete: %d", rec.Code)
	}
	if es := e.audit(store.ActionEnvDelete); len(es) != 2 || es[0].Result != store.ResultAllowed || es[0].Resource != authz.ObjEnv ||
		es[0].Details["name"] != "LOG_LEVEL" || es[0].Details["scope"] != "org" {
		t.Fatalf("delete audit: %+v", es)
	}
	if rec := e.do("DELETE", envVarPath(org, id), ownerTok); rec.Code != 404 {
		t.Fatalf("delete again: %d", rec.Code)
	}
	if rec := e.do("DELETE", envVarPath(org, "nope"), ownerTok); rec.Code != 404 {
		t.Fatalf("bad id: %d", rec.Code)
	}
}

func TestEnvTeamScope(t *testing.T) {
	e := newEnv(t)
	_, ownerTok := e.user("alice")
	bob, bobTok := e.user("bob")
	org := e.createOrg(ownerTok, "Acme")
	otherOrg := e.createOrg(ownerTok, "Other")
	e.addMember(org, bob, store.RoleMember)
	core, ops := e.createTeam(ownerTok, org, "core"), e.createTeam(ownerTok, org, "ops")
	foreign := e.createTeam(ownerTok, otherOrg, "foreign")
	e.makeTeamAdmin(ownerTok, org, core, bob)

	id := e.createEnv(bobTok, org, `{"name":"DB_URL","value":"x","kind":"secret","scope":"team","teamId":"`+core.String()+`"}`)
	if es := e.audit(store.ActionEnvSet); es[0].Resource != authz.TeamEnvObject(core) || es[0].Details["ownerId"] != core.String() || es[0].Details["scope"] != "team" {
		t.Fatalf("team audit: %+v", es[0])
	}
	if rec := e.send("POST", envPath(org), bobTok, `{"name":"DB_URL","value":"x","kind":"secret","scope":"team","teamId":"`+ops.String()+`"}`); rec.Code != 403 {
		t.Fatalf("other team: %d", rec.Code)
	}
	for _, team := range []string{foreign.String(), uuid.NewString(), "nope", ""} {
		if rec := e.send("POST", envPath(org), ownerTok, `{"name":"A","value":"x","kind":"env","scope":"team","teamId":"`+team+`"}`); rec.Code != 400 {
			t.Fatalf("team %q: %d", team, rec.Code)
		}
	}

	list := e.listEnv(bobTok, org)
	if len(list.Vars) != 1 || list.Vars[0].ID != id || list.Vars[0].TeamID == nil || *list.Vars[0].TeamID != core || list.Vars[0].TeamName != "core" || !list.Vars[0].CanEdit {
		t.Fatalf("team list: %+v", list)
	}
	if len(list.WritableScopes) != 2 || list.WritableScopes[0].Scope != "team" || *list.WritableScopes[0].TeamID != core ||
		list.WritableScopes[0].TeamName != "core" || list.WritableScopes[1].Scope != "user" {
		t.Fatalf("team admin writable scopes: %+v", list.WritableScopes)
	}

	// A team admin may not move into the org scope nor pull an org var into the team (no env:delete on org/env).
	if rec := e.send("POST", envVarPath(org, id)+"/move", bobTok, `{"scope":"org"}`); rec.Code != 403 {
		t.Fatalf("team admin moves to org: %d", rec.Code)
	}
	orgID := e.createEnv(ownerTok, org, `{"name":"DB_URL","value":"y","kind":"secret","scope":"org"}`)
	if rec := e.send("POST", envVarPath(org, orgID)+"/move", bobTok, `{"scope":"team","teamId":"`+core.String()+`"}`); rec.Code != 403 {
		t.Fatalf("team admin pulls org var: %d", rec.Code)
	}
	if rec := e.send("POST", envVarPath(org, orgID)+"/move", ownerTok, `{"scope":"team","teamId":"`+core.String()+`"}`); rec.Code != 409 {
		t.Fatalf("move onto same name: %d", rec.Code)
	}
	if rec := e.send("POST", envVarPath(org, orgID)+"/move", ownerTok, `{"scope":"team","teamId":"`+foreign.String()+`"}`); rec.Code != 400 {
		t.Fatalf("move to foreign team: %d", rec.Code)
	}
	if rec := e.send("POST", envVarPath(org, orgID)+"/move", ownerTok, `{"scope":"team","teamId":"`+ops.String()+`"}`); rec.Code != 204 {
		t.Fatalf("move: %d %s", rec.Code, rec.Body.String())
	}
	es := e.audit(store.ActionEnvMove)
	if len(es) != 1 || es[0].Result != store.ResultAllowed || es[0].Resource != authz.TeamEnvObject(ops) ||
		es[0].Details["scope"] != "team" || es[0].Details["ownerId"] != ops.String() || es[0].Details["name"] != "DB_URL" || es[0].Details["kind"] != "secret" {
		t.Fatalf("move audit: %+v", es)
	}
	if got := e.resolve(org, authz.TeamSubject(ops)); got["DB_URL"] != "y" {
		t.Fatalf("resolve after move: %v", got)
	}
}

func TestEnvValidationAndConflicts(t *testing.T) {
	e := newEnv(t)
	_, ownerTok := e.user("alice")
	bob, bobTok := e.user("bob")
	org := e.createOrg(ownerTok, "Acme")
	e.addMember(org, bob, store.RoleMember)

	for _, body := range []string{
		`{"name":"lower","value":"x","kind":"env","scope":"org"}`,
		`{"name":"1ABC","value":"x","kind":"env","scope":"org"}`,
		`{"name":"` + strings.Repeat("A", 129) + `","value":"x","kind":"env","scope":"org"}`,
		`{"name":"A","value":"` + strings.Repeat("x", 65537) + `","kind":"env","scope":"org"}`,
		`{"name":"A","value":"x","kind":"plain","scope":"org"}`,
		`{"name":"A","value":"x","kind":"env","scope":"global"}`,
		`nope`,
	} {
		if rec := e.send("POST", envPath(org), ownerTok, body); rec.Code != 400 {
			t.Fatalf("%.60s: %d", body, rec.Code)
		}
	}
	e.createEnv(ownerTok, org, `{"name":"`+strings.Repeat("A", 128)+`","value":"`+strings.Repeat("x", 65536)+`","kind":"env","scope":"org"}`)

	id := e.createEnv(ownerTok, org, `{"name":"API_KEY","value":"a","kind":"env","scope":"org"}`)
	if rec := e.send("POST", envPath(org), ownerTok, `{"name":"API_KEY","value":"b","kind":"secret","scope":"org"}`); rec.Code != 409 {
		t.Fatalf("duplicate: %d", rec.Code)
	}
	mine := e.createEnv(bobTok, org, `{"name":"API_KEY","value":"c","kind":"secret","scope":"user"}`)
	e.createEnv(bobTok, org, `{"name":"OTHER","value":"d","kind":"secret","scope":"user"}`)
	if rec := e.send("PATCH", envVarPath(org, mine), bobTok, `{"name":"OTHER"}`); rec.Code != 409 {
		t.Fatalf("rename onto existing: %d", rec.Code)
	}
	if rec := e.send("PATCH", envVarPath(org, mine), bobTok, `{"name":"bad name"}`); rec.Code != 400 {
		t.Fatalf("invalid rename: %d", rec.Code)
	}
	if rec := e.send("PATCH", envVarPath(org, id), ownerTok, `{"value":"`+strings.Repeat("x", 65537)+`"}`); rec.Code != 400 {
		t.Fatalf("value too long: %d", rec.Code)
	}
	if rec := e.send("POST", envVarPath(org, mine)+"/move", bobTok, `{"scope":"org"}`); rec.Code != 403 {
		t.Fatalf("member moves private to org: %d", rec.Code)
	}
	if es := e.audit(store.ActionEnvSet); es[0].Result != store.ResultDenied || es[0].Resource != authz.ObjEnv {
		t.Fatalf("move denial audit: %+v", es[0])
	}
}

func TestEnvWritableScopes(t *testing.T) {
	e := newEnv(t)
	_, ownerTok := e.user("alice")
	bob, bobTok := e.user("bob")
	org := e.createOrg(ownerTok, "Acme")
	e.addMember(org, bob, store.RoleMember)
	a, b := e.createTeam(ownerTok, org, "a"), e.createTeam(ownerTok, org, "b")

	if got := e.listEnv(bobTok, org).WritableScopes; len(got) != 1 || got[0].Scope != "user" || got[0].TeamID != nil {
		t.Fatalf("member: %+v", got)
	}
	got := e.listEnv(ownerTok, org).WritableScopes
	if len(got) != 4 || got[0].Scope != "org" || *got[1].TeamID != a || got[1].TeamName != "a" || *got[2].TeamID != b || got[3].Scope != "user" {
		t.Fatalf("owner: %+v", got)
	}
}

const boundValue = "0123456789abcdef"

func secretBody(name string, hosts string) string {
	return `{"name":"` + name + `","value":"` + boundValue + `","kind":"secret","scope":"org","hosts":` + hosts + `}`
}

func TestEnvHostsAPI(t *testing.T) {
	e := newEnv(t)
	owner, ownerTok := e.user("alice")
	org := e.createOrg(ownerTok, "Acme")

	rec := e.send("POST", envPath(org), ownerTok, secretBody("GH_TOKEN", `["API.GitHub.com","github.com"]`))
	if rec.Code != 201 {
		t.Fatalf("create: %d %s", rec.Code, rec.Body.String())
	}
	list := e.do("GET", envPath(org), ownerTok)
	for _, b := range []string{rec.Body.String(), list.Body.String()} {
		if strings.Contains(b, "obsph_") || strings.Contains(b, boundValue) {
			t.Fatalf("placeholder or value in response: %s", b)
		}
	}
	var got envListJSON
	decode(t, list, &got)
	if len(got.Vars) != 1 || strings.Join(got.Vars[0].Hosts, ",") != "api.github.com,github.com" {
		t.Fatalf("list: %s", list.Body.String())
	}
	es := e.audit(store.ActionEnvSet)
	if len(es) != 1 || es[0].ActorID != owner.ID.String() || strings.Join(toStrings(es[0].Details["hosts"]), ",") != "api.github.com,github.com" {
		t.Fatalf("audit: %+v", es)
	}
	e.createEnv(ownerTok, org, `{"name":"PLAIN","value":"x","kind":"env","scope":"org"}`)
	var all envListJSON
	decode(t, e.do("GET", envPath(org), ownerTok), &all)
	for _, v := range all.Vars {
		if v.Name == "PLAIN" && (v.Hosts == nil || len(v.Hosts) != 0) {
			t.Fatalf("unbound hosts must be an empty array: %+v", v.Hosts)
		}
	}
}

func toStrings(v any) []string {
	var out []string
	for _, x := range v.([]any) {
		out = append(out, x.(string))
	}
	return out
}

func TestEnvHostsValidation(t *testing.T) {
	e := newEnv(t)
	_, ownerTok := e.user("alice")
	org := e.createOrg(ownerTok, "Acme")
	var many []string
	for i := 0; i < 11; i++ {
		many = append(many, `"h`+strings.Repeat("a", i)+`.test"`)
	}
	for name, body := range map[string]string{
		"wildcard": secretBody("A", `["*.github.com"]`),
		"ip":       secretBody("A", `["1.2.3.4"]`),
		"port":     secretBody("A", `["github.com:443"]`),
		"eleven":   secretBody("A", "["+strings.Join(many, ",")+"]"),
		"kind env": `{"name":"A","value":"` + boundValue + `","kind":"env","scope":"org","hosts":["github.com"]}`,
		"short":    `{"name":"A","value":"01234567890","kind":"secret","scope":"org","hosts":["github.com"]}`,
	} {
		if rec := e.send("POST", envPath(org), ownerTok, body); rec.Code != 400 {
			t.Fatalf("%s: %d %s", name, rec.Code, rec.Body.String())
		}
	}
	rec := e.send("POST", envPath(org), ownerTok, secretBody("A", `["github.com:443"]`))
	if !strings.Contains(rec.Body.String(), `invalid host: \"github.com:443\"`) {
		t.Fatalf("message: %s", rec.Body.String())
	}
	id := e.createEnv(ownerTok, org, `{"name":"B","value":"`+boundValue+`","kind":"secret","scope":"org"}`)
	if rec := e.send("PATCH", envVarPath(org, id), ownerTok, `{"hosts":["1.2.3.4"]}`); rec.Code != 400 {
		t.Fatalf("patch: %d", rec.Code)
	}
}

func TestEnvUnbind(t *testing.T) {
	e := newEnv(t)
	_, ownerTok := e.user("alice")
	org := e.createOrg(ownerTok, "Acme")
	id := e.createEnv(ownerTok, org, secretBody("GH_TOKEN", `["github.com"]`))

	if rec := e.send("PATCH", envVarPath(org, id), ownerTok, `{"name":"GH_TOKEN2"}`); rec.Code != 204 {
		t.Fatalf("rename: %d %s", rec.Code, rec.Body.String())
	}
	if got := e.listEnv(ownerTok, org); len(got.Vars[0].Hosts) != 1 {
		t.Fatalf("omitted hosts changed: %+v", got.Vars[0].Hosts)
	}
	if rec := e.send("PATCH", envVarPath(org, id), ownerTok, `{"hosts":[]}`); rec.Code != 204 {
		t.Fatalf("unbind: %d %s", rec.Code, rec.Body.String())
	}
	if got := e.listEnv(ownerTok, org); got.Vars[0].Hosts == nil || len(got.Vars[0].Hosts) != 0 {
		t.Fatalf("after unbind: %+v", got.Vars[0].Hosts)
	}
	es := e.audit(store.ActionEnvSet)
	if h, ok := es[0].Details["hosts"].([]any); !ok || len(h) != 0 {
		t.Fatalf("unbind audit: %+v", es[0].Details)
	}
}

func TestEnvStaleUpdateKeepsHosts(t *testing.T) {
	e := newEnv(t)
	_, ownerTok := e.user("alice")
	org := e.createOrg(ownerTok, "Acme")
	id := e.createEnv(ownerTok, org, `{"name":"GH_TOKEN","value":"`+boundValue+`","kind":"secret","scope":"org"}`)
	ctx := context.Background()
	vars, err := e.store.EnvVars(ctx, org)
	if err != nil {
		t.Fatal(err)
	}
	stale := vars[0]

	if rec := e.send("PATCH", envVarPath(org, id), ownerTok, `{"hosts":["github.com"]}`); rec.Code != 204 {
		t.Fatalf("bind: %d", rec.Code)
	}
	renamed := stale
	renamed.Name = "OTHER"
	if err := e.store.UpdateEnvVar(ctx, stale, renamed, nil); !errors.Is(err, store.ErrStale) {
		t.Fatalf("stale update: %v", err)
	}
	if got := e.listEnv(ownerTok, org); got.Vars[0].Name != "GH_TOKEN" || len(got.Vars[0].Hosts) != 1 {
		t.Fatalf("stale update changed the row: %+v", got.Vars[0])
	}
}

func TestEnvHostsPermission(t *testing.T) {
	e := newEnv(t)
	_, ownerTok := e.user("alice")
	bob, bobTok := e.user("bob")
	carol, carolTok := e.user("carol")
	org := e.createOrg(ownerTok, "Acme")
	e.addMember(org, bob, store.RoleMember)
	e.addMember(org, carol, store.RoleMember)
	team := e.createTeam(ownerTok, org, "core")
	teamVar := e.createEnv(ownerTok, org, `{"name":"T","value":"`+boundValue+`","kind":"secret","scope":"team","teamId":"`+team.String()+`"}`)

	mine := e.createEnv(bobTok, org, `{"name":"M","value":"`+boundValue+`","kind":"secret","scope":"user"}`)
	if rec := e.send("PATCH", envVarPath(org, mine), bobTok, `{"hosts":["github.com"]}`); rec.Code != 204 {
		t.Fatalf("bind own: %d %s", rec.Code, rec.Body.String())
	}
	if rec := e.send("PATCH", envVarPath(org, teamVar), carolTok, `{"hosts":["github.com"]}`); rec.Code != 403 {
		t.Fatalf("bind team var without env:set: %d", rec.Code)
	}

	role := e.createRole(ownerTok, org, `{"name":"nohosts","rules":[{"effect":"deny","object":"org/user-env/*","action":"env:set"}]}`)
	e.bind(ownerTok, org, authz.UserSubject(bob.ID), role)
	for _, body := range []string{`{"hosts":["example.com"]}`, `{"hosts":[]}`} {
		if rec := e.send("PATCH", envVarPath(org, mine), bobTok, body); rec.Code != 403 {
			t.Fatalf("%s with deny: %d", body, rec.Code)
		}
	}
}

func TestVMEgressInfo(t *testing.T) {
	e := newEnv(t)
	_, ownerTok := e.user("alice")
	bob, bobTok := e.user("bob")
	org := e.createOrg(ownerTok, "Acme")
	e.addMember(org, bob, store.RoleMember)
	node := e.enrolledNode(ownerTok, org, "n1")
	vm := e.createVM(ownerTok, org, vmBody("v", userOwner(), node))
	id := e.createEnv(ownerTok, org, `{"name":"GH_TOKEN","value":"`+boundValue+`","kind":"secret","scope":"user","hosts":["api.github.com"]}`)
	_ = id
	vmID := uuid.MustParse(vm.ID)
	ctx := context.Background()
	if _, err := e.store.EnsureEgressCAs(ctx, org, vmID, []string{"api.github.com"}, time.Now()); err != nil {
		t.Fatal(err)
	}
	path := vmsPath(org) + "/" + vm.ID + "/egress"
	type info struct {
		Secrets      int
		Hosts        []string
		CANotAfter   *time.Time
		TrustChanged bool
	}
	get := func() info {
		rec := e.do("GET", path, ownerTok)
		if rec.Code != 200 || strings.Contains(rec.Body.String(), "obsph_") {
			t.Fatalf("egress: %d %s", rec.Code, rec.Body.String())
		}
		var got info
		decode(t, rec, &got)
		return got
	}
	if got := get(); got.Secrets != 1 || strings.Join(got.Hosts, ",") != "api.github.com" || got.CANotAfter == nil || got.TrustChanged {
		t.Fatalf("initial: %+v", got)
	}
	if rec := e.send("PATCH", envVarPath(org, id), ownerTok, `{"hosts":["api.github.com","github.com"]}`); rec.Code != 204 {
		t.Fatalf("bind second: %d", rec.Code)
	}
	if _, err := e.store.EnsureEgressCAs(ctx, org, vmID, []string{"api.github.com", "github.com"}, time.Now()); err != nil {
		t.Fatal(err)
	}
	if got := get(); !got.TrustChanged || len(got.Hosts) != 2 {
		t.Fatalf("after bind: %+v", got)
	}
	if rec := e.do("GET", path, bobTok); rec.Code != 403 {
		t.Fatalf("no vm:read: %d", rec.Code)
	}
}

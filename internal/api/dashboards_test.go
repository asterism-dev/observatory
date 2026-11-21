package api

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/asterism-dev/observatory/internal/store"
)

type dashboardTest struct {
	ID        uuid.UUID
	Name      string
	Scope     string
	OwnerID   *uuid.UUID
	Panels    []map[string]any
	CreatedBy uuid.UUID
	CreatedAt time.Time
	UpdatedAt string
	CanEdit   bool
}

const testPanels = `[{"title":"CPU","type":"timeseries","query":"rate(system_cpu_time_seconds_total[5m])","unit":"percent","width":"half"}]`

func dashboardsPath(org uuid.UUID) string { return "/api/orgs/" + org.String() + "/dashboards" }

func dashboardPath(org, id uuid.UUID) string { return dashboardsPath(org) + "/" + id.String() }

func (e *env) createDashboard(tok string, org uuid.UUID, body string) dashboardTest {
	e.t.Helper()
	rec := e.send("POST", dashboardsPath(org), tok, body)
	if rec.Code != 201 {
		e.t.Fatalf("create dashboard %s: %d %s", body, rec.Code, rec.Body.String())
	}
	var d dashboardTest
	decode(e.t, rec, &d)
	return d
}

func (e *env) listDashboards(tok string, org uuid.UUID) []dashboardTest {
	e.t.Helper()
	rec := e.do("GET", dashboardsPath(org), tok)
	if rec.Code != 200 {
		e.t.Fatalf("list dashboards: %d %s", rec.Code, rec.Body.String())
	}
	var got []dashboardTest
	decode(e.t, rec, &got)
	return got
}

func (e *env) getDashboard(tok string, org, id uuid.UUID) (dashboardTest, int) {
	e.t.Helper()
	rec := e.do("GET", dashboardPath(org, id), tok)
	var d dashboardTest
	if rec.Code == 200 {
		decode(e.t, rec, &d)
	}
	return d, rec.Code
}

// putDashboard renames d, keeping its scope and panels, with the updatedAt the caller last saw.
func (e *env) putDashboard(tok string, org uuid.UUID, d dashboardTest, name string) (dashboardTest, int) {
	e.t.Helper()
	body := map[string]any{"name": name, "scope": d.Scope, "panels": d.Panels, "updatedAt": d.UpdatedAt}
	if d.Scope == "team" {
		body["teamId"] = d.OwnerID
	}
	raw, _ := json.Marshal(body)
	rec := e.send("PUT", dashboardPath(org, d.ID), tok, string(raw))
	var got dashboardTest
	if rec.Code == 200 {
		decode(e.t, rec, &got)
	}
	return got, rec.Code
}

func (e *env) setTeamMember(ownerTok string, org, team uuid.UUID, u store.User) {
	e.t.Helper()
	if rec := e.send("PUT", teamMemberPath(org, team, u), ownerTok, `{"admin":false}`); rec.Code != 204 {
		e.t.Fatalf("add team member: %d %s", rec.Code, rec.Body.String())
	}
}

func listed(ds []dashboardTest, id uuid.UUID) *dashboardTest {
	for i := range ds {
		if ds[i].ID == id {
			return &ds[i]
		}
	}
	return nil
}

func TestPrivateDashboardHiddenFromAdmin(t *testing.T) {
	e := newEnv(t)
	_, ownerTok := e.user("alice")
	bob, bobTok := e.user("bob")
	org := e.createOrg(ownerTok, "Acme")
	e.addMember(org, bob, store.RoleMember)

	d := e.createDashboard(bobTok, org, `{"name":"Mine","scope":"user","panels":`+testPanels+`}`)
	if d.Scope != "user" || d.OwnerID == nil || *d.OwnerID != bob.ID || d.CreatedBy != bob.ID || !d.CanEdit || len(d.Panels) != 1 {
		t.Fatalf("created: %+v", d)
	}
	if got := listed(e.listDashboards(bobTok, org), d.ID); got == nil || !got.CanEdit {
		t.Fatalf("bob list: %+v", got)
	}
	if got := e.listDashboards(ownerTok, org); len(got) != 0 {
		t.Fatalf("owner sees private dashboard: %+v", got)
	}
	if _, code := e.getDashboard(ownerTok, org, d.ID); code != 404 {
		t.Fatalf("owner get: %d", code)
	}
	if _, code := e.putDashboard(ownerTok, org, d, "Taken"); code != 404 {
		t.Fatalf("owner put: %d", code)
	}
	if rec := e.do("DELETE", dashboardPath(org, d.ID), ownerTok); rec.Code != 404 {
		t.Fatalf("owner delete: %d", rec.Code)
	}
	if rec := e.send("POST", dashboardsPath(org), ownerTok, `{"name":"X","scope":"user","panels":[]}`); rec.Code != 201 {
		t.Fatalf("owner own private: %d", rec.Code)
	}
	if got, code := e.getDashboard(bobTok, org, d.ID); code != 200 || got.Name != "Mine" {
		t.Fatalf("bob get: %d %+v", code, got)
	}
}

func TestTeamDashboardMemberRead(t *testing.T) {
	e := newEnv(t)
	_, ownerTok := e.user("alice")
	bob, bobTok := e.user("bob")
	carol, carolTok := e.user("carol")
	dave, daveTok := e.user("dave")
	org := e.createOrg(ownerTok, "Acme")
	for _, u := range []store.User{bob, carol, dave} {
		e.addMember(org, u, store.RoleMember)
	}
	team := e.createTeam(ownerTok, org, "core")
	e.setTeamMember(ownerTok, org, team, bob)
	e.setTeamMember(ownerTok, org, team, carol)

	d := e.createDashboard(bobTok, org, `{"name":"Shop","scope":"team","teamId":"`+team.String()+`","panels":`+testPanels+`}`)
	if d.Scope != "team" || *d.OwnerID != team || !d.CanEdit {
		t.Fatalf("created: %+v", d)
	}
	got := listed(e.listDashboards(carolTok, org), d.ID)
	if got == nil || got.CanEdit || got.Name != "Shop" {
		t.Fatalf("carol list: %+v", got)
	}
	if _, code := e.getDashboard(carolTok, org, d.ID); code != 200 {
		t.Fatalf("carol get: %d", code)
	}
	if _, code := e.putDashboard(carolTok, org, d, "Hijack"); code != 403 {
		t.Fatalf("carol put: %d", code)
	}
	if rec := e.do("DELETE", dashboardPath(org, d.ID), carolTok); rec.Code != 403 {
		t.Fatalf("carol delete: %d", rec.Code)
	}
	if got := e.listDashboards(daveTok, org); len(got) != 0 {
		t.Fatalf("dave sees team dashboard: %+v", got)
	}
	if _, code := e.getDashboard(daveTok, org, d.ID); code != 403 {
		t.Fatalf("dave get: %d", code)
	}
	if got := listed(e.listDashboards(ownerTok, org), d.ID); got == nil || !got.CanEdit {
		t.Fatalf("owner list: %+v", got)
	}
}

func TestTeamDashboardCreatorEdit(t *testing.T) {
	e := newEnv(t)
	owner, ownerTok := e.user("alice")
	bob, bobTok := e.user("bob")
	carol, carolTok := e.user("carol")
	org := e.createOrg(ownerTok, "Acme")
	e.addMember(org, bob, store.RoleMember)
	e.addMember(org, carol, store.RoleMember)
	team := e.createTeam(ownerTok, org, "core")
	e.setTeamMember(ownerTok, org, team, bob)
	e.makeTeamAdmin(ownerTok, org, team, carol)

	d := e.createDashboard(bobTok, org, `{"name":"Shop","scope":"team","teamId":"`+team.String()+`","panels":[]}`)
	d, code := e.putDashboard(bobTok, org, d, "Shop 2")
	if code != 200 || d.Name != "Shop 2" || !d.CanEdit {
		t.Fatalf("creator edit: %d %+v", code, d)
	}
	if d, code = e.putDashboard(carolTok, org, d, "Shop 3"); code != 200 {
		t.Fatalf("team admin edit: %d", code)
	}
	if d, code = e.putDashboard(ownerTok, org, d, "Shop 4"); code != 200 {
		t.Fatalf("org owner edit: %d", code)
	}
	if es := e.audit(store.ActionDashboardUpdate); len(es) != 3 || es[0].ActorID != owner.ID.String() || es[0].Result != store.ResultAllowed || es[0].Details["name"] != "Shop 4" {
		t.Fatalf("update audit: %+v", es)
	}

	if rec := e.do("DELETE", teamMemberPath(org, team, bob), ownerTok); rec.Code != 204 {
		t.Fatalf("remove bob: %d", rec.Code)
	}
	if _, code := e.putDashboard(bobTok, org, d, "Mine again"); code != 403 {
		t.Fatalf("ex-member creator edit: %d", code)
	}
	if rec := e.do("DELETE", dashboardPath(org, d.ID), bobTok); rec.Code != 403 {
		t.Fatalf("ex-member creator delete: %d", rec.Code)
	}
	if rec := e.do("DELETE", dashboardPath(org, d.ID), carolTok); rec.Code != 204 {
		t.Fatalf("team admin delete: %d %s", rec.Code, rec.Body.String())
	}
	if es := e.audit(store.ActionDashboardDelete); len(es) != 1 || es[0].ActorID != carol.ID.String() {
		t.Fatalf("delete audit: %+v", es)
	}
	if es := e.audit(store.ActionDashboardCreate); len(es) != 1 || es[0].ActorID != bob.ID.String() || es[0].Resource != "org/team-dashboards/"+team.String() {
		t.Fatalf("create audit: %+v", es)
	}
}

func TestOrgDashboardMemberCannotCreate(t *testing.T) {
	e := newEnv(t)
	_, ownerTok := e.user("alice")
	bob, bobTok := e.user("bob")
	carol, carolTok := e.user("carol")
	org := e.createOrg(ownerTok, "Acme")
	e.addMember(org, bob, store.RoleAdmin)
	e.addMember(org, carol, store.RoleMember)

	if rec := e.send("POST", dashboardsPath(org), carolTok, `{"name":"All","scope":"org","panels":[]}`); rec.Code != 403 {
		t.Fatalf("member create org: %d", rec.Code)
	}
	d := e.createDashboard(bobTok, org, `{"name":"All","scope":"org","panels":[]}`)
	got := listed(e.listDashboards(carolTok, org), d.ID)
	if got == nil || got.CanEdit {
		t.Fatalf("carol list: %+v", got)
	}
	if _, code := e.putDashboard(carolTok, org, d, "Mine"); code != 403 {
		t.Fatalf("member edit org: %d", code)
	}
	// The creator keeps edit rights on an org dashboard after losing the admin role.
	if rec := e.send("PATCH", memberPath(org, bob), ownerTok, `{"role":"member"}`); rec.Code != 204 {
		t.Fatalf("demote: %d %s", rec.Code, rec.Body.String())
	}
	if got, code := e.putDashboard(bobTok, org, d, "All 2"); code != 200 || !got.CanEdit {
		t.Fatalf("creator edit after demotion: %d %+v", code, got)
	}
}

func TestDashboardShareNeedsTargetCreate(t *testing.T) {
	e := newEnv(t)
	_, ownerTok := e.user("alice")
	bob, bobTok := e.user("bob")
	org := e.createOrg(ownerTok, "Acme")
	other := e.createOrg(ownerTok, "Other")
	e.addMember(org, bob, store.RoleMember)
	team := e.createTeam(ownerTok, org, "core")
	foreign := e.createTeam(ownerTok, org, "ops")
	otherTeam := e.createTeam(ownerTok, other, "x")
	e.setTeamMember(ownerTok, org, team, bob)

	d := e.createDashboard(bobTok, org, `{"name":"Mine","scope":"user","panels":[]}`)
	move := func(target string) int {
		rec := e.send("PUT", dashboardPath(org, d.ID), bobTok, `{"name":"Mine","panels":[],"updatedAt":"`+d.UpdatedAt+`",`+target+`}`)
		if rec.Code == 200 {
			decode(t, rec, &d)
		}
		return rec.Code
	}
	if code := move(`"scope":"org"`); code != 403 {
		t.Fatalf("share to org: %d", code)
	}
	if code := move(`"scope":"team","teamId":"` + foreign.String() + `"`); code != 403 {
		t.Fatalf("share to foreign team: %d", code)
	}
	for _, bad := range []string{`"scope":"team","teamId":"` + otherTeam.String() + `"`, `"scope":"team"`, `"scope":"nope"`} {
		if code := move(bad); code != 400 {
			t.Fatalf("share to %s: %d", bad, code)
		}
	}
	if rec := e.send("POST", dashboardsPath(org), bobTok, `{"name":"X","scope":"team","teamId":"`+otherTeam.String()+`","panels":[]}`); rec.Code != 400 {
		t.Fatalf("create in other org's team: %d", rec.Code)
	}
	if code := move(`"scope":"team","teamId":"` + team.String() + `"`); code != 200 || d.Scope != "team" || *d.OwnerID != team || !d.CanEdit {
		t.Fatalf("share to own team: %d %+v", code, d)
	}
	if got := listed(e.listDashboards(ownerTok, org), d.ID); got == nil {
		t.Fatal("org owner doesn't see the shared dashboard")
	}
}

func TestDashboardStaleUpdate(t *testing.T) {
	e := newEnv(t)
	_, ownerTok := e.user("alice")
	org := e.createOrg(ownerTok, "Acme")
	d := e.createDashboard(ownerTok, org, `{"name":"A","scope":"org","panels":[]}`)

	first, code := e.putDashboard(ownerTok, org, d, "B")
	if code != 200 || first.UpdatedAt == d.UpdatedAt {
		t.Fatalf("first save: %d %+v", code, first)
	}
	if _, code := e.putDashboard(ownerTok, org, d, "C"); code != 409 {
		t.Fatalf("stale save: %d", code)
	}
	if got, _ := e.getDashboard(ownerTok, org, d.ID); got.Name != "B" || got.UpdatedAt != first.UpdatedAt {
		t.Fatalf("lost update: %+v", got)
	}
	if rec := e.send("PUT", dashboardPath(org, d.ID), ownerTok, `{"name":"D","scope":"org","panels":[]}`); rec.Code != 400 {
		t.Fatalf("missing updatedAt: %d", rec.Code)
	}
	if _, code := e.putDashboard(ownerTok, org, first, "E"); code != 200 {
		t.Fatalf("fresh save: %d", code)
	}
}

func TestDashboardPanelValidation(t *testing.T) {
	e := newEnv(t)
	_, ownerTok := e.user("alice")
	org := e.createOrg(ownerTok, "Acme")
	panel := func(override string) string {
		return `{"title":"T","type":"stat","query":"up","unit":"none","width":"full"` + override + `}`
	}
	many := strings.TrimSuffix(strings.Repeat(panel("")+",", 21), ",")
	cases := []struct{ name, panels, field string }{
		{"A", `[` + panel(`,"type":"pie"`) + `]`, "panels[0].type"},
		{"A", `[` + panel("") + `,` + panel(`,"unit":"furlongs"`) + `]`, "panels[1].unit"},
		{"A", `[` + panel(`,"width":"third"`) + `]`, "panels[0].width"},
		{"A", `[` + panel(`,"query":"  "`) + `]`, "panels[0].query"},
		{"A", `[` + panel(`,"query":"`+strings.Repeat("x", 4097)+`"`) + `]`, "panels[0].query"},
		{"A", `[` + panel(`,"title":"`+strings.Repeat("x", 101)+`"`) + `]`, "panels[0].title"},
		{"A", `[` + many + `]`, "panels"},
		{"", `[]`, "name"},
		{strings.Repeat("n", 101), `[]`, "name"},
	}
	for _, c := range cases {
		rec := e.send("POST", dashboardsPath(org), ownerTok, `{"name":"`+c.name+`","scope":"org","panels":`+c.panels+`}`)
		var got struct{ Error, Field string }
		decode(t, rec, &got)
		if rec.Code != 400 || got.Field != c.field || got.Error == "" {
			t.Errorf("%s: %d %s", c.field, rec.Code, rec.Body.String())
		}
	}
	twenty := strings.TrimSuffix(strings.Repeat(panel(`,"extra":"dropped"`)+",", 20), ",")
	d := e.createDashboard(ownerTok, org, `{"name":"`+strings.Repeat("n", 100)+`","scope":"org","panels":[`+twenty+`]}`)
	if len(d.Panels) != 20 || d.Panels[0]["extra"] != nil || d.Panels[0]["query"] != "up" {
		t.Fatalf("stored panels: %+v", d.Panels)
	}
}

func TestDashboardLimit(t *testing.T) {
	e := newEnv(t)
	owner, ownerTok := e.user("alice")
	org := e.createOrg(ownerTok, "Acme")
	for i := range 50 {
		if _, err := e.store.CreateDashboard(context.Background(), store.Dashboard{OrgID: org, Scope: store.EnvScopeOrg, Name: "d" + string(rune('a'+i%26)), CreatedBy: owner.ID}); err != nil {
			t.Fatal(err)
		}
	}
	if rec := e.send("POST", dashboardsPath(org), ownerTok, `{"name":"one too many","scope":"user","panels":[]}`); rec.Code != 409 {
		t.Fatalf("51st dashboard: %d %s", rec.Code, rec.Body.String())
	}
}

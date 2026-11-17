package api

import (
	"testing"

	"github.com/google/uuid"

	"github.com/asterism-dev/observatory/internal/store"
)

func (e *env) createTeam(tok string, org uuid.UUID, name string) uuid.UUID {
	e.t.Helper()
	rec := e.send("POST", "/api/orgs/"+org.String()+"/teams", tok, `{"name":"`+name+`"}`)
	if rec.Code != 201 {
		e.t.Fatalf("create team: %d %s", rec.Code, rec.Body.String())
	}
	var got struct{ ID uuid.UUID }
	decode(e.t, rec, &got)
	return got.ID
}

func teamPath(org, team uuid.UUID) string {
	return "/api/orgs/" + org.String() + "/teams/" + team.String()
}

func teamMemberPath(org, team uuid.UUID, u store.User) string {
	return teamPath(org, team) + "/members/" + u.ID.String()
}

func TestTeamCRUD(t *testing.T) {
	e := newEnv(t)
	owner, ownerTok := e.user("alice")
	member, memberTok := e.user("bob")
	org := e.createOrg(ownerTok, "Acme")
	e.addMember(org, member, store.RoleMember)
	base := "/api/orgs/" + org.String() + "/teams"

	for _, body := range []string{`{"name":""}`, `{"name":"a\u0000b"}`, `nope`} {
		if rec := e.send("POST", base, ownerTok, body); rec.Code != 400 {
			t.Fatalf("%s: %d", body, rec.Code)
		}
	}
	if rec := e.send("POST", base, memberTok, `{"name":"x"}`); rec.Code != 403 {
		t.Fatalf("member create: %d", rec.Code)
	}
	if es := e.audit(store.ActionTeamCreate); len(es) != 1 || es[0].Result != store.ResultDenied || es[0].ActorID != member.ID.String() {
		t.Fatalf("denied audit: %+v", es)
	}
	id := e.createTeam(ownerTok, org, "  core ")
	if rec := e.send("POST", base, ownerTok, `{"name":"core"}`); rec.Code != 409 {
		t.Fatalf("duplicate: %d", rec.Code)
	}
	if es := e.audit(store.ActionTeamCreate); len(es) != 2 || es[0].Resource != "org/teams/"+id.String() || es[0].Result != store.ResultAllowed || es[0].ActorID != owner.ID.String() {
		t.Fatalf("create audit: %+v", es)
	}

	var list []struct {
		ID          uuid.UUID
		Name        string
		MemberCount int
	}
	rec := e.do("GET", base, memberTok)
	decode(t, rec, &list)
	if rec.Code != 200 || len(list) != 1 || list[0].Name != "core" || list[0].MemberCount != 0 {
		t.Fatalf("list: %d %s", rec.Code, rec.Body.String())
	}

	var detail struct {
		Name    string
		Members []struct {
			ID, Email, Name string
			Admin           bool
		}
		CanManageMembers, CanUpdate, CanDelete bool
	}
	rec = e.do("GET", teamPath(org, id), memberTok)
	decode(t, rec, &detail)
	if rec.Code != 200 || detail.Name != "core" || detail.Members == nil || len(detail.Members) != 0 || detail.CanManageMembers || detail.CanUpdate || detail.CanDelete {
		t.Fatalf("member detail: %d %s", rec.Code, rec.Body.String())
	}
	rec = e.do("GET", teamPath(org, id), ownerTok)
	decode(t, rec, &detail)
	if !detail.CanManageMembers || !detail.CanUpdate || !detail.CanDelete {
		t.Fatalf("owner detail: %s", rec.Body.String())
	}

	if rec := e.send("PATCH", teamPath(org, id), memberTok, `{"name":"x"}`); rec.Code != 403 {
		t.Fatalf("member rename: %d", rec.Code)
	}
	if rec := e.send("PATCH", teamPath(org, id), ownerTok, `{"name":"platform"}`); rec.Code != 204 {
		t.Fatalf("rename: %d %s", rec.Code, rec.Body.String())
	}
	if es := e.audit(store.ActionTeamUpdate); len(es) != 2 || es[0].Result != store.ResultAllowed || es[0].Details["name"] != "platform" {
		t.Fatalf("update audit: %+v", es)
	}
	if rec := e.send("PATCH", teamPath(org, uuid.New()), ownerTok, `{"name":"x"}`); rec.Code != 404 {
		t.Fatalf("unknown: %d", rec.Code)
	}
	if rec := e.do("GET", base+"/"+id.String()+"x", ownerTok); rec.Code != 404 {
		t.Fatalf("non-canonical id: %d", rec.Code)
	}
	if rec := e.do("DELETE", teamPath(org, id), memberTok); rec.Code != 403 {
		t.Fatalf("member delete: %d", rec.Code)
	}
	if rec := e.do("DELETE", teamPath(org, id), ownerTok); rec.Code != 204 {
		t.Fatalf("delete: %d", rec.Code)
	}
	if es := e.audit(store.ActionTeamDelete); len(es) != 2 || es[0].Result != store.ResultAllowed || es[0].Resource != "org/teams/"+id.String() {
		t.Fatalf("delete audit: %+v", es)
	}
	if rec := e.do("GET", teamPath(org, id), ownerTok); rec.Code != 404 {
		t.Fatalf("after delete: %d", rec.Code)
	}
}

func TestTeamAdminManagesMembers(t *testing.T) {
	e := newEnv(t)
	_, ownerTok := e.user("alice")
	lead, leadTok := e.user("bob")
	peer, _ := e.user("carol")
	org := e.createOrg(ownerTok, "Acme")
	e.addMember(org, lead, store.RoleMember)
	e.addMember(org, peer, store.RoleMember)
	mine, other := e.createTeam(ownerTok, org, "mine"), e.createTeam(ownerTok, org, "other")

	if rec := e.send("PUT", teamMemberPath(org, mine, lead), leadTok, `{"admin":false}`); rec.Code != 403 {
		t.Fatalf("plain member: %d", rec.Code)
	}
	if rec := e.send("PUT", teamMemberPath(org, mine, lead), ownerTok, `{"admin":true}`); rec.Code != 204 {
		t.Fatalf("owner makes admin: %d", rec.Code)
	}
	if rec := e.send("PUT", teamMemberPath(org, mine, peer), leadTok, `{"admin":false}`); rec.Code != 204 {
		t.Fatalf("team admin adds: %d %s", rec.Code, rec.Body.String())
	}
	if es := e.audit(store.ActionTeamMemberSet); len(es) != 2 || es[0].Resource != "org/teams/"+mine.String()+"/members/"+peer.ID.String() || es[0].Details["admin"] != false {
		t.Fatalf("set audit: %+v", es)
	}
	if rec := e.send("PUT", teamMemberPath(org, mine, peer), leadTok, `{"admin":true}`); rec.Code != 403 {
		t.Fatalf("team admin promotes: %d", rec.Code)
	}
	if rec := e.send("PUT", teamMemberPath(org, other, peer), leadTok, `{"admin":false}`); rec.Code != 403 {
		t.Fatalf("other team: %d", rec.Code)
	}
	if rec := e.do("DELETE", teamMemberPath(org, other, peer), leadTok); rec.Code != 403 {
		t.Fatalf("remove from other team: %d", rec.Code)
	}
	var detail struct {
		Members []struct {
			ID    string
			Admin bool
		}
		CanManageMembers bool
	}
	rec := e.do("GET", teamPath(org, mine), leadTok)
	decode(t, rec, &detail)
	if len(detail.Members) != 2 || !detail.CanManageMembers {
		t.Fatalf("detail: %s", rec.Body.String())
	}
	if rec := e.do("DELETE", teamMemberPath(org, mine, peer), leadTok); rec.Code != 204 {
		t.Fatalf("team admin removes: %d", rec.Code)
	}
	if rec := e.do("DELETE", teamMemberPath(org, mine, peer), leadTok); rec.Code != 404 {
		t.Fatalf("remove twice: %d", rec.Code)
	}
	if es := e.audit(store.ActionTeamMemberRemove); len(es) != 1 || es[0].Result != store.ResultAllowed {
		t.Fatalf("remove audit: %+v", es)
	}
	denied := 0
	for _, a := range []string{store.ActionTeamMemberSet, store.ActionTeamUpdate} {
		for _, en := range e.audit(a) {
			if en.Result == store.ResultDenied {
				denied++
			}
		}
	}
	if denied == 0 {
		t.Fatal("no denied audit rows")
	}
}

func TestTeamMemberMustBeOrgMember(t *testing.T) {
	e := newEnv(t)
	_, ownerTok := e.user("alice")
	stranger, _ := e.user("bob")
	org := e.createOrg(ownerTok, "Acme")
	team := e.createTeam(ownerTok, org, "core")
	if rec := e.send("PUT", teamMemberPath(org, team, stranger), ownerTok, `{"admin":false}`); rec.Code != 400 {
		t.Fatalf("non-member: %d %s", rec.Code, rec.Body.String())
	}
	if rec := e.send("PUT", teamMemberPath(org, team, stranger)+"x", ownerTok, `{"admin":false}`); rec.Code != 404 {
		t.Fatalf("non-canonical: %d", rec.Code)
	}
	if got, _ := e.store.Team(t.Context(), org, team); got.MemberCount != 0 {
		t.Fatalf("members: %+v", got)
	}
}

func TestTeamCrossOrgIDs(t *testing.T) {
	e := newEnv(t)
	alice, aTok := e.user("alice")
	_, bTok := e.user("bob")
	orgA, orgB := e.createOrg(aTok, "A"), e.createOrg(bTok, "B")
	teamB := e.createTeam(bTok, orgB, "core")
	for _, c := range [][2]string{
		{"GET", ""}, {"PATCH", `{"name":"x"}`}, {"DELETE", ""},
	} {
		if rec := e.send(c[0], teamPath(orgA, teamB), aTok, c[1]); rec.Code != 404 {
			t.Fatalf("%s: %d", c[0], rec.Code)
		}
	}
	owner := alice.ID
	if rec := e.send("PUT", teamPath(orgA, teamB)+"/members/"+owner.String(), aTok, `{"admin":false}`); rec.Code != 404 {
		t.Fatalf("put: %d", rec.Code)
	}
	if rec := e.do("DELETE", teamPath(orgA, teamB)+"/members/"+owner.String(), aTok); rec.Code != 404 {
		t.Fatalf("delete member: %d", rec.Code)
	}
	if rec := e.do("GET", teamPath(orgB, teamB), aTok); rec.Code != 404 {
		t.Fatalf("non-member of B: %d", rec.Code)
	}
	if got, err := e.store.Team(t.Context(), orgB, teamB); err != nil || got.Name != "core" || got.MemberCount != 0 {
		t.Fatalf("B changed: %+v %v", got, err)
	}
}

func TestDeleteTeamRemovesBindings(t *testing.T) {
	e := newEnv(t)
	_, ownerTok := e.user("alice")
	org := e.createOrg(ownerTok, "Acme")
	team := e.createTeam(ownerTok, org, "core")
	keep := e.createTeam(ownerTok, org, "keep")
	var role uuid.UUID
	if err := e.pool.QueryRow(t.Context(), `INSERT INTO roles (org_id, name) VALUES ($1, 'r') RETURNING id`, org).Scan(&role); err != nil {
		t.Fatal(err)
	}
	for _, id := range []uuid.UUID{team, keep} {
		e.exec(`INSERT INTO role_bindings (org_id, subject, role_id) VALUES ($1, $2, $3)`, org, "team:"+id.String(), role)
	}
	if rec := e.do("DELETE", teamPath(org, team), ownerTok); rec.Code != 204 {
		t.Fatalf("delete: %d", rec.Code)
	}
	var subjects []string
	rows, err := e.pool.Query(t.Context(), `SELECT subject FROM role_bindings WHERE org_id = $1`, org)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			t.Fatal(err)
		}
		subjects = append(subjects, s)
	}
	if len(subjects) != 1 || subjects[0] != "team:"+keep.String() {
		t.Fatalf("bindings: %v", subjects)
	}
}

func TestTeamAdminCannotChangePeerAdmin(t *testing.T) {
	e := newEnv(t)
	_, ownerTok := e.user("alice")
	lead, leadTok := e.user("bob")
	peer, _ := e.user("carol")
	boss, bossTok := e.user("dave")
	org := e.createOrg(ownerTok, "Acme")
	for _, u := range []store.User{lead, peer} {
		e.addMember(org, u, store.RoleMember)
	}
	e.addMember(org, boss, store.RoleAdmin)
	team := e.createTeam(ownerTok, org, "core")
	for _, u := range []store.User{lead, peer} {
		if rec := e.send("PUT", teamMemberPath(org, team, u), ownerTok, `{"admin":true}`); rec.Code != 204 {
			t.Fatalf("make admin: %d", rec.Code)
		}
	}
	before := len(e.audit(store.ActionTeamUpdate))
	if rec := e.send("PUT", teamMemberPath(org, team, peer), leadTok, `{"admin":false}`); rec.Code != 403 {
		t.Fatalf("demote: %d", rec.Code)
	}
	if rec := e.do("DELETE", teamMemberPath(org, team, peer), leadTok); rec.Code != 403 {
		t.Fatalf("remove: %d", rec.Code)
	}
	es := e.audit(store.ActionTeamUpdate)
	if len(es) != before+2 || es[0].Result != store.ResultDenied || es[0].ActorID != lead.ID.String() || es[0].Resource != "org/teams/"+team.String() {
		t.Fatalf("denied audit: %+v", es)
	}
	if ms, _ := e.store.TeamMembers(t.Context(), org, team); len(ms) != 2 || !ms[0].Admin || !ms[1].Admin {
		t.Fatalf("members changed: %+v", ms)
	}
	if rec := e.send("PUT", teamMemberPath(org, team, peer), bossTok, `{}`); rec.Code != 400 {
		t.Fatalf("missing admin: %d", rec.Code)
	}
	if rec := e.send("PUT", teamMemberPath(org, team, peer), bossTok, `{"admin":false}`); rec.Code != 204 {
		t.Fatalf("org admin demotes: %d", rec.Code)
	}
	if rec := e.do("DELETE", teamMemberPath(org, team, lead), bossTok); rec.Code != 204 {
		t.Fatalf("org admin removes: %d", rec.Code)
	}
}

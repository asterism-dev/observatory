package iam

import (
	"context"
	"slices"
	"strings"
	"testing"

	"connectrpc.com/connect"
	"github.com/google/uuid"

	iamv1 "github.com/asterism-dev/observatory/gen/go/observatory/iam/v1"
	"github.com/asterism-dev/observatory/internal/authz"
	"github.com/asterism-dev/observatory/internal/store"
)

func (e *env) createTeam(jwt string, org uuid.UUID, name string) uuid.UUID {
	e.t.Helper()
	res, err := e.client.CreateTeam(context.Background(), req(jwt, &iamv1.CreateTeamRequest{Org: org.String(), Name: name}))
	if err != nil {
		e.t.Fatalf("create team: %v", err)
	}
	return uuid.MustParse(res.Msg.Team.Id)
}

func (e *env) getTeam(jwt string, org, team uuid.UUID) (*iamv1.GetTeamResponse, error) {
	res, err := e.client.GetTeam(context.Background(), req(jwt, &iamv1.GetTeamRequest{Org: org.String(), Team: team.String()}))
	if err != nil {
		return nil, err
	}
	return res.Msg, nil
}

func (e *env) renameTeam(jwt string, org, team uuid.UUID, name string) error {
	_, err := e.client.UpdateTeam(context.Background(), req(jwt, &iamv1.UpdateTeamRequest{Org: org.String(), Team: team.String(), Name: name}))
	return err
}

func (e *env) deleteTeam(jwt string, org, team uuid.UUID) error {
	_, err := e.client.DeleteTeam(context.Background(), req(jwt, &iamv1.DeleteTeamRequest{Org: org.String(), Team: team.String()}))
	return err
}

func (e *env) setTeamMember(jwt string, org, team uuid.UUID, u store.User, admin bool) error {
	_, err := e.client.SetTeamMember(context.Background(), req(jwt, &iamv1.SetTeamMemberRequest{Org: org.String(), Team: team.String(), User: u.ID.String(), Admin: &admin}))
	return err
}

func (e *env) removeTeamMember(jwt string, org, team uuid.UUID, u store.User) error {
	_, err := e.client.RemoveTeamMember(context.Background(), req(jwt, &iamv1.RemoveTeamMemberRequest{Org: org.String(), Team: team.String(), User: u.ID.String()}))
	return err
}

func (e *env) leaveTeam(jwt string, org, team uuid.UUID) error {
	_, err := e.client.LeaveTeam(context.Background(), req(jwt, &iamv1.LeaveTeamRequest{Org: org.String(), Team: team.String()}))
	return err
}

func TestTeamCRUD(t *testing.T) {
	e := newEnv(t)
	owner, ownerTok := e.user("alice")
	member, memberTok := e.user("bob")
	org := e.createOrg(ownerTok, "Acme")
	e.addMember(org, member, store.RoleMember)
	ctx := context.Background()

	for _, name := range []string{"", "a\x00b"} {
		_, err := e.client.CreateTeam(ctx, req(ownerTok, &iamv1.CreateTeamRequest{Org: org.String(), Name: name}))
		wantCode(t, err, connect.CodeInvalidArgument)
	}
	_, err := e.client.CreateTeam(ctx, req(memberTok, &iamv1.CreateTeamRequest{Org: org.String(), Name: "x"}))
	wantCode(t, err, connect.CodePermissionDenied)
	if es := e.audit(store.ActionTeamCreate); len(es) != 1 || es[0].Result != store.ResultDenied || es[0].ActorID != member.ID.String() {
		t.Fatalf("denied audit: %+v", es)
	}
	id := e.createTeam(ownerTok, org, "  core ")
	_, err = e.client.CreateTeam(ctx, req(ownerTok, &iamv1.CreateTeamRequest{Org: org.String(), Name: "core"}))
	wantCode(t, err, connect.CodeFailedPrecondition)
	if es := e.audit(store.ActionTeamCreate); len(es) != 2 || es[0].Resource != "org/teams/"+id.String() || es[0].Result != store.ResultAllowed || es[0].ActorID != owner.ID.String() || es[0].Details["name"] != "core" {
		t.Fatalf("create audit: %+v", es)
	}

	list := must[iamv1.IAMServiceListTeamsResponse](t)(e.client.ListTeams(ctx, req(memberTok, &iamv1.IAMServiceListTeamsRequest{Org: org.String()}))).Teams
	if len(list) != 1 || list[0].Name != "core" || list[0].MemberCount != 0 || list[0].Id != id.String() || list[0].CreatedAt == nil {
		t.Fatalf("list: %+v", list)
	}

	wantCode(t, e.renameTeam(memberTok, org, id, "x"), connect.CodePermissionDenied)
	res := must[iamv1.UpdateTeamResponse](t)(e.client.UpdateTeam(ctx, req(ownerTok, &iamv1.UpdateTeamRequest{Org: org.String(), Team: id.String(), Name: "platform"})))
	if res.Team.Name != "platform" || res.Team.Id != id.String() {
		t.Fatalf("update: %+v", res)
	}
	if es := e.audit(store.ActionTeamUpdate); len(es) != 2 || es[0].Result != store.ResultAllowed || es[0].Details["name"] != "platform" {
		t.Fatalf("update audit: %+v", es)
	}
	wantCode(t, e.renameTeam(ownerTok, org, uuid.New(), "x"), connect.CodeNotFound)
	_, err = e.client.GetTeam(ctx, req(ownerTok, &iamv1.GetTeamRequest{Org: org.String(), Team: id.String() + "x"}))
	wantCode(t, err, connect.CodeInvalidArgument)
	wantCode(t, e.deleteTeam(memberTok, org, id), connect.CodePermissionDenied)
	e.takeNotified()
	if err := e.deleteTeam(ownerTok, org, id); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if n := e.takeNotified(); !slices.Equal(n, []uuid.UUID{org}) {
		t.Fatalf("notified %v", n)
	}
	if es := e.audit(store.ActionTeamDelete); len(es) != 2 || es[0].Result != store.ResultAllowed || es[0].Resource != "org/teams/"+id.String() {
		t.Fatalf("delete audit: %+v", es)
	}
	_, err = e.getTeam(ownerTok, org, id)
	wantCode(t, err, connect.CodeNotFound)
}

func TestCreateTeamReservedName(t *testing.T) {
	e := newEnv(t)
	_, ownerTok := e.user("alice")
	org := e.createOrg(ownerTok, "Acme")
	id := e.createTeam(ownerTok, org, "core")
	for _, name := range []string{"me", "org", "observatory"} {
		_, err := e.client.CreateTeam(context.Background(), req(ownerTok, &iamv1.CreateTeamRequest{Org: org.String(), Name: name}))
		wantCode(t, err, connect.CodeInvalidArgument)
		if !strings.Contains(err.Error(), "reserved for image references") {
			t.Fatalf("message: %v", err)
		}
		wantCode(t, e.renameTeam(ownerTok, org, id, name), connect.CodeInvalidArgument)
	}
}

func TestGetTeamCapabilities(t *testing.T) {
	e := newEnv(t)
	_, ownerTok := e.user("alice")
	member, memberTok := e.user("bob")
	org := e.createOrg(ownerTok, "Acme")
	e.addMember(org, member, store.RoleMember)
	team := e.createTeam(ownerTok, org, "core")

	got, err := e.getTeam(memberTok, org, team)
	if err != nil || got.Team.Name != "core" || len(got.Members) != 0 || got.CanManageMembers || got.CanUpdate || got.CanDelete || !got.CanLeave {
		t.Fatalf("member: %+v %v", got, err)
	}
	if err := e.setTeamMember(ownerTok, org, team, member, true); err != nil {
		t.Fatal(err)
	}
	got, _ = e.getTeam(memberTok, org, team)
	if len(got.Members) != 1 || got.Members[0].Id != member.ID.String() || got.Members[0].Email != "bob@x.io" || got.Members[0].Name != "bob" || !got.Members[0].Admin ||
		!got.CanManageMembers || got.CanUpdate || got.CanDelete || got.Team.MemberCount != 1 {
		t.Fatalf("team admin: %+v", got)
	}
	got, _ = e.getTeam(ownerTok, org, team)
	if !got.CanManageMembers || !got.CanUpdate || !got.CanDelete || !got.CanLeave {
		t.Fatalf("owner: %+v", got)
	}
}

func TestSetTeamMemberTeamAdmin(t *testing.T) {
	e := newEnv(t)
	_, ownerTok := e.user("alice")
	lead, leadTok := e.user("bob")
	peer, _ := e.user("carol")
	org := e.createOrg(ownerTok, "Acme")
	e.addMember(org, lead, store.RoleMember)
	e.addMember(org, peer, store.RoleMember)
	mine, other := e.createTeam(ownerTok, org, "mine"), e.createTeam(ownerTok, org, "other")

	wantCode(t, e.setTeamMember(leadTok, org, mine, lead, false), connect.CodePermissionDenied)
	if err := e.setTeamMember(ownerTok, org, mine, lead, true); err != nil {
		t.Fatalf("owner makes admin: %v", err)
	}
	e.takeNotified()
	if err := e.setTeamMember(leadTok, org, mine, peer, false); err != nil {
		t.Fatalf("team admin adds: %v", err)
	}
	if n := e.takeNotified(); !slices.Equal(n, []uuid.UUID{org}) {
		t.Fatalf("notified %v", n)
	}
	if es := e.audit(store.ActionTeamMemberSet); len(es) != 2 || es[0].Resource != "org/teams/"+mine.String()+"/members/"+peer.ID.String() || es[0].Details["admin"] != false {
		t.Fatalf("set audit: %+v", es)
	}
	before := e.denied()
	wantCode(t, e.setTeamMember(leadTok, org, mine, peer, true), connect.CodePermissionDenied)
	if es := e.audit(store.ActionTeamUpdate); e.denied() != before+1 || es[0].Result != store.ResultDenied || es[0].Resource != "org/teams/"+mine.String() {
		t.Fatalf("promote denied audit: %+v", es)
	}
	wantCode(t, e.setTeamMember(leadTok, org, other, peer, false), connect.CodePermissionDenied)
	wantCode(t, e.removeTeamMember(leadTok, org, other, peer), connect.CodePermissionDenied)
	if got, _ := e.getTeam(leadTok, org, mine); len(got.Members) != 2 || !got.CanManageMembers {
		t.Fatalf("detail: %+v", got)
	}
	if err := e.removeTeamMember(leadTok, org, mine, peer); err != nil {
		t.Fatalf("team admin removes: %v", err)
	}
	wantCode(t, e.removeTeamMember(leadTok, org, mine, peer), connect.CodeNotFound)
	if es := e.audit(store.ActionTeamMemberRemove); len(es) != 1 || es[0].Result != store.ResultAllowed || es[0].Resource != "org/teams/"+mine.String()+"/members/"+peer.ID.String() {
		t.Fatalf("remove audit: %+v", es)
	}
}

func TestSetTeamMemberAdminRequired(t *testing.T) {
	e := newEnv(t)
	_, ownerTok := e.user("alice")
	member, _ := e.user("bob")
	org := e.createOrg(ownerTok, "Acme")
	e.addMember(org, member, store.RoleMember)
	team := e.createTeam(ownerTok, org, "core")
	before := len(e.audit(store.ActionTeamMemberSet))

	_, err := e.client.SetTeamMember(context.Background(), req(ownerTok, &iamv1.SetTeamMemberRequest{Org: org.String(), Team: team.String(), User: member.ID.String()}))
	wantCode(t, err, connect.CodeInvalidArgument)
	if err.(*connect.Error).Message() != "admin is required" {
		t.Fatalf("message: %v", err)
	}
	code, msg := e.restSend("PUT", "/api/iam/v1/orgs/"+org.String()+"/teams/"+team.String()+"/members/"+member.ID.String(), ownerTok, `{}`)
	if code != 400 || msg != "admin is required" {
		t.Fatalf("rest: %d %q", code, msg)
	}
	if ms, _ := e.store.TeamMembers(t.Context(), org, team); len(ms) != 0 || len(e.audit(store.ActionTeamMemberSet)) != before {
		t.Fatalf("changed: %+v", ms)
	}
}

func TestSetTeamMemberNotOrgMember(t *testing.T) {
	e := newEnv(t)
	_, ownerTok := e.user("alice")
	stranger, _ := e.user("bob")
	org := e.createOrg(ownerTok, "Acme")
	team := e.createTeam(ownerTok, org, "core")
	wantCode(t, e.setTeamMember(ownerTok, org, team, stranger, false), connect.CodeInvalidArgument)
	_, err := e.client.SetTeamMember(context.Background(), req(ownerTok, &iamv1.SetTeamMemberRequest{Org: org.String(), Team: team.String(), User: stranger.ID.String() + "x", Admin: new(bool)}))
	wantCode(t, err, connect.CodeInvalidArgument)
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
	_, err := e.getTeam(aTok, orgA, teamB)
	wantCode(t, err, connect.CodeNotFound)
	wantCode(t, e.renameTeam(aTok, orgA, teamB, "x"), connect.CodeNotFound)
	wantCode(t, e.deleteTeam(aTok, orgA, teamB), connect.CodeNotFound)
	wantCode(t, e.setTeamMember(aTok, orgA, teamB, alice, false), connect.CodeNotFound)
	_, err = e.client.RemoveTeamMember(context.Background(), req(aTok, &iamv1.RemoveTeamMemberRequest{Org: orgA.String(), Team: teamB.String(), User: uuid.NewString()}))
	wantCode(t, err, connect.CodeNotFound)
	_, err = e.getTeam(aTok, orgB, teamB)
	wantCode(t, err, connect.CodeNotFound)
	if got, err := e.store.Team(t.Context(), orgB, teamB); err != nil || got.Name != "core" || got.MemberCount != 0 {
		t.Fatalf("B changed: %+v %v", got, err)
	}
}

func TestDeleteTeamRemovesBindings(t *testing.T) {
	e := newEnv(t)
	_, ownerTok := e.user("alice")
	org := e.createOrg(ownerTok, "Acme")
	team, keep := e.createTeam(ownerTok, org, "core"), e.createTeam(ownerTok, org, "keep")
	role := e.createRole(ownerTok, org, &iamv1.CreateRoleRequest{Name: "r"})
	for _, id := range []uuid.UUID{team, keep} {
		e.bind(ownerTok, org, authz.TeamSubject(id), role)
	}
	if err := e.deleteTeam(ownerTok, org, team); err != nil {
		t.Fatalf("delete: %v", err)
	}
	bs, err := e.store.Bindings(t.Context(), org)
	if err != nil || len(bs) != 1 || bs[0].Subject != authz.TeamSubject(keep) {
		t.Fatalf("bindings: %+v %v", bs, err)
	}
}

func TestSetTeamMemberDemoteNeedsUpdate(t *testing.T) {
	e := newEnv(t)
	_, ownerTok := e.user("alice")
	lead, leadTok := e.user("bob")
	peer, _ := e.user("carol")
	boss, bossTok := e.user("dave")
	org := e.createOrg(ownerTok, "Acme")
	e.addMember(org, lead, store.RoleMember)
	e.addMember(org, peer, store.RoleMember)
	e.addMember(org, boss, store.RoleAdmin)
	team := e.createTeam(ownerTok, org, "core")
	for _, u := range []store.User{lead, peer} {
		if err := e.setTeamMember(ownerTok, org, team, u, true); err != nil {
			t.Fatal(err)
		}
	}
	before, deniedBefore := len(e.audit(store.ActionTeamUpdate)), e.denied()
	wantCode(t, e.setTeamMember(leadTok, org, team, peer, false), connect.CodePermissionDenied)
	wantCode(t, e.removeTeamMember(leadTok, org, team, peer), connect.CodePermissionDenied)
	es := e.audit(store.ActionTeamUpdate)
	if len(es) != before+2 || e.denied() != deniedBefore+2 || es[0].Result != store.ResultDenied || es[0].ActorID != lead.ID.String() || es[0].Resource != "org/teams/"+team.String() {
		t.Fatalf("denied audit: %+v", es)
	}
	if ms, _ := e.store.TeamMembers(t.Context(), org, team); len(ms) != 2 || !ms[0].Admin || !ms[1].Admin {
		t.Fatalf("members changed: %+v", ms)
	}
	if err := e.setTeamMember(bossTok, org, team, peer, false); err != nil {
		t.Fatalf("org admin demotes: %v", err)
	}
	if err := e.removeTeamMember(bossTok, org, team, lead); err != nil {
		t.Fatalf("org admin removes: %v", err)
	}
}

func TestLeaveTeam(t *testing.T) {
	e := newEnv(t)
	_, ownerTok := e.user("alice")
	member, memberTok := e.user("bob")
	lead, leadTok := e.user("carol")
	org := e.createOrg(ownerTok, "Acme")
	e.addMember(org, member, store.RoleMember)
	e.addMember(org, lead, store.RoleMember)
	team := e.createTeam(ownerTok, org, "ops")
	if err := e.setTeamMember(ownerTok, org, team, member, false); err != nil {
		t.Fatal(err)
	}
	if err := e.setTeamMember(ownerTok, org, team, lead, true); err != nil {
		t.Fatal(err)
	}

	wantCode(t, e.removeTeamMember(memberTok, org, team, lead), connect.CodePermissionDenied)
	wantCode(t, e.removeTeamMember(leadTok, org, team, lead), connect.CodeInvalidArgument)
	e.takeNotified()
	if err := e.leaveTeam(memberTok, org, team); err != nil {
		t.Fatalf("leave: %v", err)
	}
	if n := e.takeNotified(); !slices.Equal(n, []uuid.UUID{org}) {
		t.Fatalf("notified %v", n)
	}
	if es := e.audit(store.ActionTeamLeave); len(es) != 1 || es[0].Result != store.ResultAllowed || es[0].ActorID != member.ID.String() || es[0].Resource != "org/teams/"+team.String()+"/members/"+member.ID.String() {
		t.Fatalf("leave audit: %+v", es)
	}
	wantCode(t, e.leaveTeam(memberTok, org, team), connect.CodeNotFound)
	// A team admin may leave too, although demoting a team admin otherwise needs iam.teams.update.
	if err := e.leaveTeam(leadTok, org, team); err != nil {
		t.Fatalf("team admin leaves: %v", err)
	}

	noLeave := e.createRole(ownerTok, org, &iamv1.CreateRoleRequest{Name: "stay", Rules: []*iamv1.RoleRule{rule("deny", "org/teams/*", "iam.teams.leave")}})
	e.bind(ownerTok, org, authz.UserSubject(member.ID), noLeave)
	if err := e.setTeamMember(ownerTok, org, team, member, false); err != nil {
		t.Fatal(err)
	}
	if got, _ := e.getTeam(memberTok, org, team); got.CanLeave {
		t.Fatal("canLeave despite deny")
	}
	before := e.denied()
	wantCode(t, e.leaveTeam(memberTok, org, team), connect.CodePermissionDenied)
	if es := e.audit(store.ActionTeamLeave); es[0].Result != store.ResultDenied || e.denied() != before+1 {
		t.Fatalf("denied leave audit: %+v", es)
	}
	if ms, _ := e.store.TeamMembers(t.Context(), org, team); len(ms) != 1 {
		t.Fatalf("left despite deny: %+v", ms)
	}
}

func TestDeleteTeamOwningDisk(t *testing.T) {
	e := newEnv(t)
	_, ownerTok := e.user("alice")
	org := e.createOrg(ownerTok, "Acme")
	team := e.createTeam(ownerTok, org, "core")
	e.resources.setDiskOwner(authz.TeamSubject(team), true)

	before := len(e.audit(""))
	e.takeNotified()
	err := e.deleteTeam(ownerTok, org, team)
	wantCode(t, err, connect.CodeFailedPrecondition)
	if err.(*connect.Error).Message() != store.ErrOwnsResources.Error() || len(e.audit("")) != before || len(e.takeNotified()) != 0 {
		t.Fatalf("refusal: %v", err)
	}
	e.resources.setDiskOwner(authz.TeamSubject(team), false)
	if err := e.deleteTeam(ownerTok, org, team); err != nil {
		t.Fatalf("delete after disk deleted: %v", err)
	}
}

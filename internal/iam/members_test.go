package iam

import (
	"context"
	"testing"

	"connectrpc.com/connect"
	"github.com/google/uuid"

	iamv1 "github.com/asterism-dev/observatory/gen/go/observatory/iam/v1"
	"github.com/asterism-dev/observatory/internal/authz"
	"github.com/asterism-dev/observatory/internal/store"
)

func (e *env) deleteMember(jwt string, org uuid.UUID, u store.User) error {
	_, err := e.client.DeleteMember(context.Background(), req(jwt, &iamv1.DeleteMemberRequest{Org: org.String(), User: u.ID.String()}))
	return err
}

func (e *env) updateMember(jwt string, org uuid.UUID, u store.User, role string) error {
	_, err := e.client.UpdateMember(context.Background(), req(jwt, &iamv1.UpdateMemberRequest{Org: org.String(), User: u.ID.String(), Role: role}))
	return err
}

func (e *env) leaveOrg(jwt string, org uuid.UUID) error {
	_, err := e.client.LeaveOrg(context.Background(), req(jwt, &iamv1.LeaveOrgRequest{OrgId: org.String()}))
	return err
}

func TestUpdateMemberAdminCannotPromote(t *testing.T) {
	e := newEnv(t)
	_, ownerTok := e.user("alice")
	admin, adminTok := e.user("bob")
	member, _ := e.user("carol")
	org := e.createOrg(ownerTok, "Acme")
	e.addMember(org, admin, store.RoleAdmin)
	e.addMember(org, member, store.RoleMember)

	wantCode(t, e.updateMember(adminTok, org, member, "admin"), connect.CodePermissionDenied)
	es := e.audit(store.ActionMemberSetRole)
	if len(es) != 1 || es[0].Result != store.ResultDenied || es[0].ActorID != admin.ID.String() || *es[0].OrgID != org || es[0].Resource != "org/members" {
		t.Fatalf("denied audit: %+v", es)
	}
	if e.role(org, member) != store.RoleMember {
		t.Fatal("role changed")
	}
}

func TestUpdateMemberOwnerPromotes(t *testing.T) {
	e := newEnv(t)
	owner, ownerTok := e.user("alice")
	member, memberTok := e.user("bob")
	org := e.createOrg(ownerTok, "Acme")
	e.addMember(org, member, store.RoleMember)

	list := must[iamv1.ListMembersResponse](t)(e.client.ListMembers(context.Background(), req(memberTok, &iamv1.ListMembersRequest{Org: org.String()}))).Members
	if len(list) != 2 || list[0].Id != owner.ID.String() || list[0].Email != "alice@x.io" || list[0].Name != "alice" || list[0].Role != "owner" || list[0].JoinedAt == nil {
		t.Fatalf("list: %+v", list)
	}
	for _, role := range []string{"boss", ""} {
		wantCode(t, e.updateMember(ownerTok, org, member, role), connect.CodeInvalidArgument)
	}
	if err := e.updateMember(ownerTok, org, member, "owner"); err != nil {
		t.Fatalf("promote: %v", err)
	}
	es := e.audit(store.ActionMemberSetRole)
	if len(es) != 1 || es[0].Result != store.ResultAllowed || es[0].ActorID != owner.ID.String() || es[0].Resource != "org/members/"+member.ID.String() || es[0].Details["role"] != "owner" {
		t.Fatalf("audit: %+v", es)
	}
	// The new owner can use owner rights right away; the last remaining owner can't step down.
	if err := e.updateMember(memberTok, org, owner, "member"); err != nil {
		t.Fatalf("demote: %v", err)
	}
	wantCode(t, e.updateMember(memberTok, org, member, "admin"), connect.CodeFailedPrecondition)
	if e.role(org, member) != store.RoleOwner || e.role(org, owner) != store.RoleMember {
		t.Fatal("roles")
	}
}

func TestDeleteMemberAdminCannotRemoveOwner(t *testing.T) {
	e := newEnv(t)
	owner, ownerTok := e.user("alice")
	admin, adminTok := e.user("bob")
	admin2, _ := e.user("carol")
	member, memberTok := e.user("dave")
	org := e.createOrg(ownerTok, "Acme")
	e.addMember(org, admin, store.RoleAdmin)
	e.addMember(org, admin2, store.RoleAdmin)
	e.addMember(org, member, store.RoleMember)

	for _, target := range []store.User{owner, admin2} {
		denied := e.denied()
		wantCode(t, e.deleteMember(adminTok, org, target), connect.CodePermissionDenied)
		// The guard denial is recorded once, by the handler, as in 0.x.
		if e.denied() != denied+1 {
			t.Fatalf("remove %s: %d denied events", target.Name, e.denied()-denied)
		}
	}
	if es := e.audit(store.ActionMemberSetRole); len(es) != 2 || es[0].Result != store.ResultDenied || es[0].ActorID != admin.ID.String() || es[0].Resource != "org/members" {
		t.Fatalf("denied audit: %+v", es)
	}
	wantCode(t, e.deleteMember(memberTok, org, admin), connect.CodePermissionDenied)
	if e.role(org, owner) != store.RoleOwner || e.role(org, admin2) != store.RoleAdmin || e.role(org, admin) != store.RoleAdmin {
		t.Fatal("member removed")
	}

	if err := e.deleteMember(adminTok, org, member); err != nil {
		t.Fatalf("remove member: %v", err)
	}
	es := e.audit(store.ActionMemberRemove)
	if len(es) != 2 || es[0].Result != store.ResultAllowed || es[0].Resource != "org/members/"+member.ID.String() || es[1].Result != store.ResultDenied {
		t.Fatalf("audit: %+v", es)
	}
	if e.role(org, member) != "" {
		t.Fatal("still a member")
	}
	_, err := e.getOrg(memberTok, org)
	wantCode(t, err, connect.CodeNotFound)
	if err := e.deleteMember(ownerTok, org, admin2); err != nil {
		t.Fatalf("owner removes admin: %v", err)
	}
}

func TestDeleteMemberSelfUseLeaveOrg(t *testing.T) {
	e := newEnv(t)
	owner, ownerTok := e.user("alice")
	org := e.createOrg(ownerTok, "Acme")
	err := e.deleteMember(ownerTok, org, owner)
	wantCode(t, err, connect.CodeInvalidArgument)
	if connect.CodeOf(err) == connect.CodeInvalidArgument && err.(*connect.Error).Message() != "use LeaveOrg" {
		t.Fatalf("message: %v", err)
	}
	if e.role(org, owner) != store.RoleOwner {
		t.Fatal("owner left")
	}
}

func TestMemberDeleteAuditEventOnce(t *testing.T) {
	e := newEnv(t)
	owner, ownerTok := e.user("alice")
	bob, _ := e.user("bob")
	org := e.createOrg(ownerTok, "Acme")
	e.addMember(org, bob, store.RoleMember)
	before := len(e.audit(""))

	if err := e.deleteMember(ownerTok, org, bob); err != nil {
		t.Fatal(err)
	}
	all := e.audit("")
	es := e.audit(store.ActionMemberRemove)
	if len(all) != before+1 || len(es) != 1 || es[0].Resource != "org/members/"+bob.ID.String() || es[0].Result != store.ResultAllowed ||
		es[0].ActorID != owner.ID.String() || *es[0].OrgID != org {
		t.Fatalf("events: %+v", all[:len(all)-before])
	}
}

func TestLeaveOrgNoPermissionNeeded(t *testing.T) {
	e := newEnv(t)
	owner, ownerTok := e.user("alice")
	member, memberTok := e.user("bob")
	org := e.createOrg(ownerTok, "Acme")
	e.addMember(org, member, store.RoleMember)
	e.bindRole(org, authz.UserSubject(member.ID), "deny", "*", "*")

	if err := e.leaveOrg(memberTok, org); err != nil {
		t.Fatalf("leave: %v", err)
	}
	es := e.audit(store.ActionMemberLeave)
	if len(es) != 1 || es[0].ActorID != member.ID.String() || *es[0].OrgID != org || es[0].Result != store.ResultAllowed || es[0].Resource != "org/members/"+member.ID.String() {
		t.Fatalf("audit: %+v", es)
	}
	_, err := e.getOrg(memberTok, org)
	wantCode(t, err, connect.CodeNotFound)
	wantCode(t, e.leaveOrg(ownerTok, org), connect.CodeFailedPrecondition)
	wantCode(t, e.leaveOrg(ownerTok, e.personalOrg(owner)), connect.CodeFailedPrecondition)
	if e.role(org, owner) != store.RoleOwner {
		t.Fatal("owner left")
	}
}

func TestLeaveOrgNonMemberNotFound(t *testing.T) {
	e := newEnv(t)
	_, ownerTok := e.user("alice")
	_, bobTok := e.user("bob")
	org := e.createOrg(ownerTok, "Acme")
	wantCode(t, e.leaveOrg(bobTok, org), connect.CodeNotFound)
	wantCode(t, e.leaveOrg(bobTok, uuid.New()), connect.CodeNotFound)
	_, err := e.client.LeaveOrg(context.Background(), req(bobTok, &iamv1.LeaveOrgRequest{OrgId: "nope"}))
	wantCode(t, err, connect.CodeInvalidArgument)
	if es := e.audit(store.ActionMemberLeave); len(es) != 0 {
		t.Fatalf("audit: %+v", es)
	}
}

// Removing a member drops their bindings; a re-invite would come back deny-free, so only owners/admins may do it.
func TestDeleteMemberCarryingDeny(t *testing.T) {
	e := newEnv(t)
	_, ownerTok := e.user("alice")
	admin, adminTok := e.user("bob")
	remover, removerTok := e.user("carol")
	restricted, _ := e.user("dave")
	plain, _ := e.user("erin")
	org := e.createOrg(ownerTok, "Acme")
	e.addMember(org, admin, store.RoleAdmin)
	for _, u := range []store.User{remover, restricted, plain} {
		e.addMember(org, u, store.RoleMember)
	}
	e.bindRole(org, authz.UserSubject(remover.ID), "allow", authz.ObjMembers, "iam.members.delete")
	e.bindRole(org, authz.UserSubject(restricted.ID), "deny", authz.ObjAudit, "audit.events.list")

	denied := e.denied()
	wantCode(t, e.deleteMember(removerTok, org, restricted), connect.CodePermissionDenied)
	es := e.audit(store.ActionMemberRemove)
	if e.denied() != denied+1 || len(es) != 1 || es[0].Result != store.ResultDenied || es[0].ActorID != remover.ID.String() || es[0].Resource != "org/members/"+restricted.ID.String() || es[0].Details["error"] == nil {
		t.Fatalf("denied audit: %+v", es)
	}
	if e.role(org, restricted) != store.RoleMember {
		t.Fatal("restricted member removed")
	}
	if err := e.deleteMember(removerTok, org, plain); err != nil {
		t.Fatalf("remove plain: %v", err)
	}
	if err := e.deleteMember(adminTok, org, restricted); err != nil {
		t.Fatalf("admin removes restricted: %v", err)
	}
}

func TestDeleteMemberOwningVM(t *testing.T) {
	e := newEnv(t)
	_, ownerTok := e.user("alice")
	bob, bobTok := e.user("bob")
	org := e.createOrg(ownerTok, "Acme")
	e.addMember(org, bob, store.RoleMember)
	e.resources.setOwner(authz.UserSubject(bob.ID), true)

	before := len(e.audit(""))
	err := e.deleteMember(ownerTok, org, bob)
	wantCode(t, err, connect.CodeFailedPrecondition)
	if err.(*connect.Error).Message() != store.ErrOwnsResources.Error() {
		t.Fatalf("message: %v", err)
	}
	wantCode(t, e.leaveOrg(bobTok, org), connect.CodeFailedPrecondition)
	if n := len(e.audit("")); n != before {
		t.Fatalf("audit events written: %d -> %d", before, n)
	}
	e.resources.setOwner(authz.UserSubject(bob.ID), false)
	if err := e.deleteMember(ownerTok, org, bob); err != nil {
		t.Fatalf("remove after VM deleted: %v", err)
	}
}

// The last-owner guard is still reachable through DeleteMember for a non-owner holding setRole and delete.
func TestDeleteMemberLastOwner(t *testing.T) {
	e := newEnv(t)
	owner, ownerTok := e.user("alice")
	bob, bobTok := e.user("bob")
	org := e.createOrg(ownerTok, "Acme")
	e.addMember(org, bob, store.RoleMember)
	e.bindRole(org, authz.UserSubject(bob.ID), "allow", authz.ObjMembers, store.ActionMemberSetRole)
	e.bindRole(org, authz.UserSubject(bob.ID), "allow", authz.ObjMembers, store.ActionMemberRemove)

	err := e.deleteMember(bobTok, org, owner)
	wantCode(t, err, connect.CodeFailedPrecondition)
	if err.(*connect.Error).Message() != store.ErrLastOwner.Error() {
		t.Fatalf("message: %v", err)
	}
	if e.role(org, owner) != store.RoleOwner {
		t.Fatal("last owner removed")
	}
	if es := e.audit(store.ActionMemberRemove); len(es) != 0 {
		t.Fatalf("audit: %+v", es)
	}
}

func TestCrossOrgMemberID(t *testing.T) {
	e := newEnv(t)
	_, aTok := e.user("alice")
	adminA, adminATok := e.user("bob")
	_, bTok := e.user("carol")
	victim, _ := e.user("dave")
	orgA := e.createOrg(aTok, "A")
	orgB := e.createOrg(bTok, "B")
	e.addMember(orgA, adminA, store.RoleAdmin)
	e.addMember(orgB, victim, store.RoleMember)
	ctx := context.Background()

	wantCode(t, e.deleteMember(adminATok, orgA, victim), connect.CodeNotFound)
	wantCode(t, e.updateMember(aTok, orgA, victim, "admin"), connect.CodeNotFound)
	inv := must[iamv1.CreateInviteResponse](t)(e.client.CreateInvite(ctx, req(bTok, &iamv1.CreateInviteRequest{Org: orgB.String(), Role: "member"}))).Invite
	_, err := e.client.DeleteInvite(ctx, req(adminATok, &iamv1.DeleteInviteRequest{Org: orgA.String(), Invite: inv.Id}))
	wantCode(t, err, connect.CodeNotFound)
	_, err = e.client.ListMembers(ctx, req(adminATok, &iamv1.ListMembersRequest{Org: orgB.String()}))
	wantCode(t, err, connect.CodeNotFound)
	_, err = e.client.ListInvites(ctx, req(adminATok, &iamv1.ListInvitesRequest{Org: orgB.String()}))
	wantCode(t, err, connect.CodeNotFound)
	if e.role(orgB, victim) != store.RoleMember {
		t.Fatal("victim changed")
	}
	list := must[iamv1.ListInvitesResponse](t)(e.client.ListInvites(ctx, req(bTok, &iamv1.ListInvitesRequest{Org: orgB.String()}))).Invites
	if len(list) != 1 || list[0].Id != inv.Id {
		t.Fatalf("invite in B: %+v", list)
	}
}

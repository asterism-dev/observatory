package iam

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"connectrpc.com/connect"
	"github.com/google/uuid"
	"google.golang.org/protobuf/encoding/protojson"

	iamv1 "github.com/asterism-dev/observatory/gen/go/observatory/iam/v1"
	"github.com/asterism-dev/observatory/internal/authz"
	"github.com/asterism-dev/observatory/internal/store"
)

func (e *env) invite(org uuid.UUID, jwt, role string) string {
	e.t.Helper()
	res, err := e.client.CreateInvite(context.Background(), req(jwt, &iamv1.CreateInviteRequest{Org: org.String(), Role: role}))
	if err != nil {
		e.t.Fatalf("invite: %v", err)
	}
	return res.Msg.Token
}

func (e *env) accept(jwt, token string) (*iamv1.AcceptInviteResponse, error) {
	res, err := e.client.AcceptInvite(context.Background(), req(jwt, &iamv1.AcceptInviteRequest{Token: token}))
	if err != nil {
		return nil, err
	}
	return res.Msg, nil
}

func TestCreateInviteAsAdminRequiresOwner(t *testing.T) {
	e := newEnv(t)
	_, ownerTok := e.user("alice")
	admin, adminTok := e.user("bob")
	member, memberTok := e.user("carol")
	org := e.createOrg(ownerTok, "Acme")
	e.addMember(org, admin, store.RoleAdmin)
	e.addMember(org, member, store.RoleMember)
	ctx := context.Background()
	create := func(jwt, role string) (*iamv1.CreateInviteResponse, error) {
		res, err := e.client.CreateInvite(ctx, req(jwt, &iamv1.CreateInviteRequest{Org: org.String(), Role: role}))
		if err != nil {
			return nil, err
		}
		return res.Msg, nil
	}

	for _, role := range []string{"admin", "owner"} {
		denied := e.denied()
		_, err := create(adminTok, role)
		wantCode(t, err, connect.CodePermissionDenied)
		if e.denied() != denied+1 {
			t.Fatalf("admin invites %s: %d denied events", role, e.denied()-denied)
		}
	}
	if es := e.audit(store.ActionMemberSetRole); len(es) != 2 || es[0].Result != store.ResultDenied || es[0].Resource != "org/members" {
		t.Fatalf("denied audit: %+v", es)
	}
	_, err := create(memberTok, "member")
	wantCode(t, err, connect.CodePermissionDenied)
	_, err = create(adminTok, "boss")
	wantCode(t, err, connect.CodeInvalidArgument)

	created, err := create(adminTok, "member")
	if err != nil || !strings.HasPrefix(created.Token, "obsinv_") || created.Url != "https://hub.test/invite/"+created.Token || created.Invite.ExpiresAt == nil || created.Invite.Role != "member" {
		t.Fatalf("create: %+v %v", created, err)
	}
	if _, err := create(ownerTok, "admin"); err != nil {
		t.Fatalf("owner invites admin: %v", err)
	}
	es := e.audit(store.ActionMemberInvite)
	if len(es) != 3 || es[1].Result != store.ResultAllowed || es[1].Resource != "org/invites/"+created.Invite.Id || es[1].Details["role"] != "member" {
		t.Fatalf("audit: %+v", es)
	}

	list := must[iamv1.ListInvitesResponse](t)(e.client.ListInvites(ctx, req(adminTok, &iamv1.ListInvitesRequest{Org: org.String()})))
	if b, _ := protojson.Marshal(list); strings.Contains(string(b), created.Token) || len(list.Invites) != 2 || list.Invites[0].CreatedBy != admin.ID.String() && list.Invites[1].CreatedBy != admin.ID.String() {
		t.Fatalf("list: %s", b)
	}
	_, err = e.client.ListInvites(ctx, req(memberTok, &iamv1.ListInvitesRequest{Org: org.String()}))
	wantCode(t, err, connect.CodePermissionDenied)

	must[iamv1.DeleteInviteResponse](t)(e.client.DeleteInvite(ctx, req(adminTok, &iamv1.DeleteInviteRequest{Org: org.String(), Invite: created.Invite.Id})))
	if es := e.audit(store.ActionInviteRevoke); len(es) != 1 || es[0].Resource != "org/invites/"+created.Invite.Id {
		t.Fatalf("revoke audit: %+v", es)
	}
	_, err = e.client.DeleteInvite(ctx, req(adminTok, &iamv1.DeleteInviteRequest{Org: org.String(), Invite: created.Invite.Id}))
	wantCode(t, err, connect.CodeNotFound)
	if left := must[iamv1.ListInvitesResponse](t)(e.client.ListInvites(ctx, req(adminTok, &iamv1.ListInvitesRequest{Org: org.String()}))).Invites; len(left) != 1 || left[0].Role != "admin" {
		t.Fatalf("after revoke: %+v", left)
	}
}

func TestAcceptInvite(t *testing.T) {
	e := newEnv(t)
	owner, ownerTok := e.user("alice")
	joiner, joinerTok := e.user("bob")
	_, otherTok := e.user("carol")
	org := e.createOrg(ownerTok, "Acme")

	_, err := e.client.CreateInvite(context.Background(), req(ownerTok, &iamv1.CreateInviteRequest{Org: e.personalOrg(owner).String(), Role: "member"}))
	wantCode(t, err, connect.CodeFailedPrecondition)
	token := e.invite(org, ownerTok, "admin")

	_, err = e.accept(ownerTok, token)
	wantCode(t, err, connect.CodeFailedPrecondition)
	got, err := e.accept(joinerTok, token)
	if err != nil || got.OrgId != org.String() || got.Role != "admin" {
		t.Fatalf("accept: %+v %v", got, err)
	}
	if _, err := e.client.ListInvites(context.Background(), req(joinerTok, &iamv1.ListInvitesRequest{Org: org.String()})); err != nil {
		t.Fatalf("new admin can't list invites: %v", err)
	}
	for _, tk := range []string{token, "obsinv_nope", ""} {
		_, err := e.accept(otherTok, tk)
		wantCode(t, err, connect.CodeInvalidArgument)
	}

	var allowed, failed int
	for _, en := range e.audit(store.ActionInviteAccept) {
		if strings.Contains(en.Resource, "obsinv_") || strings.Contains(fmt.Sprint(en.Details), "obsinv_") {
			t.Fatalf("token in audit: %+v", en)
		}
		switch en.Result {
		case store.ResultAllowed:
			allowed++
			if en.ActorID != joiner.ID.String() || en.OrgID == nil || *en.OrgID != org || en.Resource != "org/members" || en.Details["role"] != "admin" {
				t.Fatalf("allowed audit: %+v", en)
			}
		case store.ResultFailed:
			failed++
			if en.OrgID != nil {
				t.Fatalf("failed audit has org: %+v", en)
			}
		}
	}
	if allowed != 1 || failed != 4 {
		t.Fatalf("allowed %d, failed %d", allowed, failed)
	}
}

func TestAcceptInviteRechecksInviter(t *testing.T) {
	e := newEnv(t)
	_, ownerTok := e.user("alice")
	owner2, owner2Tok := e.user("bob")
	admin, adminTok := e.user("carol")
	inviter, inviterTok := e.user("dave")
	_, joinerTok := e.user("erin")
	org := e.createOrg(ownerTok, "Acme")
	e.addMember(org, owner2, store.RoleOwner)
	e.addMember(org, admin, store.RoleAdmin)
	e.addMember(org, inviter, store.RoleMember)
	accept := func(token string) connect.Code {
		_, err := e.accept(joinerTok, token)
		return connect.CodeOf(err)
	}

	adminInvite := e.invite(org, adminTok, "member")
	if err := e.deleteMember(ownerTok, org, admin); err != nil {
		t.Fatal(err)
	}
	if code := accept(adminInvite); code != connect.CodeInvalidArgument {
		t.Fatalf("removed creator: %v", code)
	}

	ownerInvite := e.invite(org, owner2Tok, "admin")
	if err := e.updateMember(ownerTok, org, owner2, "member"); err != nil {
		t.Fatal(err)
	}
	if code := accept(ownerInvite); code != connect.CodeInvalidArgument {
		t.Fatalf("demoted creator: %v", code)
	}

	e.bindRole(org, authz.UserSubject(inviter.ID), "allow", authz.ObjMembers, "iam.invites.create")
	boundInvite := e.invite(org, inviterTok, "member")
	e.exec(`DELETE FROM role_bindings WHERE subject = $1`, authz.UserSubject(inviter.ID))
	e.reload()
	if code := accept(boundInvite); code != connect.CodeInvalidArgument {
		t.Fatalf("unbound creator: %v", code)
	}

	invs, err := e.store.Invites(context.Background(), org)
	if err != nil || len(invs) != 3 {
		t.Fatalf("invites left: %d %v", len(invs), err)
	}
	var failed int
	for _, en := range e.audit(store.ActionInviteAccept) {
		if en.Result != store.ResultFailed || en.OrgID != nil || en.Details["reason"] != "invalid invite: inviter lacks permission" {
			t.Fatalf("audit: %+v", en)
		}
		failed++
	}
	if failed != 3 {
		t.Fatalf("failed audits: %d", failed)
	}
	if _, err := e.accept(joinerTok, e.invite(org, ownerTok, "admin")); err != nil {
		t.Fatalf("valid creator: %v", err)
	}
}

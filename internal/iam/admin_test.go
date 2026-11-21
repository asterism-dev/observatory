package iam

import (
	"context"
	"testing"

	"connectrpc.com/connect"
	"github.com/google/uuid"

	iamv1 "github.com/asterism-dev/observatory/gen/go/observatory/iam/v1"
	"github.com/asterism-dev/observatory/internal/store"
)

func (e *env) setPlatformAdmin(jwt string, user uuid.UUID, admin bool) error {
	_, err := e.client.SetPlatformAdmin(context.Background(), req(jwt, &iamv1.SetPlatformAdminRequest{UserId: user.String(), Admin: admin}))
	return err
}

func TestListUsersNonAdminDenied(t *testing.T) {
	e := newEnv(t)
	e.user("root")
	alice, tok := e.user("alice")
	org := e.createOrg(tok, "Acme")
	ctx := context.Background()
	calls := []struct {
		action string
		call   func() error
	}{
		{store.ActionAdminRead, func() error {
			_, err := e.client.ListUsers(ctx, req(tok, &iamv1.ListUsersRequest{}))
			return err
		}},
		{store.ActionAdminRead, func() error {
			_, err := e.client.ListAllOrgs(ctx, req(tok, &iamv1.ListAllOrgsRequest{}))
			return err
		}},
		{store.ActionAdminRead, func() error {
			_, err := e.client.GetAnyOrg(ctx, req(tok, &iamv1.GetAnyOrgRequest{OrgId: org.String()}))
			return err
		}},
		{"iam.platformAdmins.update", func() error { return e.setPlatformAdmin(tok, alice.ID, true) }},
	}
	for i, c := range calls {
		before := e.denied()
		wantCode(t, c.call(), connect.CodePermissionDenied)
		es := e.audit(c.action)
		if e.denied() != before+1 || len(es) == 0 || es[0].Result != store.ResultDenied || es[0].ActorID != alice.ID.String() {
			t.Fatalf("%d: audit %+v", i, es)
		}
	}
	if u, _ := e.store.User(ctx, alice.ID); u.PlatformAdmin {
		t.Fatal("alice became admin")
	}
}

func TestPlatformAdminReads(t *testing.T) {
	e := newEnv(t)
	_, rootTok := e.user("root")
	_, tok := e.user("alice")
	org := e.createOrg(tok, "Acme")
	ctx := context.Background()

	orgs := must[iamv1.ListAllOrgsResponse](t)(e.client.ListAllOrgs(ctx, req(rootTok, &iamv1.ListAllOrgsRequest{}))).Orgs
	if len(orgs) != 3 {
		t.Fatalf("orgs: %+v", orgs)
	}
	for _, o := range orgs {
		if o.Id == org.String() && (o.Name != "Acme" || o.Personal || o.MemberCount != 1 || o.CreatedAt == nil) {
			t.Fatalf("org: %+v", o)
		}
	}
	got := must[iamv1.GetAnyOrgResponse](t)(e.client.GetAnyOrg(ctx, req(rootTok, &iamv1.GetAnyOrgRequest{OrgId: org.String()})))
	if got.Org.Id != org.String() || got.Org.Name != "Acme" || got.Org.Personal || len(got.Members) != 1 || got.Members[0].Role != "owner" || got.Members[0].Email != "alice@x.io" {
		t.Fatalf("detail: %+v", got)
	}
	_, err := e.client.GetAnyOrg(ctx, req(rootTok, &iamv1.GetAnyOrgRequest{OrgId: uuid.NewString()}))
	wantCode(t, err, connect.CodeNotFound)
	_, err = e.client.GetAnyOrg(ctx, req(rootTok, &iamv1.GetAnyOrgRequest{OrgId: "nope"}))
	wantCode(t, err, connect.CodeInvalidArgument)

	users := must[iamv1.ListUsersResponse](t)(e.client.ListUsers(ctx, req(rootTok, &iamv1.ListUsersRequest{}))).Users
	if len(users) != 2 || users[0].Email != "alice@x.io" || users[0].PlatformAdmin || !users[1].PlatformAdmin || users[1].CreatedAt == nil {
		t.Fatalf("users: %+v", users)
	}
}

func TestSetPlatformAdminAudits(t *testing.T) {
	e := newEnv(t)
	root, rootTok := e.user("root")
	alice, _ := e.user("alice")
	ctx := context.Background()

	if err := e.setPlatformAdmin(rootTok, alice.ID, true); err != nil {
		t.Fatalf("grant: %v", err)
	}
	if es := e.audit(store.ActionPlatformGrantAdmin); len(es) != 1 || es[0].Result != store.ResultAllowed ||
		es[0].Resource != "user/"+alice.ID.String() || es[0].ActorID != root.ID.String() || es[0].OrgID != nil {
		t.Fatalf("grant audit: %+v", es)
	}
	// Platform admin comes from the JWT, so alice's new right needs a fresh one.
	alice, _ = e.store.User(ctx, alice.ID)
	aliceTok, _ := e.session(alice)
	if _, err := e.client.ListUsers(ctx, req(aliceTok, &iamv1.ListUsersRequest{})); err != nil {
		t.Fatalf("new admin: %v", err)
	}
	if err := e.setPlatformAdmin(rootTok, alice.ID, false); err != nil {
		t.Fatalf("revoke: %v", err)
	}
	if es := e.audit(store.ActionPlatformRevokeAdmin); len(es) != 1 || es[0].Resource != "user/"+alice.ID.String() {
		t.Fatalf("revoke audit: %+v", es)
	}
	wantCode(t, e.setPlatformAdmin(rootTok, uuid.New(), true), connect.CodeNotFound)
	if es := e.audit("iam.platformAdmins.update"); len(es) != 0 {
		t.Fatalf("interceptor recorded a write: %+v", es)
	}

	wantCode(t, e.setPlatformAdmin(rootTok, root.ID, false), connect.CodeFailedPrecondition)
	if es := e.audit(store.ActionPlatformRevokeAdmin); len(es) != 1 {
		t.Fatalf("failed revoke must not audit as allowed: %+v", es)
	}
}

func TestPlatformAdminHasNoTenantAccess(t *testing.T) {
	e := newEnv(t)
	_, rootTok := e.user("root")
	_, tok := e.user("alice")
	org := e.createOrg(tok, "Acme")
	ctx := context.Background()
	for i, call := range []func() error{
		func() error { _, err := e.getOrg(rootTok, org); return err },
		func() error {
			_, err := e.client.ListMembers(ctx, req(rootTok, &iamv1.ListMembersRequest{Org: org.String()}))
			return err
		},
		func() error {
			_, err := e.client.ListTeams(ctx, req(rootTok, &iamv1.IAMServiceListTeamsRequest{Org: org.String()}))
			return err
		},
		func() error {
			_, err := e.client.CreateTeam(ctx, req(rootTok, &iamv1.CreateTeamRequest{Org: org.String(), Name: "x"}))
			return err
		},
	} {
		if err := call(); connect.CodeOf(err) != connect.CodeNotFound {
			t.Fatalf("%d: %v", i, err)
		}
	}
}

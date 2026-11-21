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

func rule(effect, object, action string) *iamv1.RoleRule {
	return &iamv1.RoleRule{Effect: effect, Object: object, Action: action}
}

func (e *env) tryCreateRole(jwt string, org uuid.UUID, m *iamv1.CreateRoleRequest) (*iamv1.Role, error) {
	m.Org = org.String()
	res, err := e.client.CreateRole(context.Background(), req(jwt, m))
	if err != nil {
		return nil, err
	}
	return res.Msg.Role, nil
}

func (e *env) createRole(jwt string, org uuid.UUID, m *iamv1.CreateRoleRequest) uuid.UUID {
	e.t.Helper()
	r, err := e.tryCreateRole(jwt, org, m)
	if err != nil {
		e.t.Fatalf("create role: %v", err)
	}
	return uuid.MustParse(r.Id)
}

func (e *env) updateRole(jwt string, org, role uuid.UUID, m *iamv1.UpdateRoleRequest) error {
	m.Org, m.Role = org.String(), role.String()
	_, err := e.client.UpdateRole(context.Background(), req(jwt, m))
	return err
}

func (e *env) deleteRole(jwt string, org, role uuid.UUID) error {
	_, err := e.client.DeleteRole(context.Background(), req(jwt, &iamv1.DeleteRoleRequest{Org: org.String(), Role: role.String()}))
	return err
}

func (e *env) tryBind(jwt string, org uuid.UUID, subject, role string) (*iamv1.RoleBinding, error) {
	res, err := e.client.CreateRoleBinding(context.Background(), req(jwt, &iamv1.CreateRoleBindingRequest{Org: org.String(), Subject: subject, RoleId: role}))
	if err != nil {
		return nil, err
	}
	return res.Msg.Binding, nil
}

func (e *env) bind(jwt string, org uuid.UUID, subject string, role uuid.UUID) uuid.UUID {
	e.t.Helper()
	b, err := e.tryBind(jwt, org, subject, role.String())
	if err != nil {
		e.t.Fatalf("bind: %v", err)
	}
	return uuid.MustParse(b.Id)
}

func (e *env) unbind(jwt string, org, binding uuid.UUID) error {
	_, err := e.client.DeleteRoleBinding(context.Background(), req(jwt, &iamv1.DeleteRoleBindingRequest{Org: org.String(), Binding: binding.String()}))
	return err
}

func (e *env) roleCount(org uuid.UUID) int {
	e.t.Helper()
	var n int
	if err := e.pool.QueryRow(context.Background(), `SELECT count(*) FROM roles WHERE org_id = $1`, org).Scan(&n); err != nil {
		e.t.Fatal(err)
	}
	return n
}

func (e *env) bindingCount(org uuid.UUID) int {
	e.t.Helper()
	var n int
	if err := e.pool.QueryRow(context.Background(), `SELECT count(*) FROM role_bindings WHERE org_id = $1`, org).Scan(&n); err != nil {
		e.t.Fatal(err)
	}
	return n
}

// canReadAudit is the check the audit service makes for the org's trail.
func (e *env) canReadAudit(user, org uuid.UUID) bool {
	return e.z.Enforcer().Allowed(authz.UserSubject(user), org, authz.ObjAudit, "audit.events.list")
}

func errorDetail(t *testing.T, a store.AuditEntry) string {
	t.Helper()
	s, ok := a.Details["error"].(string)
	if !ok {
		t.Fatalf("no error detail: %+v", a)
	}
	return s
}

func TestRoleCRUDAndAudit(t *testing.T) {
	e := newEnv(t)
	owner, ownerTok := e.user("alice")
	member, memberTok := e.user("bob")
	org := e.createOrg(ownerTok, "Acme")
	e.addMember(org, member, store.RoleMember)
	ctx := context.Background()

	catalog := must[iamv1.ListCatalogResponse](t)(e.client.ListCatalog(ctx, req(memberTok, &iamv1.ListCatalogRequest{Org: org.String()}))).Entries
	if len(catalog) != len(authz.Catalog) || catalog[0].Action != "iam.orgs.update" || catalog[0].Object != "org/settings" {
		t.Fatalf("catalog: %+v", catalog)
	}

	_, err := e.tryCreateRole(memberTok, org, &iamv1.CreateRoleRequest{Name: "x"})
	wantCode(t, err, connect.CodePermissionDenied)
	tooMany := make([]*iamv1.RoleRule, 201)
	for i := range tooMany {
		tooMany[i] = rule("allow", "org/roles", "iam.roles.list")
	}
	for _, m := range []*iamv1.CreateRoleRequest{
		{Name: ""},
		{Name: "x", Description: "a\x00b"},
		{Name: "x", Rules: []*iamv1.RoleRule{rule("allow", "teams", "iam.teams.list")}},
		{Name: "x", Rules: []*iamv1.RoleRule{rule("maybe", "org/teams", "iam.teams.list")}},
		{Name: "x", Rules: []*iamv1.RoleRule{rule("allow", "org/teams", "nope.things.*")}},
		{Name: "x", Parents: []string{"owner"}},
		{Name: "x", Rules: tooMany},
	} {
		_, err := e.tryCreateRole(ownerTok, org, m)
		wantCode(t, err, connect.CodeInvalidArgument)
	}
	if e.roleCount(org) != 0 {
		t.Fatal("invalid role stored")
	}

	e.takeNotified()
	created, err := e.tryCreateRole(ownerTok, org, &iamv1.CreateRoleRequest{Name: " team lead ", Description: "leads",
		Rules: []*iamv1.RoleRule{rule("allow", "org/teams/*", "iam.teams.*")}, Parents: []string{"member"}})
	if err != nil || created.Name != "team lead" || created.Description != "leads" || !slices.Equal(created.Parents, []string{"member"}) || created.Builtin {
		t.Fatalf("create: %+v %v", created, err)
	}
	if n := e.takeNotified(); !slices.Equal(n, []uuid.UUID{org}) {
		t.Fatalf("notified %v", n)
	}
	// Wildcard actions are stored expanded so future catalog actions aren't granted implicitly.
	if len(created.Rules) != len(authz.ExpandActions("iam.teams.*")) || created.Rules[0].Action != "iam.teams.list" || created.Rules[0].Object != "org/teams/*" {
		t.Fatalf("rules not expanded: %+v", created.Rules)
	}
	_, err = e.tryCreateRole(ownerTok, org, &iamv1.CreateRoleRequest{Name: "team lead"})
	wantCode(t, err, connect.CodeFailedPrecondition)
	if es := e.audit(store.ActionRoleCreate); len(es) != 1 || es[0].Result != store.ResultAllowed || es[0].ActorID != owner.ID.String() || es[0].Resource != "org/roles/"+created.Id {
		t.Fatalf("create audit: %+v", es)
	}

	list := must[iamv1.ListRolesResponse](t)(e.client.ListRoles(ctx, req(memberTok, &iamv1.ListRolesRequest{Org: org.String()}))).Roles
	if len(list) != 4 || !list[0].Builtin || list[0].Key != "owner" || list[0].Name != "Owner" || len(list[0].Rules) == 0 || list[3].Id != created.Id || list[3].Builtin {
		t.Fatalf("list: %+v", list)
	}

	id := uuid.MustParse(created.Id)
	if err := e.updateRole(ownerTok, org, id, &iamv1.UpdateRoleRequest{Name: "lead", Rules: []*iamv1.RoleRule{rule("deny", "org/teams/*", "iam.teams.delete")}}); err != nil {
		t.Fatalf("update: %v", err)
	}
	list = must[iamv1.ListRolesResponse](t)(e.client.ListRoles(ctx, req(ownerTok, &iamv1.ListRolesRequest{Org: org.String()}))).Roles
	if got := list[3]; got.Name != "lead" || got.Description != "" || len(got.Rules) != 1 || got.Rules[0].Effect != "deny" || len(got.Parents) != 0 {
		t.Fatalf("after update: %+v", got)
	}
	if es := e.audit(store.ActionRoleUpdate); len(es) != 1 || es[0].Result != store.ResultAllowed || es[0].Details["name"] != "lead" {
		t.Fatalf("update audit: %+v", es)
	}
	wantCode(t, e.updateRole(ownerTok, org, uuid.New(), &iamv1.UpdateRoleRequest{Name: "y"}), connect.CodeNotFound)
	wantCode(t, e.deleteRole(memberTok, org, id), connect.CodePermissionDenied)
	if err := e.deleteRole(ownerTok, org, id); err != nil {
		t.Fatalf("delete: %v", err)
	}
	wantCode(t, e.deleteRole(ownerTok, org, id), connect.CodeNotFound)
	if es := e.audit(store.ActionRoleDelete); len(es) != 1 || es[0].Result != store.ResultAllowed {
		t.Fatalf("delete audit: %+v", es)
	}
}

func TestUpdateRoleParentCycle(t *testing.T) {
	e := newEnv(t)
	_, ownerTok := e.user("alice")
	org := e.createOrg(ownerTok, "Acme")
	a := e.createRole(ownerTok, org, &iamv1.CreateRoleRequest{Name: "a"})
	b := e.createRole(ownerTok, org, &iamv1.CreateRoleRequest{Name: "b", Parents: []string{a.String()}})
	c := e.createRole(ownerTok, org, &iamv1.CreateRoleRequest{Name: "c", Parents: []string{b.String()}})
	for _, parent := range []uuid.UUID{a, b, c} {
		wantCode(t, e.updateRole(ownerTok, org, a, &iamv1.UpdateRoleRequest{Name: "a", Parents: []string{parent.String()}}), connect.CodeFailedPrecondition)
	}
	if err := e.updateRole(ownerTok, org, c, &iamv1.UpdateRoleRequest{Name: "c", Parents: []string{a.String(), "member"}}); err != nil {
		t.Fatalf("non-cyclic: %v", err)
	}
}

func TestRoleParentFromOtherOrg(t *testing.T) {
	e := newEnv(t)
	_, ownerTok := e.user("alice")
	_, otherTok := e.user("mallory")
	org := e.createOrg(ownerTok, "Acme")
	orgB := e.createOrg(otherTok, "Evil")
	foreign := e.createRole(otherTok, orgB, &iamv1.CreateRoleRequest{Name: "x"})
	mine := e.createRole(ownerTok, org, &iamv1.CreateRoleRequest{Name: "mine"})

	_, err := e.tryCreateRole(ownerTok, org, &iamv1.CreateRoleRequest{Name: "y", Parents: []string{foreign.String()}})
	wantCode(t, err, connect.CodeInvalidArgument)
	if strings.Contains(err.Error(), orgB.String()) {
		t.Fatalf("leaks org: %v", err)
	}
	wantCode(t, e.updateRole(ownerTok, org, mine, &iamv1.UpdateRoleRequest{Name: "mine", Parents: []string{foreign.String()}}), connect.CodeInvalidArgument)
	wantCode(t, e.updateRole(ownerTok, org, foreign, &iamv1.UpdateRoleRequest{Name: "hijack"}), connect.CodeNotFound)
	wantCode(t, e.deleteRole(ownerTok, org, foreign), connect.CodeNotFound)
	if e.roleCount(org) != 1 || e.roleCount(orgB) != 1 {
		t.Fatal("roles changed")
	}
}

func TestCreateRoleEscalation(t *testing.T) {
	e := newEnv(t)
	_, ownerTok := e.user("alice")
	admin, adminTok := e.user("bob")
	org := e.createOrg(ownerTok, "Acme")
	e.addMember(org, admin, store.RoleAdmin)
	harmless := e.createRole(ownerTok, org, &iamv1.CreateRoleRequest{Name: "harmless"})
	promoter := e.createRole(ownerTok, org, &iamv1.CreateRoleRequest{Name: "promoter", Rules: []*iamv1.RoleRule{rule("allow", "org/members", "iam.members.setRole")}})

	before := e.denied()
	_, err := e.tryCreateRole(adminTok, org, &iamv1.CreateRoleRequest{Name: "esc", Rules: []*iamv1.RoleRule{rule("allow", "org/members", "iam.members.*")}})
	wantCode(t, err, connect.CodePermissionDenied)
	if !strings.Contains(err.Error(), "iam.members.setRole") {
		t.Fatalf("message: %v", err)
	}
	if e.roleCount(org) != 2 {
		t.Fatal("escalating role stored")
	}
	es := e.audit(store.ActionRoleCreate)
	if e.denied() != before+1 || es[0].Result != store.ResultDenied || es[0].ActorID != admin.ID.String() || es[0].Resource != "org/roles" || !strings.Contains(errorDetail(t, es[0]), "iam.members.setRole@org/members") {
		t.Fatalf("denied audit: %+v", es)
	}
	wantCode(t, e.updateRole(adminTok, org, harmless, &iamv1.UpdateRoleRequest{Name: "harmless", Rules: []*iamv1.RoleRule{rule("allow", "org/members", "iam.members.setRole")}}), connect.CodePermissionDenied)
	if es := e.audit(store.ActionRoleUpdate); len(es) != 1 || es[0].Result != store.ResultDenied || es[0].Resource != "org/roles/"+harmless.String() {
		t.Fatalf("update audit: %+v", es)
	}
	_, err = e.tryCreateRole(adminTok, org, &iamv1.CreateRoleRequest{Name: "via parent", Parents: []string{promoter.String()}})
	wantCode(t, err, connect.CodePermissionDenied)
	if _, err := e.tryCreateRole(adminTok, org, &iamv1.CreateRoleRequest{Name: "ok", Rules: []*iamv1.RoleRule{rule("allow", "org/teams/*", "iam.teams.*")}, Parents: []string{"admin"}}); err != nil {
		t.Fatalf("admin within own rights: %v", err)
	}
}

func TestUpdateRoleCarriesDeny(t *testing.T) {
	e := newEnv(t)
	_, ownerTok := e.user("alice")
	member, memberTok := e.user("bob")
	org := e.createOrg(ownerTok, "Acme")
	e.addMember(org, member, store.RoleMember)
	writer := e.createRole(ownerTok, org, &iamv1.CreateRoleRequest{Name: "writer", Rules: []*iamv1.RoleRule{rule("allow", "org/roles", "iam.roles.*")}})
	e.bind(ownerTok, org, authz.UserSubject(member.ID), writer)
	denying := e.createRole(ownerTok, org, &iamv1.CreateRoleRequest{Name: "denying", Rules: []*iamv1.RoleRule{rule("deny", "org/audit", "audit.events.list")}})

	_, err := e.tryCreateRole(memberTok, org, &iamv1.CreateRoleRequest{Name: "d", Rules: []*iamv1.RoleRule{rule("deny", "org/teams/*", "iam.teams.delete")}})
	wantCode(t, err, connect.CodePermissionDenied)
	if es := e.audit(store.ActionRoleCreate); es[0].Result != store.ResultDenied || es[0].ActorID != member.ID.String() {
		t.Fatalf("denied audit: %+v", es)
	}
	// Removing a deny is writing one too.
	before := e.denied()
	err = e.updateRole(memberTok, org, denying, &iamv1.UpdateRoleRequest{Name: "denying"})
	wantCode(t, err, connect.CodePermissionDenied)
	if err.(*connect.Error).Message() != authz.ErrDenyNotAllowed.Error() {
		t.Fatalf("message: %v", err)
	}
	if es := e.audit(store.ActionRoleUpdate); e.denied() != before+1 || es[0].Result != store.ResultDenied || !strings.Contains(errorDetail(t, es[0]), "deny audit.events.list@org/audit") {
		t.Fatalf("update audit: %+v", es)
	}
	wantCode(t, e.deleteRole(memberTok, org, denying), connect.CodePermissionDenied)
	if _, err := e.tryCreateRole(memberTok, org, &iamv1.CreateRoleRequest{Name: "reader", Rules: []*iamv1.RoleRule{rule("allow", "org/roles", "iam.roles.list")}}); err != nil {
		t.Fatalf("allow within rights: %v", err)
	}
}

func TestCreateRoleBindingAppliesImmediately(t *testing.T) {
	e := newEnv(t)
	_, ownerTok := e.user("alice")
	member, memberTok := e.user("bob")
	org := e.createOrg(ownerTok, "Acme")
	e.addMember(org, member, store.RoleMember)
	auditor := e.createRole(ownerTok, org, &iamv1.CreateRoleRequest{Name: "auditor", Rules: []*iamv1.RoleRule{rule("allow", "org/audit", "audit.events.list")}})
	ctx := context.Background()

	if e.canReadAudit(member.ID, org) {
		t.Fatal("member reads the audit trail before the bind")
	}
	e.takeNotified()
	binding := e.bind(ownerTok, org, authz.UserSubject(member.ID), auditor)
	if !e.canReadAudit(member.ID, org) {
		t.Fatal("bind not applied")
	}
	if n := e.takeNotified(); !slices.Equal(n, []uuid.UUID{org}) {
		t.Fatalf("notified %v", n)
	}
	list := must[iamv1.ListRoleBindingsResponse](t)(e.client.ListRoleBindings(ctx, req(ownerTok, &iamv1.ListRoleBindingsRequest{Org: org.String()}))).Bindings
	if len(list) != 1 || list[0].Id != binding.String() || list[0].Subject != authz.UserSubject(member.ID) || list[0].RoleId != auditor.String() || list[0].CreatedAt == nil {
		t.Fatalf("bindings: %+v", list)
	}
	if es := e.audit(store.ActionRoleBind); len(es) != 1 || es[0].Result != store.ResultAllowed || es[0].Details["subject"] != authz.UserSubject(member.ID) || es[0].Details["binding"] != binding.String() {
		t.Fatalf("bind audit: %+v", es)
	}
	_, err := e.client.ListRoleBindings(ctx, req(memberTok, &iamv1.ListRoleBindingsRequest{Org: org.String()}))
	wantCode(t, err, connect.CodePermissionDenied)

	if err := e.unbind(ownerTok, org, binding); err != nil {
		t.Fatalf("unbind: %v", err)
	}
	if e.canReadAudit(member.ID, org) {
		t.Fatal("unbind not applied")
	}
	wantCode(t, e.unbind(ownerTok, org, binding), connect.CodeNotFound)
	if es := e.audit(store.ActionRoleUnbind); len(es) != 1 || es[0].Result != store.ResultAllowed || es[0].Resource != "org/roles/"+auditor.String() {
		t.Fatalf("unbind audit: %+v", es)
	}

	team := e.createTeam(ownerTok, org, "core")
	if err := e.setTeamMember(ownerTok, org, team, member, false); err != nil {
		t.Fatal(err)
	}
	e.bind(ownerTok, org, authz.TeamSubject(team), auditor)
	if !e.canReadAudit(member.ID, org) {
		t.Fatal("team binding not applied")
	}
}

func TestCreateRoleBindingEscalation(t *testing.T) {
	e := newEnv(t)
	_, ownerTok := e.user("alice")
	admin, adminTok := e.user("bob")
	org := e.createOrg(ownerTok, "Acme")
	e.addMember(org, admin, store.RoleAdmin)
	promoter := e.createRole(ownerTok, org, &iamv1.CreateRoleRequest{Name: "promoter", Rules: []*iamv1.RoleRule{rule("allow", "org/members", "iam.members.setRole")}})

	_, err := e.tryBind(adminTok, org, authz.UserSubject(admin.ID), promoter.String())
	wantCode(t, err, connect.CodePermissionDenied)
	if !strings.Contains(err.Error(), "iam.members.setRole") {
		t.Fatalf("message: %v", err)
	}
	if e.bindingCount(org) != 0 {
		t.Fatal("binding stored")
	}
	es := e.audit(store.ActionRoleBind)
	if len(es) != 1 || es[0].Result != store.ResultDenied || es[0].ActorID != admin.ID.String() || !strings.Contains(errorDetail(t, es[0]), "iam.members.setRole") {
		t.Fatalf("denied audit: %+v", es)
	}
}

func TestCreateRoleBindingDenyNeedsAdmin(t *testing.T) {
	e := newEnv(t)
	_, ownerTok := e.user("alice")
	member, memberTok := e.user("bob")
	admin, adminTok := e.user("carol")
	org := e.createOrg(ownerTok, "Acme")
	e.addMember(org, member, store.RoleMember)
	e.addMember(org, admin, store.RoleAdmin)
	binder := e.createRole(ownerTok, org, &iamv1.CreateRoleRequest{Name: "binder", Rules: []*iamv1.RoleRule{rule("allow", "org/roles", "iam.roleBindings.manage")}})
	e.bind(ownerTok, org, authz.UserSubject(member.ID), binder)
	denying := e.createRole(ownerTok, org, &iamv1.CreateRoleRequest{Name: "denying", Rules: []*iamv1.RoleRule{rule("deny", "org/audit", "audit.events.list")}})

	before := e.denied()
	_, err := e.tryBind(memberTok, org, authz.UserSubject(admin.ID), denying.String())
	wantCode(t, err, connect.CodePermissionDenied)
	if es := e.audit(store.ActionRoleBind); e.denied() != before+1 || es[0].Result != store.ResultDenied || es[0].Resource != "org/roles/"+denying.String() ||
		!strings.Contains(errorDetail(t, es[0]), "deny audit.events.list@org/audit") {
		t.Fatalf("denied audit: %+v", es)
	}
	if !e.canReadAudit(admin.ID, org) {
		t.Fatal("deny bound")
	}
	if _, err := e.tryBind(adminTok, org, authz.UserSubject(member.ID), denying.String()); err != nil {
		t.Fatalf("org admin binds deny: %v", err)
	}
}

func TestDeleteRoleBindingOwnDenyGuard(t *testing.T) {
	e := newEnv(t)
	_, ownerTok := e.user("alice")
	admin, adminTok := e.user("bob")
	org := e.createOrg(ownerTok, "Acme")
	e.addMember(org, admin, store.RoleAdmin)
	team := e.createTeam(ownerTok, org, "core")
	noDelete := e.createRole(ownerTok, org, &iamv1.CreateRoleRequest{Name: "no delete", Rules: []*iamv1.RoleRule{rule("deny", "org/teams/*", "iam.teams.delete")}})
	wrapper := e.createRole(ownerTok, org, &iamv1.CreateRoleRequest{Name: "wrapper", Parents: []string{noDelete.String()}})
	binding := e.bind(ownerTok, org, authz.UserSubject(admin.ID), wrapper)

	wantCode(t, e.deleteTeam(adminTok, org, team), connect.CodePermissionDenied)
	// The admin may not lift a deny that binds them, by any route.
	before, deniedBefore := len(e.audit(store.ActionRoleUnbind)), e.denied()
	err := e.unbind(adminTok, org, binding)
	wantCode(t, err, connect.CodePermissionDenied)
	if err.(*connect.Error).Message() != errLiftsOwnDeny.Error() {
		t.Fatalf("message: %v", err)
	}
	if es := e.audit(store.ActionRoleUnbind); len(es) != before+1 || e.denied() != deniedBefore+1 || es[0].Result != store.ResultDenied || es[0].ActorID != admin.ID.String() ||
		!strings.Contains(errorDetail(t, es[0]), "deny iam.teams.delete@org/teams/*") {
		t.Fatalf("unbind denied audit: %+v", es)
	}
	wantCode(t, e.updateRole(adminTok, org, noDelete, &iamv1.UpdateRoleRequest{Name: "no delete"}), connect.CodePermissionDenied)
	wantCode(t, e.updateRole(adminTok, org, wrapper, &iamv1.UpdateRoleRequest{Name: "wrapper"}), connect.CodePermissionDenied)
	wantCode(t, e.deleteRole(adminTok, org, noDelete), connect.CodePermissionDenied)
	if e.bindingCount(org) != 1 || e.roleCount(org) != 2 {
		t.Fatal("policy changed")
	}
	if err := e.unbind(ownerTok, org, binding); err != nil {
		t.Fatalf("owner unbinds: %v", err)
	}
	if err := e.deleteTeam(adminTok, org, team); err != nil {
		t.Fatalf("team delete after unbind: %v", err)
	}
}

func TestCreateRoleBindingForeignSubject(t *testing.T) {
	e := newEnv(t)
	_, ownerTok := e.user("alice")
	stranger, strangerTok := e.user("mallory")
	org := e.createOrg(ownerTok, "Acme")
	orgB := e.createOrg(strangerTok, "Evil")
	foreignTeam := e.createTeam(strangerTok, orgB, "x")
	foreignRole := e.createRole(strangerTok, orgB, &iamv1.CreateRoleRequest{Name: "x"})
	role := e.createRole(ownerTok, org, &iamv1.CreateRoleRequest{Name: "r"})

	for _, c := range [][2]string{
		{authz.UserSubject(stranger.ID), role.String()},
		{authz.TeamSubject(foreignTeam), role.String()},
		{"user:" + strings.ToUpper(stranger.ID.String()), role.String()},
		{"bogus", role.String()},
		{authz.TeamSubject(foreignTeam), foreignRole.String()},
		{authz.UserSubject(stranger.ID), "nope"},
	} {
		_, err := e.tryBind(ownerTok, org, c[0], c[1])
		wantCode(t, err, connect.CodeInvalidArgument)
		if strings.Contains(err.Error(), orgB.String()) {
			t.Fatalf("%v leaks org: %v", c, err)
		}
	}
	if e.bindingCount(org) != 0 || e.bindingCount(orgB) != 0 {
		t.Fatal("binding stored")
	}
	team := e.createTeam(ownerTok, org, "core")
	e.bind(ownerTok, org, authz.TeamSubject(team), role)
	_, err := e.tryBind(ownerTok, org, authz.TeamSubject(team), role.String())
	wantCode(t, err, connect.CodeFailedPrecondition)
}

func TestCreateRoleWildcardDenyKept(t *testing.T) {
	e := newEnv(t)
	_, ownerTok := e.user("alice")
	admin, adminTok := e.user("bob")
	org := e.createOrg(ownerTok, "Acme")
	e.addMember(org, admin, store.RoleAdmin)
	team := e.createTeam(ownerTok, org, "core")
	created, err := e.tryCreateRole(ownerTok, org, &iamv1.CreateRoleRequest{Name: "no teams", Rules: []*iamv1.RoleRule{rule("deny", "org/teams/*", "iam.teams.*"), rule("allow", "org/audit", "audit.*")}})
	if err != nil || len(created.Rules) != 4 || created.Rules[0].Action != "iam.teams.*" || created.Rules[1].Action != "audit.events.list" || created.Rules[3].Action != "audit.settings.update" {
		t.Fatalf("create: %+v %v", created, err)
	}
	e.bind(ownerTok, org, authz.UserSubject(admin.ID), uuid.MustParse(created.Id))
	wantCode(t, e.renameTeam(adminTok, org, team, "x"), connect.CodePermissionDenied)
	wantCode(t, e.deleteTeam(adminTok, org, team), connect.CodePermissionDenied)
}

func TestSetTeamMemberEscalation(t *testing.T) {
	e := newEnv(t)
	_, ownerTok := e.user("alice")
	admin, adminTok := e.user("bob")
	member, _ := e.user("carol")
	lead, leadTok := e.user("dave")
	org := e.createOrg(ownerTok, "Acme")
	e.addMember(org, admin, store.RoleAdmin)
	e.addMember(org, member, store.RoleMember)
	e.addMember(org, lead, store.RoleMember)
	promoter := e.createRole(ownerTok, org, &iamv1.CreateRoleRequest{Name: "promoter", Rules: []*iamv1.RoleRule{rule("allow", "org/members", "iam.members.setRole")}})
	elevated := e.createTeam(ownerTok, org, "elevated")
	e.bind(ownerTok, org, authz.TeamSubject(elevated), promoter)

	for _, u := range []store.User{admin, member} {
		wantCode(t, e.setTeamMember(adminTok, org, elevated, u, false), connect.CodePermissionDenied)
	}
	es := e.audit(store.ActionTeamMemberSet)
	if len(es) != 2 || es[0].Result != store.ResultDenied || es[0].ActorID != admin.ID.String() || !strings.Contains(errorDetail(t, es[0]), "iam.members.setRole@org/members") {
		t.Fatalf("denied audit: %+v", es)
	}
	if ms, _ := e.store.TeamMembers(t.Context(), org, elevated); len(ms) != 0 {
		t.Fatalf("members added: %+v", ms)
	}
	if err := e.setTeamMember(ownerTok, org, elevated, member, false); err != nil {
		t.Fatalf("owner adds: %v", err)
	}

	denying := e.createRole(ownerTok, org, &iamv1.CreateRoleRequest{Name: "denying", Rules: []*iamv1.RoleRule{rule("deny", "org/audit", "audit.events.list")}})
	restricted := e.createTeam(ownerTok, org, "restricted")
	e.bind(ownerTok, org, authz.TeamSubject(restricted), denying)
	if err := e.setTeamMember(ownerTok, org, restricted, lead, true); err != nil {
		t.Fatal(err)
	}
	wantCode(t, e.setTeamMember(leadTok, org, restricted, member, false), connect.CodePermissionDenied)
	if err := e.setTeamMember(adminTok, org, restricted, member, false); err != nil {
		t.Fatalf("org admin adds to deny team: %v", err)
	}
	wantCode(t, e.removeTeamMember(leadTok, org, restricted, member), connect.CodePermissionDenied)
}

func TestLeaveTeamOwnDenyGuard(t *testing.T) {
	e := newEnv(t)
	_, ownerTok := e.user("alice")
	admin, adminTok := e.user("bob")
	member, memberTok := e.user("carol")
	org := e.createOrg(ownerTok, "Acme")
	e.addMember(org, admin, store.RoleAdmin)
	e.addMember(org, member, store.RoleMember)
	denying := e.createRole(ownerTok, org, &iamv1.CreateRoleRequest{Name: "no audit", Rules: []*iamv1.RoleRule{rule("deny", "org/audit", "audit.events.list")}})
	team := e.createTeam(ownerTok, org, "restricted")
	e.bind(ownerTok, org, authz.TeamSubject(team), denying)
	if err := e.setTeamMember(ownerTok, org, team, admin, false); err != nil {
		t.Fatal(err)
	}
	if e.canReadAudit(admin.ID, org) {
		t.Fatal("deny not applied")
	}

	before := e.denied()
	wantCode(t, e.leaveTeam(adminTok, org, team), connect.CodePermissionDenied)
	if es := e.audit(store.ActionTeamLeave); len(es) != 1 || e.denied() != before+1 || es[0].Result != store.ResultDenied || !strings.Contains(errorDetail(t, es[0]), "deny audit.events.list@org/audit") {
		t.Fatalf("leave audit: %+v", es)
	}
	wantCode(t, e.deleteTeam(adminTok, org, team), connect.CodePermissionDenied)
	if es := e.audit(store.ActionTeamDelete); len(es) == 0 || es[0].Result != store.ResultDenied || es[0].ActorID != admin.ID.String() {
		t.Fatalf("delete audit: %+v", es)
	}
	if e.canReadAudit(admin.ID, org) {
		t.Fatal("deny lifted")
	}

	deleter := e.createRole(ownerTok, org, &iamv1.CreateRoleRequest{Name: "deleter", Rules: []*iamv1.RoleRule{rule("allow", "org/teams/*", "iam.teams.delete")}})
	e.bind(ownerTok, org, authz.UserSubject(member.ID), deleter)
	wantCode(t, e.deleteTeam(memberTok, org, team), connect.CodePermissionDenied)
	plain := e.createTeam(ownerTok, org, "plain")
	if err := e.deleteTeam(memberTok, org, plain); err != nil {
		t.Fatalf("member deletes plain team: %v", err)
	}
	if err := e.deleteTeam(ownerTok, org, team); err != nil {
		t.Fatalf("owner deletes: %v", err)
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
	writer := e.createRole(ownerTok, org, &iamv1.CreateRoleRequest{Name: "writer", Rules: []*iamv1.RoleRule{rule("allow", "org/roles", "iam.roles.*"), rule("allow", "org/roles", "iam.roleBindings.manage")}})
	e.bind(ownerTok, org, authz.UserSubject(member.ID), writer)
	target := e.createRole(ownerTok, org, &iamv1.CreateRoleRequest{Name: "target"})

	e.exec(`INSERT INTO role_rules (role_id, position, effect, object, action) VALUES ($1, 0, 'deny', 'org/audit', 'audit.events.list')`, target)
	wantCode(t, e.updateRole(memberTok, org, target, &iamv1.UpdateRoleRequest{Name: "target"}), connect.CodePermissionDenied)
	if es := e.audit(store.ActionRoleUpdate); len(es) != 1 || es[0].Result != store.ResultDenied || es[0].ActorID != member.ID.String() {
		t.Fatalf("update audit: %+v", es)
	}
	wantCode(t, e.deleteRole(memberTok, org, target), connect.CodePermissionDenied)
	_, err := e.tryBind(memberTok, org, authz.UserSubject(member.ID), target.String())
	wantCode(t, err, connect.CodePermissionDenied)
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
	if _, err := e.tryBind(ownerTok, org, authz.UserSubject(member.ID), fresh.String()); err != nil {
		t.Fatalf("bind unreloaded role: %v", err)
	}
}

func TestUpdateRoleDenyParent(t *testing.T) {
	e := newEnv(t)
	_, ownerTok := e.user("alice")
	admin, _ := e.user("bob")
	member, memberTok := e.user("carol")
	org := e.createOrg(ownerTok, "Acme")
	e.addMember(org, admin, store.RoleAdmin)
	e.addMember(org, member, store.RoleMember)
	writer := e.createRole(ownerTok, org, &iamv1.CreateRoleRequest{Name: "writer", Rules: []*iamv1.RoleRule{rule("allow", "org/roles", "iam.roles.*")}})
	e.bind(ownerTok, org, authz.UserSubject(member.ID), writer)
	harmless := e.createRole(ownerTok, org, &iamv1.CreateRoleRequest{Name: "harmless"})
	e.bind(ownerTok, org, authz.UserSubject(admin.ID), harmless)
	denying := e.createRole(ownerTok, org, &iamv1.CreateRoleRequest{Name: "denying", Rules: []*iamv1.RoleRule{rule("deny", "org/audit", "audit.events.list")}})
	wrapper := e.createRole(ownerTok, org, &iamv1.CreateRoleRequest{Name: "wrapper", Parents: []string{denying.String()}})

	for _, parent := range []uuid.UUID{denying, wrapper} {
		wantCode(t, e.updateRole(memberTok, org, harmless, &iamv1.UpdateRoleRequest{Name: "harmless", Parents: []string{parent.String()}}), connect.CodePermissionDenied)
		_, err := e.tryCreateRole(memberTok, org, &iamv1.CreateRoleRequest{Name: "new", Parents: []string{parent.String()}})
		wantCode(t, err, connect.CodePermissionDenied)
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

// UpdateRole replaces the whole role, so REST serves it as PUT, not PATCH.
func TestRESTUpdateRoleIsPut(t *testing.T) {
	e := newEnv(t)
	_, tok := e.user("alice")
	org := e.createOrg(tok, "Acme")
	role := e.createRole(tok, org, &iamv1.CreateRoleRequest{Name: "ops", Rules: []*iamv1.RoleRule{rule("allow", "org/roles", "iam.roles.list")}})
	path := "/api/iam/v1/orgs/" + org.String() + "/roles/" + role.String()
	body := `{"name":"ops2","rules":[{"effect":"allow","object":"org/roles","action":"iam.roles.list"}]}`
	if code, msg := e.restSend("PUT", path, tok, body); code != 200 {
		t.Fatalf("PUT: %d %s", code, msg)
	}
	if code, _ := e.restSend("PATCH", path, tok, body); code == 200 {
		t.Fatal("PATCH still updates the role")
	}
}

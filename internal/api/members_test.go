package api

import (
	"context"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/asterism-dev/observatory/internal/authz"
	"github.com/asterism-dev/observatory/internal/store"
)

func (e *env) role(org uuid.UUID, u store.User) store.Role {
	e.t.Helper()
	m, err := e.store.Membership(context.Background(), org, u.ID)
	if err != nil {
		return ""
	}
	return m.Role
}

func memberPath(org uuid.UUID, u store.User) string {
	return "/api/orgs/" + org.String() + "/members/" + u.ID.String()
}

func TestAdminCannotPromote(t *testing.T) {
	e := newEnv(t)
	_, ownerTok := e.user("alice")
	admin, adminTok := e.user("bob")
	member, _ := e.user("carol")
	org := e.createOrg(ownerTok, "Acme")
	e.addMember(org, admin, store.RoleAdmin)
	e.addMember(org, member, store.RoleMember)

	if rec := e.send("PATCH", memberPath(org, member), adminTok, `{"role":"admin"}`); rec.Code != 403 {
		t.Fatalf("promote: %d %s", rec.Code, rec.Body.String())
	}
	es := e.audit(store.ActionMemberSetRole)
	if len(es) != 1 || es[0].Result != store.ResultDenied || es[0].ActorID != admin.ID.String() || *es[0].OrgID != org || es[0].Resource != "org/members" {
		t.Fatalf("denied audit: %+v", es)
	}
	if e.role(org, member) != store.RoleMember {
		t.Fatal("role changed")
	}
}

func TestOwnerPromotes(t *testing.T) {
	e := newEnv(t)
	owner, ownerTok := e.user("alice")
	member, memberTok := e.user("bob")
	org := e.createOrg(ownerTok, "Acme")
	e.addMember(org, member, store.RoleMember)

	rec := e.do("GET", "/api/orgs/"+org.String()+"/members", memberTok)
	var list []struct{ ID, Email, Name, Role, JoinedAt string }
	decode(t, rec, &list)
	if rec.Code != 200 || len(list) != 2 || list[0].ID != owner.ID.String() || list[0].Email != "alice@x.io" || list[0].Name != "alice" || list[0].Role != "owner" || list[0].JoinedAt == "" {
		t.Fatalf("list: %d %s", rec.Code, rec.Body.String())
	}

	for _, body := range []string{`{"role":"boss"}`, `{}`, `nope`} {
		if rec := e.send("PATCH", memberPath(org, member), ownerTok, body); rec.Code != 400 {
			t.Fatalf("%s: %d", body, rec.Code)
		}
	}
	if rec := e.send("PATCH", memberPath(org, member), ownerTok, `{"role":"owner"}`); rec.Code != 204 {
		t.Fatalf("promote: %d %s", rec.Code, rec.Body.String())
	}
	es := e.audit(store.ActionMemberSetRole)
	if len(es) != 1 || es[0].Result != store.ResultAllowed || es[0].ActorID != owner.ID.String() || es[0].Resource != "org/members/"+member.ID.String() || es[0].Details["role"] != "owner" {
		t.Fatalf("audit: %+v", es)
	}
	// The new owner can use owner rights right away; the last remaining owner can't step down.
	if rec := e.send("PATCH", memberPath(org, owner), memberTok, `{"role":"member"}`); rec.Code != 204 {
		t.Fatalf("demote: %d %s", rec.Code, rec.Body.String())
	}
	if rec := e.send("PATCH", memberPath(org, member), memberTok, `{"role":"admin"}`); rec.Code != 409 {
		t.Fatalf("last owner: %d %s", rec.Code, rec.Body.String())
	}
	if e.role(org, member) != store.RoleOwner || e.role(org, owner) != store.RoleMember {
		t.Fatal("roles")
	}
}

func TestAdminCannotRemoveOwner(t *testing.T) {
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
		if rec := e.do("DELETE", memberPath(org, target), adminTok); rec.Code != 403 {
			t.Fatalf("remove %s: %d", target.Name, rec.Code)
		}
	}
	if es := e.audit(store.ActionMemberSetRole); len(es) != 2 || es[0].Result != store.ResultDenied || es[0].ActorID != admin.ID.String() {
		t.Fatalf("denied audit: %+v", es)
	}
	if rec := e.do("DELETE", memberPath(org, admin), memberTok); rec.Code != 403 {
		t.Fatalf("member removes admin: %d", rec.Code)
	}
	if e.role(org, owner) != store.RoleOwner || e.role(org, admin2) != store.RoleAdmin || e.role(org, admin) != store.RoleAdmin {
		t.Fatal("member removed")
	}

	if rec := e.do("DELETE", memberPath(org, member), adminTok); rec.Code != 204 {
		t.Fatalf("remove member: %d %s", rec.Code, rec.Body.String())
	}
	es := e.audit(store.ActionMemberRemove)
	if len(es) != 2 || es[0].Result != store.ResultAllowed || es[0].Resource != "org/members/"+member.ID.String() || es[1].Result != store.ResultDenied {
		t.Fatalf("audit: %+v", es)
	}
	if e.role(org, member) != "" {
		t.Fatal("still a member")
	}
	if rec := e.do("GET", "/api/orgs/"+org.String(), memberTok); rec.Code != 404 {
		t.Fatalf("removed member still has access: %d", rec.Code)
	}
	if rec := e.do("DELETE", memberPath(org, admin2), ownerTok); rec.Code != 204 {
		t.Fatalf("owner removes admin: %d", rec.Code)
	}
}

func TestLeaveOrg(t *testing.T) {
	e := newEnv(t)
	owner, ownerTok := e.user("alice")
	member, memberTok := e.user("bob")
	org := e.createOrg(ownerTok, "Acme")
	e.addMember(org, member, store.RoleMember)

	if rec := e.do("DELETE", memberPath(org, member), memberTok); rec.Code != 204 {
		t.Fatalf("leave: %d %s", rec.Code, rec.Body.String())
	}
	es := e.audit(store.ActionMemberLeave)
	if len(es) != 1 || es[0].ActorID != member.ID.String() || *es[0].OrgID != org || es[0].Result != store.ResultAllowed {
		t.Fatalf("audit: %+v", es)
	}
	if rec := e.do("GET", "/api/orgs/"+org.String(), memberTok); rec.Code != 404 {
		t.Fatalf("after leave: %d", rec.Code)
	}
	if rec := e.do("DELETE", memberPath(org, owner), ownerTok); rec.Code != 409 {
		t.Fatalf("last owner leaves: %d", rec.Code)
	}
	if rec := e.do("DELETE", memberPath(e.personalOrg(owner), owner), ownerTok); rec.Code != 409 {
		t.Fatalf("leave personal: %d", rec.Code)
	}
	if e.role(org, owner) != store.RoleOwner {
		t.Fatal("owner left")
	}
}

func TestInviteAsAdminRequiresOwner(t *testing.T) {
	e := newEnv(t)
	_, ownerTok := e.user("alice")
	admin, adminTok := e.user("bob")
	member, memberTok := e.user("carol")
	org := e.createOrg(ownerTok, "Acme")
	e.addMember(org, admin, store.RoleAdmin)
	e.addMember(org, member, store.RoleMember)
	invites := "/api/orgs/" + org.String() + "/invites"

	for _, role := range []string{"admin", "owner"} {
		if rec := e.send("POST", invites, adminTok, `{"role":"`+role+`"}`); rec.Code != 403 {
			t.Fatalf("admin invites %s: %d", role, rec.Code)
		}
	}
	if es := e.audit(store.ActionMemberSetRole); len(es) != 2 || es[0].Result != store.ResultDenied {
		t.Fatalf("denied audit: %+v", es)
	}
	if rec := e.send("POST", invites, memberTok, `{"role":"member"}`); rec.Code != 403 {
		t.Fatalf("member invites: %d", rec.Code)
	}
	if rec := e.send("POST", invites, adminTok, `{"role":"boss"}`); rec.Code != 400 {
		t.Fatalf("bad role: %d", rec.Code)
	}

	var created struct{ ID, Token, URL, ExpiresAt string }
	rec := e.send("POST", invites, adminTok, `{"role":"member"}`)
	decode(t, rec, &created)
	if rec.Code != 201 || !strings.HasPrefix(created.Token, "obsinv_") || created.URL != "https://hub.test/invite/"+created.Token || created.ExpiresAt == "" {
		t.Fatalf("create: %d %s", rec.Code, rec.Body.String())
	}
	if rec := e.send("POST", invites, ownerTok, `{"role":"admin"}`); rec.Code != 201 {
		t.Fatalf("owner invites admin: %d", rec.Code)
	}
	es := e.audit(store.ActionMemberInvite)
	if len(es) != 3 || es[1].Result != store.ResultAllowed || es[1].Resource != "org/invites/"+created.ID || es[1].Details["role"] != "member" {
		t.Fatalf("audit: %+v", es)
	}

	rec = e.do("GET", invites, adminTok)
	if rec.Code != 200 || strings.Contains(rec.Body.String(), created.Token) || strings.Contains(rec.Body.String(), "token") || !strings.Contains(rec.Body.String(), created.ID) {
		t.Fatalf("list: %d %s", rec.Code, rec.Body.String())
	}
	if rec := e.do("GET", invites, memberTok); rec.Code != 403 {
		t.Fatalf("member lists: %d", rec.Code)
	}

	if rec := e.do("DELETE", invites+"/"+created.ID, adminTok); rec.Code != 204 {
		t.Fatalf("revoke: %d %s", rec.Code, rec.Body.String())
	}
	if es := e.audit(store.ActionInviteRevoke); len(es) != 1 || es[0].Resource != "org/invites/"+created.ID {
		t.Fatalf("revoke audit: %+v", es)
	}
	if rec := e.do("DELETE", invites+"/"+created.ID, adminTok); rec.Code != 404 {
		t.Fatalf("revoke twice: %d", rec.Code)
	}
	var list []struct{ ID, Role string }
	decode(t, e.do("GET", invites, adminTok), &list)
	if len(list) != 1 || list[0].Role != "admin" {
		t.Fatalf("after revoke: %+v", list)
	}
}

func TestAcceptInvite(t *testing.T) {
	e := newEnv(t)
	owner, ownerTok := e.user("alice")
	joiner, joinerTok := e.user("bob")
	_, otherTok := e.user("carol")
	org := e.createOrg(ownerTok, "Acme")

	if rec := e.send("POST", "/api/orgs/"+e.personalOrg(owner).String()+"/invites", ownerTok, `{"role":"member"}`); rec.Code != 409 {
		t.Fatalf("personal invite: %d", rec.Code)
	}
	var created struct{ Token string }
	decode(t, e.send("POST", "/api/orgs/"+org.String()+"/invites", ownerTok, `{"role":"admin"}`), &created)
	body := `{"token":"` + created.Token + `"}`

	if rec := e.send("POST", "/api/invites/accept", ownerTok, body); rec.Code != 409 {
		t.Fatalf("already member: %d", rec.Code)
	}
	rec := e.send("POST", "/api/invites/accept", joinerTok, body)
	var got struct{ OrgID, Role string }
	decode(t, rec, &got)
	if rec.Code != 200 || got.OrgID != org.String() || got.Role != "admin" {
		t.Fatalf("accept: %d %s", rec.Code, rec.Body.String())
	}
	if rec := e.do("GET", "/api/orgs/"+org.String()+"/invites", joinerTok); rec.Code != 200 {
		t.Fatalf("new admin can't list invites: %d", rec.Code)
	}

	for _, b := range []string{body, `{"token":"obsinv_nope"}`, `{}`} {
		if rec := e.send("POST", "/api/invites/accept", otherTok, b); rec.Code != 400 {
			t.Fatalf("%s: %d", b, rec.Code)
		}
	}
	if rec := e.send("POST", "/api/invites/accept", otherTok, `nope`); rec.Code != 400 {
		t.Fatalf("bad body: %d", rec.Code)
	}

	es := e.audit(store.ActionInviteAccept)
	var allowed, failed int
	for _, en := range es {
		if strings.Contains(en.Resource, "obsinv_") || strings.Contains(strings.Join(mapValues(en.Details), " "), "obsinv_") {
			t.Fatalf("token in audit: %+v", en)
		}
		switch en.Result {
		case store.ResultAllowed:
			allowed++
			if en.ActorID != joiner.ID.String() || en.OrgID == nil || *en.OrgID != org || en.Resource != "org/members" {
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
		t.Fatalf("audit: %+v", es)
	}
}

func mapValues(m map[string]any) []string {
	var out []string
	for _, v := range m {
		if s, ok := v.(string); ok {
			out = append(out, s)
		}
	}
	return out
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

	if rec := e.do("DELETE", memberPath(orgA, victim), adminATok); rec.Code != 404 {
		t.Fatalf("remove: %d", rec.Code)
	}
	if rec := e.send("PATCH", memberPath(orgA, victim), aTok, `{"role":"admin"}`); rec.Code != 404 {
		t.Fatalf("set role: %d", rec.Code)
	}
	var invite struct{ ID string }
	decode(t, e.send("POST", "/api/orgs/"+orgB.String()+"/invites", bTok, `{"role":"member"}`), &invite)
	if rec := e.do("DELETE", "/api/orgs/"+orgA.String()+"/invites/"+invite.ID, adminATok); rec.Code != 404 {
		t.Fatalf("revoke: %d", rec.Code)
	}
	for _, path := range []string{"/members", "/invites"} {
		if rec := e.do("GET", "/api/orgs/"+orgB.String()+path, adminATok); rec.Code != 404 {
			t.Fatalf("%s: %d", path, rec.Code)
		}
	}
	if e.role(orgB, victim) != store.RoleMember {
		t.Fatal("victim changed")
	}
	var list []struct{ ID string }
	decode(t, e.do("GET", "/api/orgs/"+orgB.String()+"/invites", bTok), &list)
	if len(list) != 1 || list[0].ID != invite.ID {
		t.Fatalf("invite in B: %+v", list)
	}
}

func (e *env) invite(org uuid.UUID, token, role string) string {
	e.t.Helper()
	rec := e.send("POST", "/api/orgs/"+org.String()+"/invites", token, `{"role":"`+role+`"}`)
	if rec.Code != 201 {
		e.t.Fatalf("invite: %d %s", rec.Code, rec.Body.String())
	}
	var got struct{ Token string }
	decode(e.t, rec, &got)
	return got.Token
}

func (e *env) inviteCount(org uuid.UUID) int {
	e.t.Helper()
	invs, err := e.store.Invites(context.Background(), org)
	if err != nil {
		e.t.Fatal(err)
	}
	return len(invs)
}

func TestInviteNeedsCreatorStillAllowed(t *testing.T) {
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
	accept := func(token string) int {
		return e.send("POST", "/api/invites/accept", joinerTok, `{"token":"`+token+`"}`).Code
	}

	// Removed admin.
	adminInvite := e.invite(org, adminTok, "member")
	if rec := e.do("DELETE", memberPath(org, admin), ownerTok); rec.Code != 204 {
		t.Fatal(rec.Code)
	}
	if code := accept(adminInvite); code != 400 {
		t.Fatalf("removed creator: %d", code)
	}

	// Owner demoted to member: their admin invite no longer works.
	ownerInvite := e.invite(org, owner2Tok, "admin")
	if rec := e.send("PATCH", memberPath(org, owner2), ownerTok, `{"role":"member"}`); rec.Code != 204 {
		t.Fatal(rec.Code)
	}
	if code := accept(ownerInvite); code != 400 {
		t.Fatalf("demoted creator: %d", code)
	}

	// Member who could invite through a custom role that is then unbound.
	var role uuid.UUID
	if err := e.pool.QueryRow(context.Background(), `INSERT INTO roles (org_id, name) VALUES ($1, 'inviter') RETURNING id`, org).Scan(&role); err != nil {
		t.Fatal(err)
	}
	e.exec(`INSERT INTO role_rules (role_id, position, effect, object, action) VALUES ($1, 0, 'allow', 'org/members', 'member:invite')`, role)
	e.exec(`INSERT INTO role_bindings (org_id, subject, role_id) VALUES ($1, $2, $3)`, org, "user:"+inviter.ID.String(), role)
	e.reload()
	boundInvite := e.invite(org, inviterTok, "member")
	e.exec(`DELETE FROM role_bindings WHERE role_id = $1`, role)
	e.reload()
	if code := accept(boundInvite); code != 400 {
		t.Fatalf("unbound creator: %d", code)
	}

	// None of the rejected invites were consumed, and nobody joined.
	if n := e.inviteCount(org); n != 3 {
		t.Fatalf("invites left: %d", n)
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

	// A creator who still holds the rights: the invite works.
	if code := accept(e.invite(org, ownerTok, "admin")); code != 200 {
		t.Fatalf("valid creator: %d", code)
	}
}

// Removing a member drops their bindings; a re-invite would come back deny-free, so only owners/admins may do it.
func TestRemoveMemberCarryingDeny(t *testing.T) {
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
	removerRole := e.createRole(ownerTok, org, `{"name":"remover","rules":[{"effect":"allow","object":"org/members","action":"member:remove"}]}`)
	e.bind(ownerTok, org, authz.UserSubject(remover.ID), removerRole)
	denying := e.createRole(ownerTok, org, `{"name":"denying","rules":[{"effect":"deny","object":"org/audit","action":"audit:read"}]}`)
	e.bind(ownerTok, org, authz.UserSubject(restricted.ID), denying)

	if rec := e.do("DELETE", memberPath(org, restricted), removerTok); rec.Code != 403 {
		t.Fatalf("remove restricted: %d %s", rec.Code, rec.Body.String())
	}
	es := e.audit(store.ActionMemberRemove)
	if len(es) != 1 || es[0].Result != store.ResultDenied || es[0].ActorID != remover.ID.String() || es[0].Resource != "org/members/"+restricted.ID.String() {
		t.Fatalf("denied audit: %+v", es)
	}
	if e.role(org, restricted) != store.RoleMember || e.bindingCount(org) != 2 {
		t.Fatal("restricted member removed")
	}
	if rec := e.do("DELETE", memberPath(org, plain), removerTok); rec.Code != 204 {
		t.Fatalf("remove plain: %d %s", rec.Code, rec.Body.String())
	}
	if rec := e.do("DELETE", memberPath(org, restricted), adminTok); rec.Code != 204 {
		t.Fatalf("admin removes restricted: %d %s", rec.Code, rec.Body.String())
	}
}

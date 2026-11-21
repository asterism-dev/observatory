package api

import (
	"context"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/asterism-dev/observatory/internal/store"
)

func (e *env) exec(sql string, args ...any) {
	e.t.Helper()
	if _, err := e.pool.Exec(context.Background(), sql, args...); err != nil {
		e.t.Fatal(err)
	}
}

func (e *env) reload() {
	e.t.Helper()
	if err := e.z.Reload(context.Background()); err != nil {
		e.t.Fatal(err)
	}
}

// createOrg creates an org owned by token's user, as iam's CreateOrg would.
func (e *env) createOrg(token, name string) uuid.UUID {
	e.t.Helper()
	sess, err := e.store.SessionByToken(context.Background(), token)
	if err != nil {
		e.t.Fatal(err)
	}
	o, err := e.store.CreateOrg(context.Background(), name, sess.UserID)
	if err != nil {
		e.t.Fatal(err)
	}
	e.reload()
	return o.ID
}

func (e *env) addMember(org uuid.UUID, u store.User, role store.Role) {
	e.t.Helper()
	e.exec(`INSERT INTO memberships (org_id, user_id, role) VALUES ($1, $2, $3)`, org, u.ID, role)
	e.reload()
}

func (e *env) personalOrg(u store.User) uuid.UUID {
	e.t.Helper()
	ms, err := e.store.Memberships(context.Background(), u.ID)
	if err != nil || len(ms) == 0 || !ms[0].Org.Personal {
		e.t.Fatalf("personal org: %+v %v", ms, err)
	}
	return ms[0].Org.ID
}

// iamPath is a REST path of the iam API, which newEnv mounts next to the hub's routes.
func iamPath(org uuid.UUID, rest string) string { return "/api/iam/v1/orgs/" + org.String() + rest }

func memberPath(org uuid.UUID, u store.User) string { return iamPath(org, "/members/"+u.ID.String()) }

func teamPath(org, team uuid.UUID) string { return iamPath(org, "/teams/"+team.String()) }

func teamMemberPath(org, team uuid.UUID, u store.User) string {
	return teamPath(org, team) + "/members/" + u.ID.String()
}

// iamCreate posts body to the iam API and returns the id of the created entity, the response's only field.
func (e *env) iamCreate(tok, path, body string) uuid.UUID {
	e.t.Helper()
	rec := e.send("POST", path, tok, body)
	if rec.Code != 200 {
		e.t.Fatalf("POST %s: %d %s", path, rec.Code, rec.Body.String())
	}
	var got map[string]struct{ ID uuid.UUID }
	decode(e.t, rec, &got)
	for _, v := range got {
		return v.ID
	}
	e.t.Fatalf("POST %s: %s", path, rec.Body.String())
	return uuid.Nil
}

func (e *env) createTeam(tok string, org uuid.UUID, name string) uuid.UUID {
	return e.iamCreate(tok, iamPath(org, "/teams"), `{"name":"`+name+`"}`)
}

func (e *env) createRole(tok string, org uuid.UUID, body string) uuid.UUID {
	return e.iamCreate(tok, iamPath(org, "/roles"), body)
}

func (e *env) bind(tok string, org uuid.UUID, subject string, role uuid.UUID) uuid.UUID {
	return e.iamCreate(tok, iamPath(org, "/roleBindings"), `{"subject":"`+subject+`","roleId":"`+role.String()+`"}`)
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

func TestPermissionChangeIsImmediate(t *testing.T) {
	e := newEnv(t)
	_, ownerTok := e.user("alice")
	member, _ := e.user("bob")
	org := e.createOrg(ownerTok, "Acme")
	e.addMember(org, member, store.RoleMember)
	if e.canReadAudit(member.ID, org) {
		t.Fatal("member reads the audit trail before the grant")
	}
	var role uuid.UUID
	if err := e.pool.QueryRow(context.Background(), `INSERT INTO roles (org_id, name) VALUES ($1, 'auditor') RETURNING id`, org).Scan(&role); err != nil {
		t.Fatal(err)
	}
	e.exec(`INSERT INTO role_rules (role_id, position, effect, object, action) VALUES ($1, 0, 'allow', 'org/audit', 'audit.events.list')`, role)
	e.exec(`INSERT INTO role_bindings (org_id, subject, role_id) VALUES ($1, $2, $3)`, org, "user:"+member.ID.String(), role)
	e.reload()
	if !e.canReadAudit(member.ID, org) {
		t.Fatal("grant not applied")
	}
}

func TestCommitRejectsInvalidPolicy(t *testing.T) {
	e := newEnv(t)
	u, tok := e.user("alice")
	org := e.createOrg(tok, "Acme")
	var role uuid.UUID
	if err := e.pool.QueryRow(context.Background(), `INSERT INTO roles (org_id, name) VALUES ($1, 'r') RETURNING id`, org).Scan(&role); err != nil {
		t.Fatal(err)
	}
	e.reload()
	before := e.z.Enforcer()
	// A poison row committed behind the API's back: any write that would commit it must fail.
	e.exec(`INSERT INTO role_bindings (org_id, subject, role_id) VALUES ($1, 'bogus', $2)`, org, role)

	h := &api{store: e.store, z: e.z}
	rec := httptest.NewRecorder()
	ok := h.commit(rec, httptest.NewRequest("POST", "/", nil), func(tx *store.Store) error {
		return tx.Audit(context.Background(), store.AuditEntry{ActorKind: store.ActorUser, ActorID: u.ID.String(), Action: "test.poison", Result: store.ResultAllowed})
	})
	if ok || rec.Code != 400 || !strings.Contains(rec.Body.String(), `"invalid policy"`) || strings.Contains(rec.Body.String(), "bogus") {
		t.Fatalf("commit: %v %d %s", ok, rec.Code, rec.Body.String())
	}
	if len(e.audit("test.poison")) != 0 {
		t.Fatal("audit row of rolled back tx stored")
	}
	if e.z.Enforcer() != before {
		t.Fatal("enforcer swapped")
	}
}

func TestOrgFromPathUsesPolicy(t *testing.T) {
	e := newEnv(t)
	_, ownerTok := e.user("alice")
	bob, bobTok := e.user("bob")
	org := e.createOrg(ownerTok, "Acme")
	e.exec(`INSERT INTO memberships (org_id, user_id, role) VALUES ($1, $2, 'member')`, org, bob.ID)
	if rec := e.do("GET", "/api/orgs/"+org.String()+"/limits", bobTok); rec.Code != 404 {
		t.Fatalf("before reload: %d %s", rec.Code, rec.Body.String())
	}
	e.reload()
	if rec := e.do("GET", "/api/orgs/"+org.String()+"/limits", bobTok); rec.Code != 200 {
		t.Fatalf("after reload: %d %s", rec.Code, rec.Body.String())
	}
}

package api

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/google/uuid"

	"github.com/asterism-dev/observatory/internal/authz"
	"github.com/asterism-dev/observatory/internal/platform/iamtest"
	"github.com/asterism-dev/observatory/internal/store"
)

func (e *env) exec(sql string, args ...any) {
	e.t.Helper()
	if _, err := e.pool.Exec(context.Background(), sql, args...); err != nil {
		e.t.Fatal(err)
	}
}

// reload waits until the hub's enforcer has iam's latest revision, plus the hub's own owner refs.
func (e *env) reload() {
	e.t.Helper()
	e.pol.Sync(e.t)
}

// createOrg creates an org owned by token's user, as iam's CreateOrg would.
func (e *env) createOrg(token, name string) uuid.UUID {
	e.t.Helper()
	p, err := e.jwts.Verify(context.Background(), token)
	if err != nil {
		e.t.Fatal(err)
	}
	org := e.iam.CreateOrg(p.User, name)
	e.reload()
	return org
}

func (e *env) addMember(org uuid.UUID, u iamtest.User, role store.Role) {
	e.t.Helper()
	e.iam.AddMember(org, u.ID, string(role))
	e.reload()
}

func (e *env) removeMember(org uuid.UUID, u iamtest.User) {
	e.t.Helper()
	e.iam.RemoveMember(org, u.ID)
	e.reload()
}

func (e *env) personalOrg(u iamtest.User) uuid.UUID {
	e.t.Helper()
	return e.iam.PersonalOrg(u.ID)
}

func (e *env) createTeam(org uuid.UUID, name string) uuid.UUID {
	e.t.Helper()
	team := e.iam.AddTeam(org, name)
	e.reload()
	return team
}

func (e *env) removeTeam(org, team uuid.UUID) {
	e.t.Helper()
	e.iam.RemoveTeam(org, team)
	e.reload()
}

func (e *env) setTeamMember(org, team uuid.UUID, u iamtest.User, admin bool) {
	e.t.Helper()
	e.iam.SetTeamMember(org, team, u.ID, admin)
	e.reload()
}

// createRole keeps a custom role from its JSON body until bind puts it into iam with its binding.
func (e *env) createRole(org uuid.UUID, body string) uuid.UUID {
	e.t.Helper()
	var role authz.CustomRole
	if err := json.Unmarshal([]byte(body), &role); err != nil {
		e.t.Fatal(err)
	}
	role.ID, role.OrgID = uuid.New(), org
	e.roles[role.ID] = role
	return role.ID
}

func (e *env) bind(org uuid.UUID, subject string, role uuid.UUID) {
	e.t.Helper()
	r, ok := e.roles[role]
	if !ok {
		e.t.Fatalf("unknown role %s", role)
	}
	e.iam.Bind(org, subject, r)
	e.reload()
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
	role := e.createRole(org, `{"name":"auditor","rules":[{"effect":"allow","object":"org/audit","action":"audit.events.list"}]}`)
	e.bind(org, "user:"+member.ID.String(), role)
	if !e.canReadAudit(member.ID, org) {
		t.Fatal("grant not applied")
	}
}

func TestOrgFromPathUsesPolicy(t *testing.T) {
	e := newEnv(t)
	_, ownerTok := e.user("alice")
	bob, bobTok := e.user("bob")
	org := e.createOrg(ownerTok, "Acme")
	e.iam.AddMember(org, bob.ID, "member")
	if rec := e.do("GET", "/api/orgs/"+org.String()+"/vms", bobTok); rec.Code != 404 {
		t.Fatalf("before reload: %d %s", rec.Code, rec.Body.String())
	}
	e.reload()
	if rec := e.do("GET", "/api/orgs/"+org.String()+"/vms", bobTok); rec.Code != 200 {
		t.Fatalf("after reload: %d %s", rec.Code, rec.Body.String())
	}
}

func TestOrgRouteUnavailableWhenStale(t *testing.T) {
	e := newEnv(t)
	_, tok := e.user("alice")
	org := e.createOrg(tok, "Acme")
	var fresh atomic.Bool
	e.z.WithFreshness(fresh.Load)
	rec := e.do("GET", "/api/orgs/"+org.String()+"/vms", tok)
	if rec.Code != http.StatusServiceUnavailable || !strings.Contains(rec.Body.String(), "policy unavailable") {
		t.Fatalf("stale policy: %d %s", rec.Code, rec.Body.String())
	}
	fresh.Store(true)
	if rec := e.do("GET", "/api/orgs/"+org.String()+"/vms", tok); rec.Code != http.StatusOK {
		t.Fatalf("fresh policy: %d %s", rec.Code, rec.Body.String())
	}
}

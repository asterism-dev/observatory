package api

import (
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/asterism-dev/observatory/internal/store"
)

type limitsResp struct {
	Org     map[string]*int            `json:"org"`
	Teams   map[string]map[string]*int `json:"teams"`
	Members map[string]map[string]*int `json:"members"`
}

func TestLimitsStack(t *testing.T) {
	e := newEnv(t)
	admin, adminTok := e.user("root")
	owner, ownerTok := e.user("alice")
	member, memberTok := e.user("bob")
	org := e.createOrg(ownerTok, "Acme")
	e.addMember(org, member, store.RoleMember)
	team := e.createTeam(ownerTok, org, "core")
	base := "/api/orgs/" + org.String() + "/limits"

	if rec := e.send("PUT", "/api/admin/orgs/"+org.String()+"/limits", adminTok, `{"vcpu":10,"diskGb":100}`); rec.Code != 204 {
		t.Fatalf("admin set org: %d %s", rec.Code, rec.Body.String())
	}
	var got limitsResp
	rec := e.do("GET", base, ownerTok)
	decode(t, rec, &got)
	if rec.Code != 200 || *got.Org["vcpu"] != 10 || *got.Org["diskGb"] != 100 || got.Org["ramMb"] != nil || got.Org["vmCount"] != nil ||
		got.Teams == nil || got.Members == nil || len(got.Teams) != 0 {
		t.Fatalf("get: %d %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"ramMb":null`) {
		t.Fatalf("null must be explicit: %s", rec.Body.String())
	}
	if rec := e.do("GET", base, memberTok); rec.Code != 200 {
		t.Fatalf("member read: %d", rec.Code)
	}

	teamPath := base + "/team:" + team.String()
	userPath := base + "/user:" + member.ID.String()
	if rec := e.send("PUT", teamPath, ownerTok, `{"vcpu":4,"ramMb":null}`); rec.Code != 204 {
		t.Fatalf("set team: %d %s", rec.Code, rec.Body.String())
	}
	if rec := e.send("PUT", userPath, ownerTok, `{"vcpu":2}`); rec.Code != 204 {
		t.Fatalf("set user: %d %s", rec.Code, rec.Body.String())
	}
	if es := e.audit(store.ActionLimitSet); len(es) != 3 || es[0].Result != store.ResultAllowed || es[0].ActorID != owner.ID.String() {
		t.Fatalf("audit: %+v", es)
	}
	rec = e.do("GET", base, ownerTok)
	decode(t, rec, &got)
	if *got.Teams[team.String()]["vcpu"] != 4 || *got.Members[member.ID.String()]["vcpu"] != 2 {
		t.Fatalf("get after set: %s", rec.Body.String())
	}

	rec = e.send("PUT", teamPath, ownerTok, `{"vcpu":11}`)
	if rec.Code != 400 || !strings.Contains(rec.Body.String(), `"limit exceeds org limit"`) {
		t.Fatalf("above org: %d %s", rec.Code, rec.Body.String())
	}
	if rec := e.send("PUT", "/api/admin/orgs/"+org.String()+"/limits", adminTok, `{"vcpu":3}`); rec.Code != 400 {
		t.Fatalf("org below team: %d", rec.Code)
	}
	for _, body := range []string{`{"vcpu":-1}`, `{"vcpu":1.5}`, `{"vcpu":"3"}`, `{"vcpu":99999999999}`, `nope`} {
		if rec := e.send("PUT", teamPath, ownerTok, body); rec.Code != 400 {
			t.Fatalf("%s: %d", body, rec.Code)
		}
	}
	if rec := e.send("PUT", teamPath, memberTok, `{"vcpu":1}`); rec.Code != 403 {
		t.Fatalf("member set: %d", rec.Code)
	}
	if es := e.audit(store.ActionLimitSet); es[0].Result != store.ResultDenied || es[0].ActorID != member.ID.String() {
		t.Fatalf("denied audit: %+v", es[0])
	}
	_ = admin
}

func TestLimitsScopeForeignTeam(t *testing.T) {
	e := newEnv(t)
	_, aTok := e.user("alice")
	_, bTok := e.user("bob")
	orgA, orgB := e.createOrg(aTok, "A"), e.createOrg(bTok, "B")
	foreign := e.createTeam(bTok, orgB, "x")
	bob, _ := e.store.Users(t.Context())
	base := "/api/orgs/" + orgA.String() + "/limits/"
	for _, scope := range []string{"team:" + foreign.String(), "team:" + uuid.NewString(), "user:" + bob[1].ID.String(),
		"user:" + uuid.NewString(), "team:nope", "bogus", "team:" + strings.ToUpper(foreign.String())} {
		if rec := e.send("PUT", base+scope, aTok, `{"vcpu":1}`); rec.Code != 404 {
			t.Fatalf("%s: %d %s", scope, rec.Code, rec.Body.String())
		}
	}
	if rec := e.send("PUT", base+"team:"+foreign.String(), bTok, `{"vcpu":1}`); rec.Code != 404 {
		t.Fatalf("foreign org path: %d", rec.Code)
	}
}

func TestOrgLimitOnlyPlatformAdmin(t *testing.T) {
	e := newEnv(t)
	admin, adminTok := e.user("root")
	_, ownerTok := e.user("alice")
	org := e.createOrg(ownerTok, "Acme")

	if rec := e.send("PUT", "/api/orgs/"+org.String()+"/limits/org", ownerTok, `{"vcpu":1}`); rec.Code != 403 {
		t.Fatalf("owner sets org limit: %d", rec.Code)
	}
	if rec := e.send("PUT", "/api/admin/orgs/"+org.String()+"/limits", ownerTok, `{"vcpu":1}`); rec.Code != 403 {
		t.Fatalf("owner via admin api: %d", rec.Code)
	}
	if es := e.audit(store.ActionLimitSet); len(es) != 2 || es[0].Result != store.ResultDenied || es[1].Result != store.ResultDenied {
		t.Fatalf("denied audit: %+v", es)
	}
	if rec := e.send("PUT", "/api/admin/orgs/"+org.String()+"/limits", adminTok, `{"vcpu":1}`); rec.Code != 204 {
		t.Fatalf("admin: %d %s", rec.Code, rec.Body.String())
	}
	es := e.audit(store.ActionLimitSet)
	if len(es) != 3 || es[0].Result != store.ResultAllowed || es[0].ActorID != admin.ID.String() || es[0].OrgID == nil || *es[0].OrgID != org {
		t.Fatalf("allowed audit: %+v", es)
	}
	if rec := e.send("PUT", "/api/admin/orgs/"+uuid.NewString()+"/limits", adminTok, `{}`); rec.Code != 404 {
		t.Fatalf("unknown org: %d", rec.Code)
	}
}

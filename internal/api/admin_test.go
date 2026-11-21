package api

import (
	"reflect"
	"testing"

	"github.com/google/uuid"

	"github.com/asterism-dev/observatory/internal/store"
)

func TestAdminEndpointsRequirePlatformAdmin(t *testing.T) {
	e := newEnv(t)
	e.user("root")
	alice, tok := e.user("alice")
	org := e.createOrg(tok, "Acme")
	o, u := "/api/admin/orgs", "/api/admin/users/"+alice.ID.String()+"/platform-admin"
	for _, c := range []struct{ method, path, body, action string }{
		{"GET", o, "", store.ActionAdminRead},
		{"GET", o + "/" + org.String(), "", store.ActionAdminRead},
		{"GET", "/api/admin/users", "", store.ActionAdminRead},
		{"PUT", o + "/" + org.String() + "/limits", `{}`, store.ActionLimitSet},
		{"PUT", u, `{"admin":true}`, store.ActionPlatformGrantAdmin},
		{"GET", "/api/admin/network", "", store.ActionAdminRead},
		{"PUT", "/api/admin/network", `{"guestMTU":1200}`, store.ActionPlatformGuestMTU},
	} {
		if rec := e.send(c.method, c.path, tok, c.body); rec.Code != 403 {
			t.Fatalf("%s %s: %d", c.method, c.path, rec.Code)
		}
		es := e.audit(c.action)
		if len(es) == 0 || es[0].Result != store.ResultDenied || es[0].ActorID != alice.ID.String() {
			t.Fatalf("%s %s audit: %+v", c.method, c.path, es)
		}
	}
	if got, _ := e.store.Users(t.Context()); got[0].PlatformAdmin == got[1].PlatformAdmin {
		t.Fatalf("alice must not have become admin: %+v", got)
	}
}

func TestAdminReadsAndGrant(t *testing.T) {
	e := newEnv(t)
	root, rootTok := e.user("root")
	alice, tok := e.user("alice")
	org := e.createOrg(tok, "Acme")
	team := e.createTeam(tok, org, "core")
	e.send("PUT", "/api/admin/orgs/"+org.String()+"/limits", rootTok, `{"vcpu":8}`)
	e.send("PUT", "/api/orgs/"+org.String()+"/limits/team:"+team.String(), tok, `{"vcpu":2}`)

	var orgs []struct {
		ID          uuid.UUID
		Name        string
		MemberCount int
	}
	rec := e.do("GET", "/api/admin/orgs", rootTok)
	decode(t, rec, &orgs)
	if rec.Code != 200 || len(orgs) != 3 {
		t.Fatalf("orgs: %d %s", rec.Code, rec.Body.String())
	}

	var detail struct {
		ID      uuid.UUID
		Name    string
		Members []struct{ ID, Email, Role string }
		Limits  limitsResp
	}
	rec = e.do("GET", "/api/admin/orgs/"+org.String(), rootTok)
	decode(t, rec, &detail)
	if rec.Code != 200 || detail.Name != "Acme" || len(detail.Members) != 1 || detail.Members[0].Role != "owner" ||
		*detail.Limits.Org["vcpu"] != 8 || *detail.Limits.Teams[team.String()]["vcpu"] != 2 {
		t.Fatalf("detail: %d %s", rec.Code, rec.Body.String())
	}
	if rec := e.do("GET", "/api/admin/orgs/"+uuid.NewString(), rootTok); rec.Code != 404 {
		t.Fatalf("unknown org: %d", rec.Code)
	}

	var users []struct {
		ID            uuid.UUID
		PlatformAdmin bool
	}
	decode(t, e.do("GET", "/api/admin/users", rootTok), &users)
	if len(users) != 2 {
		t.Fatalf("users: %+v", users)
	}

	path := "/api/admin/users/" + alice.ID.String() + "/platform-admin"
	for _, body := range []string{`{}`, `{"admin":"yes"}`, `nope`} {
		if rec := e.send("PUT", path, rootTok, body); rec.Code != 400 {
			t.Fatalf("%s: %d", body, rec.Code)
		}
	}
	if rec := e.send("PUT", path, rootTok, `{"admin":true}`); rec.Code != 204 {
		t.Fatalf("grant: %d", rec.Code)
	}
	if es := e.audit(store.ActionPlatformGrantAdmin); len(es) != 1 || es[0].Result != store.ResultAllowed ||
		es[0].Resource != "user/"+alice.ID.String() || es[0].ActorID != root.ID.String() || es[0].OrgID != nil {
		t.Fatalf("grant audit: %+v", es)
	}
	if rec := e.do("GET", "/api/admin/users", tok); rec.Code != 200 {
		t.Fatalf("new admin: %d", rec.Code)
	}
	if rec := e.send("PUT", path, rootTok, `{"admin":false}`); rec.Code != 204 {
		t.Fatalf("revoke: %d", rec.Code)
	}
	if es := e.audit(store.ActionPlatformRevokeAdmin); len(es) != 1 || es[0].Resource != "user/"+alice.ID.String() {
		t.Fatalf("revoke audit: %+v", es)
	}
	if rec := e.send("PUT", "/api/admin/users/"+uuid.NewString()+"/platform-admin", rootTok, `{"admin":true}`); rec.Code != 404 {
		t.Fatalf("unknown user: %d", rec.Code)
	}
}

func TestLastPlatformAdmin(t *testing.T) {
	e := newEnv(t)
	root, rootTok := e.user("root")
	rec := e.send("PUT", "/api/admin/users/"+root.ID.String()+"/platform-admin", rootTok, `{"admin":false}`)
	if rec.Code != 409 {
		t.Fatalf("revoke last: %d %s", rec.Code, rec.Body.String())
	}
	if es := e.audit(store.ActionPlatformRevokeAdmin); len(es) != 0 {
		t.Fatalf("failed revoke must not audit as allowed: %+v", es)
	}
}

func TestPlatformAdminHasNoTenantAccess(t *testing.T) {
	e := newEnv(t)
	_, rootTok := e.user("root")
	_, tok := e.user("alice")
	org := e.createOrg(tok, "Acme")
	base := "/api/orgs/" + org.String()
	for _, c := range []struct{ method, path, body string }{
		{"GET", base, ""}, {"GET", base + "/members", ""}, {"GET", base + "/limits", ""},
		{"GET", base + "/teams", ""}, {"POST", base + "/teams", `{"name":"x"}`}, {"PUT", base + "/limits/org", `{}`},
	} {
		if rec := e.send(c.method, c.path, rootTok, c.body); rec.Code != 404 {
			t.Fatalf("%s %s: %d", c.method, c.path, rec.Code)
		}
	}
	// The admin read is read-only: a platform admin gets no tenant action out of it.
	if rec := e.send("PATCH", "/api/admin/orgs/"+org.String(), rootTok, `{"name":"x"}`); rec.Code == 200 || rec.Code == 204 {
		t.Fatalf("admin org write: %d", rec.Code)
	}
}

func TestAdminNetworkGuestMTU(t *testing.T) {
	e := newEnv(t)
	_, rootTok := e.user("root")
	var got struct{ GuestMTU int }
	decode(t, e.do("GET", "/api/admin/network", rootTok), &got)
	if got.GuestMTU != 1300 {
		t.Fatalf("default %d", got.GuestMTU)
	}
	for _, body := range []string{`{"guestMTU":999}`, `{"guestMTU":1401}`, `{}`, `x`} {
		if rec := e.send("PUT", "/api/admin/network", rootTok, body); rec.Code != 400 {
			t.Fatalf("%s: %d", body, rec.Code)
		}
	}
	if rec := e.send("PUT", "/api/admin/network", rootTok, `{"guestMTU":1200}`); rec.Code != 200 {
		t.Fatalf("put: %d %s", rec.Code, rec.Body.String())
	}
	decode(t, e.do("GET", "/api/admin/network", rootTok), &got)
	if got.GuestMTU != 1200 {
		t.Fatalf("got %d", got.GuestMTU)
	}
}

func TestSetGuestMTUDoesNotTouchVMs(t *testing.T) {
	e := newEnv(t)
	root, rootTok := e.user("root")
	org := e.createOrg(rootTok, "Acme")
	node, _ := uuid.Parse(e.enrolledNode(rootTok, org, "n"))
	vm, err := e.store.CreateVM(t.Context(), store.VM{OrgID: org, NodeID: node, UserID: &root.ID, Name: "dev", CPUs: 1, MemoryMB: 1, DiskGB: 1, CreatedBy: root.ID})
	if err != nil {
		t.Fatal(err)
	}
	before, _ := e.store.VM(t.Context(), org, vm.ID)
	if rec := e.send("PUT", "/api/admin/network", rootTok, `{"guestMTU":1200}`); rec.Code != 200 {
		t.Fatalf("put: %d", rec.Code)
	}
	after, _ := e.store.VM(t.Context(), org, vm.ID)
	if !reflect.DeepEqual(before, after) {
		t.Fatalf("vm changed: %+v -> %+v", before, after)
	}
}

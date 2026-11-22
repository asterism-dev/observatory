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
	for _, c := range []struct{ method, path, body, action string }{
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
}

func TestPlatformAdminHasNoTenantAccess(t *testing.T) {
	e := newEnv(t)
	_, rootTok := e.user("root")
	_, tok := e.user("alice")
	org := e.createOrg(tok, "Acme")
	base := "/api/orgs/" + org.String()
	for _, c := range []struct{ method, path, body string }{
		{"GET", base + "/vms", ""},
	} {
		if rec := e.send(c.method, c.path, rootTok, c.body); rec.Code != 404 {
			t.Fatalf("%s %s: %d", c.method, c.path, rec.Code)
		}
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
	for _, body := range []string{`{"guestMTU":999}`, `{"guestMTU":1363}`, `{}`, `x`} {
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

func TestLimitsRoutesGone(t *testing.T) {
	e := newEnv(t)
	_, rootTok := e.user("root")
	alice, tok := e.user("alice")
	org := e.createOrg(tok, "Acme")
	for _, c := range []struct{ method, path, body string }{
		{"GET", "/api/admin/orgs/" + org.String() + "/limits", ""},
		{"PUT", "/api/admin/orgs/" + org.String() + "/limits", `{"vcpu":8}`},
		{"GET", "/api/orgs/" + org.String() + "/limits", ""},
		{"PUT", "/api/orgs/" + org.String() + "/limits/user:" + alice.ID.String(), `{"vcpu":2}`},
	} {
		for _, tk := range []string{rootTok, tok} {
			if rec := e.send(c.method, c.path, tk, c.body); rec.Code != 404 {
				t.Fatalf("%s %s: %d", c.method, c.path, rec.Code)
			}
		}
	}
}

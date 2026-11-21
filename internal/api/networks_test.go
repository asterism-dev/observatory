package api

import (
	"context"
	"net/netip"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/asterism-dev/observatory/internal/store"
)

func networksPath(org uuid.UUID) string { return "/api/orgs/" + org.String() + "/networks" }

type networkTest struct {
	ID, Name, Subnet  string
	VMCount           int
	CanUse, CanDelete bool
}

func (e *env) createNetwork(tok string, org uuid.UUID, body string) networkTest {
	e.t.Helper()
	rec := e.send("POST", networksPath(org), tok, body)
	if rec.Code != 201 {
		e.t.Fatalf("create network %s: %d %s", body, rec.Code, rec.Body.String())
	}
	var got networkTest
	decode(e.t, rec, &got)
	return got
}

func (e *env) setVMNetworks(tok string, org uuid.UUID, vm string, restart bool, ids ...string) int {
	e.t.Helper()
	body := `{"networkIds":["` + strings.Join(ids, `","`) + `"],"restartAllowed":` + map[bool]string{true: "true", false: "false"}[restart] + `}`
	if len(ids) == 0 {
		body = `{"networkIds":[],"restartAllowed":` + map[bool]string{true: "true", false: "false"}[restart] + `}`
	}
	return e.send("PUT", vmsPath(org)+"/"+vm+"/networks", tok, body).Code
}

func (e *env) generation(vm string) int {
	e.t.Helper()
	var g int
	if err := e.pool.QueryRow(context.Background(), "SELECT generation FROM vms WHERE id = $1", vm).Scan(&g); err != nil {
		e.t.Fatal(err)
	}
	return g
}

func (e *env) vmGet(tok string, org uuid.UUID, vm string) vmFullTest {
	e.t.Helper()
	var got vmFullTest
	decode(e.t, e.do("GET", vmsPath(org)+"/"+vm, tok), &got)
	return got
}

type vmFullTest struct {
	MgmtIP, DNSName string
	CanNetwork      bool
	Networks        []struct{ ID, Name, IP, DNSName string }
}

func TestNetworkCreate(t *testing.T) {
	e := newEnv(t)
	_, tok := e.user("alice")
	_, otherTok := e.user("bob")
	org := e.createOrg(tok, "Acme")
	other := e.createOrg(otherTok, "Other")

	n := e.createNetwork(tok, org, `{"name":"web"}`)
	if n.Subnet != "10.210.0.0/24" || !n.CanUse || !n.CanDelete {
		t.Fatalf("auto: %+v", n)
	}
	for _, body := range []string{`{"name":"Web"}`, `{"name":"1a"}`, `{"name":"a_b"}`, `{"name":"` + strings.Repeat("a", 32) + `"}`, `{"name":"x","subnet":"10.64.0.0/24"}`, `{"name":"x","subnet":"nope"}`} {
		if rec := e.send("POST", networksPath(org), tok, body); rec.Code != 400 {
			t.Fatalf("%s: %d %s", body, rec.Code, rec.Body.String())
		}
	}
	e.createNetwork(tok, org, `{"name":"db","subnet":"172.20.0.0/24"}`)
	if rec := e.send("POST", networksPath(other), otherTok, `{"name":"clash","subnet":"172.20.0.0/25"}`); rec.Code != 409 {
		t.Fatalf("overlap: %d", rec.Code)
	}
	if rec := e.send("POST", networksPath(org), tok, `{"name":"web","subnet":"172.21.0.0/24"}`); rec.Code != 409 {
		t.Fatalf("dup: %d", rec.Code)
	}
	if es := e.audit("network.networks.create"); len(es) != 2 || es[1].Details["name"] != "web" || es[1].Details["subnet"] != "10.210.0.0/24" {
		t.Fatalf("audit: %+v", es)
	}
	if _, err := netip.ParsePrefix(n.Subnet); err != nil {
		t.Fatal(err)
	}
}

func TestNetworkCreateByMemberCannotDelete(t *testing.T) {
	e := newEnv(t)
	_, ownerTok := e.user("alice")
	bob, bobTok := e.user("bob")
	org := e.createOrg(ownerTok, "Acme")
	e.addMember(org, bob, store.RoleMember)
	n := e.createNetwork(bobTok, org, `{"name":"web"}`)
	if !n.CanUse || n.CanDelete {
		t.Fatalf("create response: %+v", n)
	}
}

func TestVMNetworksRestart(t *testing.T) {
	e := newEnv(t)
	_, tok := e.user("alice")
	org := e.createOrg(tok, "Acme")
	node := e.enrolledNode(tok, org, "n1")
	n := e.createNetwork(tok, org, `{"name":"web"}`)
	v := e.createVM(tok, org, vmBody("dev", userOwner(), node))
	if rec := e.do("POST", vmsPath(org)+"/"+v.ID+"/start", tok); rec.Code != 200 {
		t.Fatal(rec.Code)
	}
	gen := e.generation(v.ID)
	before := e.vmGet(tok, org, v.ID)

	if code := e.setVMNetworks(tok, org, v.ID, false, n.ID); code != 409 {
		t.Fatalf("running without flag: %d", code)
	}
	if got := e.vmGet(tok, org, v.ID); len(got.Networks) != 0 || e.generation(v.ID) != gen {
		t.Fatalf("changed: %+v", got)
	}
	if code := e.setVMNetworks(tok, org, v.ID, true, n.ID); code != 200 {
		t.Fatalf("with flag: %d", code)
	}
	got := e.vmGet(tok, org, v.ID)
	prefix := netip.MustParsePrefix(n.Subnet)
	if e.generation(v.ID) != gen+1 || len(got.Networks) != 1 || !prefix.Contains(netip.MustParseAddr(got.Networks[0].IP)) ||
		got.Networks[0].DNSName != "dev.web.internal" || got.Networks[0].Name != "web" {
		t.Fatalf("after: gen %d %+v", e.generation(v.ID), got)
	}
	es := e.audit("compute.instances.setNetworks")
	if len(es) != 1 || es[0].Details["name"] != "dev" || len(es[0].Details["networks"].([]any)) != 1 {
		t.Fatalf("audit: %+v", es)
	}
	if code := e.setVMNetworks(tok, org, v.ID, false, n.ID); code != 200 || len(e.audit("compute.instances.setNetworks")) != 1 || e.generation(v.ID) != gen+1 {
		t.Fatalf("same list: %d", code)
	}
	if code := e.setVMNetworks(tok, org, v.ID, true); code != 200 {
		t.Fatalf("detach: %d", code)
	}
	if after := e.vmGet(tok, org, v.ID); after.MgmtIP != before.MgmtIP || len(after.Networks) != 0 {
		t.Fatalf("detach: %+v vs %+v", after, before)
	}

	if rec := e.do("POST", vmsPath(org)+"/"+v.ID+"/stop", tok); rec.Code != 200 {
		t.Fatal(rec.Code)
	}
	if code := e.setVMNetworks(tok, org, v.ID, false, n.ID); code != 200 {
		t.Fatalf("stopped no flag: %d", code)
	}
}

func TestVMNetworksPermissions(t *testing.T) {
	e := newEnv(t)
	_, ownerTok := e.user("alice")
	bob, bobTok := e.user("bob")
	carol, carolTok := e.user("carol")
	dave, daveTok := e.user("dave")
	org := e.createOrg(ownerTok, "Acme")
	e.addMember(org, bob, store.RoleMember)
	e.addMember(org, carol, store.RoleMember)
	e.addMember(org, dave, store.RoleAdmin)
	team := e.createTeam(ownerTok, org, "core")
	if rec := e.send("PUT", teamMemberPath(org, team, bob), ownerTok, `{"admin":false}`); rec.Code != 204 {
		t.Fatal(rec.Code)
	}
	e.makeTeamAdmin(ownerTok, org, team, carol)
	node := e.enrolledNode(ownerTok, org, "n1")
	n := e.createNetwork(ownerTok, org, `{"name":"web"}`)
	v := e.createVM(carolTok, org, vmBody("t1", teamOwner(team), node))

	if code := e.setVMNetworks(bobTok, org, v.ID, true, n.ID); code != 403 {
		t.Fatalf("team member: %d", code)
	}
	for _, tok := range []string{carolTok, daveTok, ownerTok} {
		if code := e.setVMNetworks(tok, org, v.ID, true, n.ID); code != 200 {
			t.Fatalf("allowed caller: %d", code)
		}
		if code := e.setVMNetworks(tok, org, v.ID, true); code != 200 {
			t.Fatalf("detach: %d", code)
		}
	}

	role := e.createRole(ownerTok, org, `{"name":"no-use","rules":[{"effect":"deny","object":"org/networks/*","action":"network.networks.use"}]}`)
	e.bind(ownerTok, org, "user:"+bob.ID.String(), role)
	mine := e.createVM(bobTok, org, vmBody("b1", userOwner(), node))
	if code := e.setVMNetworks(bobTok, org, mine.ID, true, n.ID); code != 403 {
		t.Fatalf("no network.networks.use: %d", code)
	}
	if code := e.setVMNetworks(bobTok, org, mine.ID, true); code != 200 {
		t.Fatalf("detach-only needs no network.networks.use: %d", code)
	}
}

func TestNetworkDeleteAttached(t *testing.T) {
	e := newEnv(t)
	_, tok := e.user("alice")
	org := e.createOrg(tok, "Acme")
	node := e.enrolledNode(tok, org, "n1")
	n := e.createNetwork(tok, org, `{"name":"web"}`)
	v := e.createVM(tok, org, vmBody("dev", userOwner(), node))
	if code := e.setVMNetworks(tok, org, v.ID, true, n.ID); code != 200 {
		t.Fatal(code)
	}
	if rec := e.do("DELETE", networksPath(org)+"/"+n.ID, tok); rec.Code != 409 {
		t.Fatalf("delete attached: %d", rec.Code)
	}
	var list []networkTest
	decode(t, e.do("GET", networksPath(org), tok), &list)
	if len(list) != 1 || list[0].VMCount != 1 {
		t.Fatalf("list: %+v", list)
	}
	e.setVMNetworks(tok, org, v.ID, true)
	if rec := e.do("DELETE", networksPath(org)+"/"+n.ID, tok); rec.Code != 204 {
		t.Fatalf("delete: %d", rec.Code)
	}
	if es := e.audit("network.networks.delete"); len(es) != 1 || es[0].Details["name"] != "web" {
		t.Fatalf("audit: %+v", es)
	}
}

func TestVMJSONMgmt(t *testing.T) {
	e := newEnv(t)
	_, tok := e.user("alice")
	org := e.createOrg(tok, "Acme")
	node := e.enrolledNode(tok, org, "n1")
	v := e.createVM(tok, org, vmBody("dev", userOwner(), node))
	got := e.vmGet(tok, org, v.ID)
	slice, err := e.store.OrgSlice(context.Background(), org)
	if err != nil {
		t.Fatal(err)
	}
	if ip, err := netip.ParseAddr(got.MgmtIP); err != nil || !slice.Contains(ip) || got.DNSName != "dev.internal" || !got.CanNetwork || got.Networks == nil {
		t.Fatalf("%+v slice %s", got, slice)
	}
}

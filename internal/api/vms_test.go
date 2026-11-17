package api

import (
	"slices"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/asterism-dev/observatory/internal/store"
)

func vmsPath(org uuid.UUID) string { return "/api/orgs/" + org.String() + "/vms" }

type vmJSONTest struct {
	ID, Name, State, Power string
	Owner                  struct{ Kind, ID, Name string }
	Node                   struct{ ID, Name string }
	CanPower, CanDelete    bool
}

func vmBody(name, owner, node string) string {
	return `{"name":"` + name + `","owner":` + owner + `,"nodeId":"` + node + `","cpus":1,"memoryMB":512,"diskGB":5}`
}

func userOwner() string { return `{"kind":"user"}` }

func teamOwner(team uuid.UUID) string { return `{"kind":"team","teamId":"` + team.String() + `"}` }

func (e *env) createVM(tok string, org uuid.UUID, body string) vmJSONTest {
	e.t.Helper()
	rec := e.send("POST", vmsPath(org), tok, body)
	if rec.Code != 201 {
		e.t.Fatalf("create vm %s: %d %s", body, rec.Code, rec.Body.String())
	}
	var got vmJSONTest
	decode(e.t, rec, &got)
	return got
}

type vmListTest struct {
	VMs             []vmJSONTest
	CreatableOwners []struct{ Kind, ID, Name string }
	Nodes           []struct {
		ID, Name string
		Online   bool
	}
}

func (e *env) listVMs(tok string, org uuid.UUID) vmListTest {
	e.t.Helper()
	rec := e.do("GET", vmsPath(org), tok)
	if rec.Code != 200 {
		e.t.Fatalf("list vms: %d %s", rec.Code, rec.Body.String())
	}
	for _, leak := range []string{"env", "sshPublicKeys", "Env", "SSHPublicKeys"} {
		if strings.Contains(rec.Body.String(), `"`+leak+`"`) {
			e.t.Fatalf("list leaks %s: %s", leak, rec.Body.String())
		}
	}
	var got vmListTest
	decode(e.t, rec, &got)
	return got
}

func TestVMCreateLimitsAndValidation(t *testing.T) {
	e := newEnv(t)
	alice, tok := e.user("alice")
	org := e.createOrg(tok, "Acme")
	node := e.createNode(tok, org, "n1").Node.ID

	for _, body := range []string{
		vmBody("Bad", userOwner(), node),
		vmBody("-x", userOwner(), node),
		`{"name":"a","owner":{"kind":"user"},"nodeId":"` + node + `","cpus":0,"memoryMB":512,"diskGB":5}`,
		`{"name":"a","owner":{"kind":"user"},"nodeId":"` + node + `","cpus":33,"memoryMB":512,"diskGB":5}`,
		`{"name":"a","owner":{"kind":"user"},"nodeId":"` + node + `","cpus":1,"memoryMB":511,"diskGB":5}`,
		`{"name":"a","owner":{"kind":"user"},"nodeId":"` + node + `","cpus":1,"memoryMB":512,"diskGB":2049}`,
		vmBody("a", `{"kind":"robot"}`, node),
		vmBody("a", `{"kind":"team"}`, node),
	} {
		if rec := e.send("POST", vmsPath(org), tok, body); rec.Code != 400 {
			t.Errorf("%s: %d %s", body, rec.Code, rec.Body.String())
		}
	}
	if rec := e.send("POST", vmsPath(org), tok, vmBody("a", userOwner(), uuid.NewString())); rec.Code != 404 {
		t.Errorf("unknown node: %d", rec.Code)
	}

	if rec := e.send("PUT", "/api/orgs/"+org.String()+"/limits/user:"+alice.ID.String(), tok, `{"vmCount":1}`); rec.Code != 204 {
		t.Fatalf("set limit: %d %s", rec.Code, rec.Body.String())
	}
	v := e.createVM(tok, org, vmBody("one", userOwner(), node))
	if v.Owner.Kind != "user" || v.Owner.ID != alice.ID.String() || v.Owner.Name != "alice" || v.Node.Name != "n1" || v.State != "pending" || !v.CanPower || !v.CanDelete {
		t.Fatalf("%+v", v)
	}
	rec := e.send("POST", vmsPath(org), tok, vmBody("two", userOwner(), node))
	if rec.Code != 409 || !strings.Contains(rec.Body.String(), "limit exceeded") {
		t.Fatalf("over limit: %d %s", rec.Code, rec.Body.String())
	}
	if rec := e.send("POST", vmsPath(org), tok, vmBody("one", userOwner(), node)); rec.Code != 409 {
		t.Fatalf("duplicate name: %d", rec.Code)
	}

	es := e.audit("vm:create")
	if len(es) != 1 || es[0].Resource != "org/vms/"+v.ID {
		t.Fatalf("audit: %+v", es)
	}
	var keys []string
	for k := range es[0].Details {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	if !slices.Equal(keys, []string{"cpus", "diskGB", "memoryMB", "name", "node", "owner"}) {
		t.Fatalf("audit details: %v", es[0].Details)
	}
}

func TestVMTeamPermissions(t *testing.T) {
	e := newEnv(t)
	_, ownerTok := e.user("alice")
	bob, bobTok := e.user("bob")
	carol, carolTok := e.user("carol")
	org := e.createOrg(ownerTok, "Acme")
	e.addMember(org, bob, store.RoleMember)
	e.addMember(org, carol, store.RoleMember)
	team := e.createTeam(ownerTok, org, "core")
	if rec := e.send("PUT", teamMemberPath(org, team, bob), ownerTok, `{"admin":false}`); rec.Code != 204 {
		t.Fatal(rec.Code)
	}
	e.makeTeamAdmin(ownerTok, org, team, carol)
	node := e.createNode(ownerTok, org, "n1").Node.ID

	if rec := e.send("POST", vmsPath(org), bobTok, vmBody("t1", teamOwner(team), node)); rec.Code != 403 {
		t.Fatalf("team VM by non-admin member: %d", rec.Code)
	}
	v := e.createVM(carolTok, org, vmBody("t1", teamOwner(team), node))
	if v.Owner.Kind != "team" || v.Owner.Name != "core" {
		t.Fatalf("%+v", v)
	}
	bobList := e.listVMs(bobTok, org)
	if len(bobList.VMs) != 1 || !bobList.VMs[0].CanPower || bobList.VMs[0].CanDelete {
		t.Fatalf("bob list: %+v", bobList.VMs)
	}
	if !slices.ContainsFunc(bobList.Nodes, func(n struct {
		ID, Name string
		Online   bool
	}) bool {
		return n.ID == node
	}) {
		t.Fatalf("nodes: %+v", bobList.Nodes)
	}
	if len(bobList.CreatableOwners) != 1 || bobList.CreatableOwners[0].Kind != "user" {
		t.Fatalf("bob owners: %+v", bobList.CreatableOwners)
	}
	carolOwners := e.listVMs(carolTok, org).CreatableOwners
	if len(carolOwners) != 2 || carolOwners[1].Kind != "team" || carolOwners[1].ID != team.String() || carolOwners[1].Name != "core" {
		t.Fatalf("carol owners: %+v", carolOwners)
	}

	if rec := e.do("POST", vmsPath(org)+"/"+v.ID+"/shutdown", bobTok); rec.Code != 200 {
		t.Fatalf("member power: %d %s", rec.Code, rec.Body.String())
	}
	if rec := e.do("DELETE", vmsPath(org)+"/"+v.ID, bobTok); rec.Code != 403 {
		t.Fatalf("member delete: %d", rec.Code)
	}
	if rec := e.do("DELETE", vmsPath(org)+"/"+v.ID, carolTok); rec.Code != 204 {
		t.Fatalf("team admin delete: %d %s", rec.Code, rec.Body.String())
	}
	if es := e.audit("vm:delete"); len(es) == 0 || es[0].Result != store.ResultAllowed {
		t.Fatalf("delete audit: %+v", es)
	}
}

func TestVMPowerAndVisibility(t *testing.T) {
	e := newEnv(t)
	_, ownerTok := e.user("alice")
	bob, bobTok := e.user("bob")
	carol, carolTok := e.user("carol")
	org := e.createOrg(ownerTok, "Acme")
	e.addMember(org, bob, store.RoleMember)
	e.addMember(org, carol, store.RoleMember)
	node := e.createNode(ownerTok, org, "n1").Node.ID
	v := e.createVM(bobTok, org, vmBody("bobs", userOwner(), node))

	if got := e.listVMs(carolTok, org).VMs; len(got) != 0 {
		t.Fatalf("carol sees %+v", got)
	}
	if rec := e.do("GET", vmsPath(org)+"/"+v.ID, carolTok); rec.Code != 403 {
		t.Fatalf("carol get: %d", rec.Code)
	}
	if rec := e.do("POST", vmsPath(org)+"/"+v.ID+"/stop", carolTok); rec.Code != 403 {
		t.Fatalf("carol power: %d", rec.Code)
	}

	for _, c := range []struct{ action, power, mode string }{
		{"start", "running", ""}, {"shutdown", "stopped", "graceful"}, {"stop", "stopped", "hard"},
	} {
		rec := e.do("POST", vmsPath(org)+"/"+v.ID+"/"+c.action, ownerTok)
		if rec.Code != 200 {
			t.Fatalf("admin %s: %d %s", c.action, rec.Code, rec.Body.String())
		}
		var got vmJSONTest
		decode(t, rec, &got)
		if got.Power != c.power {
			t.Fatalf("%s: %+v", c.action, got)
		}
		es := e.audit("vm:power")
		if es[0].Details["power"] != c.power || (c.mode != "" && es[0].Details["stopMode"] != c.mode) {
			t.Fatalf("%s audit: %+v", c.action, es[0].Details)
		}
	}
	rec := e.do("GET", vmsPath(org)+"/"+v.ID, bobTok)
	if rec.Code != 200 || strings.Contains(rec.Body.String(), "sshPublicKeys") || strings.Contains(rec.Body.String(), `"env"`) {
		t.Fatalf("get: %d %s", rec.Code, rec.Body.String())
	}
	if got := e.listVMs(ownerTok, org).VMs; len(got) != 1 || !got[0].CanDelete {
		t.Fatalf("admin list: %+v", got)
	}
}

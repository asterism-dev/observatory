package api

import (
	"context"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/asterism-dev/observatory/internal/proto"
	"github.com/asterism-dev/observatory/internal/store"
)

func vmsPath(org uuid.UUID) string { return "/api/orgs/" + org.String() + "/vms" }

type vmJSONTest struct {
	ID, Name, State, Power           string
	ImageRef, ImageSource            string
	Owner                            struct{ Kind, ID, Name string }
	Node                             struct{ ID, Name string }
	CanPower, CanDelete              bool
	CanSnapshot, CanRestore, CanFork bool
}

func vmBody(name, owner, node string) string {
	return `{"name":"` + name + `","owner":` + owner + `,"nodeId":"` + node + `","cpus":1,"memoryMB":512,"diskGB":10}`
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
	node := e.enrolledNode(tok, org, "n1")

	for _, body := range []string{
		vmBody("Bad", userOwner(), node),
		vmBody("-x", userOwner(), node),
		vmBody("x-", userOwner(), node),
		`{"name":"a","owner":{"kind":"user"},"nodeId":"` + node + `","cpus":0,"memoryMB":512,"diskGB":10}`,
		`{"name":"a","owner":{"kind":"user"},"nodeId":"` + node + `","cpus":33,"memoryMB":512,"diskGB":10}`,
		`{"name":"a","owner":{"kind":"user"},"nodeId":"` + node + `","cpus":1,"memoryMB":511,"diskGB":10}`,
		`{"name":"a","owner":{"kind":"user"},"nodeId":"` + node + `","cpus":1,"memoryMB":512,"diskGB":2049}`,
		`{"name":"a","owner":{"kind":"user"},"nodeId":"` + node + `","cpus":1,"memoryMB":512,"diskGB":9}`,
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
	pending := e.createNode(tok, org, "pending").Node.ID
	if rec := e.send("POST", vmsPath(org), tok, vmBody("a", userOwner(), pending)); rec.Code != 409 || !strings.Contains(rec.Body.String(), "node not enrolled") {
		t.Errorf("pending node: %d %s", rec.Code, rec.Body.String())
	}
	if nodes := e.listVMs(tok, org).Nodes; len(nodes) != 1 || nodes[0].ID != node {
		t.Errorf("creatable nodes: %+v", nodes)
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

	es := e.audit("compute.instances.create")
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
	node := e.enrolledNode(ownerTok, org, "n1")

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
	if b := bobList.VMs[0]; !b.CanSnapshot || b.CanRestore || b.CanFork {
		t.Fatalf("team member snapshot flags: %+v", b)
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
	if es := e.audit("compute.instances.delete"); len(es) == 0 || es[0].Result != store.ResultAllowed {
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
	node := e.enrolledNode(ownerTok, org, "n1")
	v := e.createVM(bobTok, org, vmBody("bobs", userOwner(), node))

	if got := e.listVMs(carolTok, org).VMs; len(got) != 0 {
		t.Fatalf("carol sees %+v", got)
	}
	if got := e.listVMs(bobTok, org).VMs; len(got) != 1 || !got[0].CanSnapshot || !got[0].CanRestore || !got[0].CanFork {
		t.Fatalf("owner snapshot flags: %+v", got)
	}
	dan, danTok := e.user("dan")
	e.addMember(org, dan, store.RoleAdmin)
	if got := e.listVMs(danTok, org).VMs; len(got) != 1 || !got[0].CanSnapshot || !got[0].CanRestore || got[0].CanFork {
		t.Fatalf("admin snapshot flags: %+v", got)
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
		es := e.audit("compute.instances.setPower")
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

func TestPowerPauseHibernateResume(t *testing.T) {
	e := newEnv(t)
	_, tok := e.user("alice")
	org := e.createOrg(tok, "Acme")
	v := e.createVM(tok, org, vmBody("a", userOwner(), e.enrolledNode(tok, org, "n1")))
	for _, c := range []struct{ action, power string }{
		{"pause", "paused"}, {"hibernate", "hibernated"}, {"pause", "paused"}, {"resume", "running"},
		{"hibernate", "hibernated"}, {"resume", "running"},
	} {
		rec := e.do("POST", vmsPath(org)+"/"+v.ID+"/"+c.action, tok)
		if rec.Code != 200 {
			t.Fatalf("%s: %d %s", c.action, rec.Code, rec.Body.String())
		}
		var got vmJSONTest
		decode(t, rec, &got)
		if got.Power != c.power {
			t.Fatalf("%s: %+v", c.action, got)
		}
		if d := e.audit("compute.instances.setPower")[0].Details; d["action"] != c.action || d["power"] != c.power {
			t.Fatalf("%s audit: %+v", c.action, d)
		}
	}
}

func TestPauseStoppedVM(t *testing.T) {
	e := newEnv(t)
	_, tok := e.user("alice")
	org := e.createOrg(tok, "Acme")
	v := e.createVM(tok, org, vmBody("a", userOwner(), e.enrolledNode(tok, org, "n1")))
	if rec := e.do("POST", vmsPath(org)+"/"+v.ID+"/stop", tok); rec.Code != 200 {
		t.Fatal(rec.Code)
	}
	for _, action := range []string{"pause", "hibernate"} {
		if rec := e.do("POST", vmsPath(org)+"/"+v.ID+"/"+action, tok); rec.Code != 409 {
			t.Fatalf("%s stopped VM: %d %s", action, rec.Code, rec.Body.String())
		}
	}
}

func TestPausePermission(t *testing.T) {
	e := newEnv(t)
	_, ownerTok := e.user("alice")
	bob, bobTok := e.user("bob")
	carol, carolTok := e.user("carol")
	org := e.createOrg(ownerTok, "Acme")
	e.addMember(org, bob, store.RoleMember)
	e.addMember(org, carol, store.RoleMember)
	core, other := e.createTeam(ownerTok, org, "core"), e.createTeam(ownerTok, org, "other")
	e.makeTeamAdmin(ownerTok, org, core, carol)
	if rec := e.send("PUT", teamMemberPath(org, other, bob), ownerTok, `{"admin":false}`); rec.Code != 204 {
		t.Fatal(rec.Code)
	}
	v := e.createVM(carolTok, org, vmBody("t1", teamOwner(core), e.enrolledNode(ownerTok, org, "n1")))
	if rec := e.do("POST", vmsPath(org)+"/"+v.ID+"/pause", bobTok); rec.Code != 403 {
		t.Fatalf("other team pause: %d", rec.Code)
	}
	if rec := e.send("PUT", teamMemberPath(org, core, bob), ownerTok, `{"admin":false}`); rec.Code != 204 {
		t.Fatal(rec.Code)
	}
	if rec := e.do("POST", vmsPath(org)+"/"+v.ID+"/pause", bobTok); rec.Code != 200 {
		t.Fatalf("owning team pause: %d %s", rec.Code, rec.Body.String())
	}
}

func TestValidateVMDisks(t *testing.T) {
	disk := func(path string) store.VMDisk { return store.VMDisk{DiskID: uuid.New(), MountPath: path} }
	for path, ok := range map[string]bool{
		"/data": true, "/home/dev/work": true, "/var/data": true, "/var/lib/foo": true,
		"/": false, "/data/": false, "/a/../b": false, "/etc": false, "/etc/x": false, "/home/dev": false,
		"/home/dev/.ssh/k": false, "/var": false, "/var/lib/cloud/x": false, "data": false, "": false, "/a b": false,
	} {
		if err := validateVMDisks([]store.VMDisk{disk(path)}); (err == nil) != ok {
			t.Errorf("%q: %v", path, err)
		}
	}
	var nine []store.VMDisk
	for i := 0; i < 9; i++ {
		nine = append(nine, disk("/d"+strconv.Itoa(i)))
	}
	if validateVMDisks(nine[:8]) != nil || validateVMDisks(nine) == nil {
		t.Error("limit of 8")
	}
	if validateVMDisks([]store.VMDisk{disk("/a"), disk("/a")}) == nil {
		t.Error("duplicate path")
	}
	if validateVMDisks([]store.VMDisk{disk("/data/x"), disk("/data")}) == nil {
		t.Error("nested path")
	}
	if validateVMDisks([]store.VMDisk{disk("/data"), disk("/data2")}) != nil {
		t.Error("sibling path with a common prefix")
	}
	d := disk("/a")
	if validateVMDisks([]store.VMDisk{d, {DiskID: d.DiskID, MountPath: "/b"}}) == nil {
		t.Error("duplicate disk")
	}
}

type vmDisksTest struct {
	Disks []struct {
		ID, Name, MountPath, State, Error string
		SizeGB                            int
	}
	CanDisks bool
}

func (e *env) setVMDisks(tok string, org uuid.UUID, vm string, restart bool, disks ...string) *httptest.ResponseRecorder {
	e.t.Helper()
	items := []string{}
	for i, d := range disks {
		items = append(items, `{"diskId":"`+d+`","mountPath":"/d`+strconv.Itoa(i)+`"}`)
	}
	return e.send("PUT", vmsPath(org)+"/"+vm+"/disks", tok, `{"disks":[`+strings.Join(items, ",")+`],"restartAllowed":`+strconv.FormatBool(restart)+`}`)
}

func (e *env) stopVM(tok string, org uuid.UUID, vm string) {
	e.t.Helper()
	if rec := e.do("POST", vmsPath(org)+"/"+vm+"/stop", tok); rec.Code != 200 {
		e.t.Fatalf("stop: %d", rec.Code)
	}
}

func TestSetVMDisksAPI(t *testing.T) {
	e := newEnv(t)
	alice, tok := e.user("alice")
	org := e.createOrg(tok, "Acme")
	node := e.enrolledNode(tok, org, "n1")
	team := e.createTeam(tok, org, "core")
	e.makeTeamAdmin(tok, org, team, alice)
	d := e.createDisk(tok, org, diskBody("data", userOwner(), 1))
	td := e.createDisk(tok, org, diskBody("shared", teamOwner(team), 1))
	v := e.createVM(tok, org, vmBody("dev", userOwner(), node))
	e.stopVM(tok, org, v.ID)

	rec := e.setVMDisks(tok, org, v.ID, false, d.ID)
	var got vmDisksTest
	decode(t, rec, &got)
	if rec.Code != 200 || len(got.Disks) != 1 || got.Disks[0].MountPath != "/d0" || got.Disks[0].Name != "data" || got.Disks[0].SizeGB != 1 || !got.CanDisks {
		t.Fatalf("attach: %d %s", rec.Code, rec.Body.String())
	}
	if es := e.audit("compute.instances.setDisks"); len(es) != 1 || es[0].Details["name"] != "dev" || es[0].Details["disks"].([]any)[0] != "data:/d0" {
		t.Fatalf("audit: %+v", es)
	}
	if rec := e.setVMDisks(tok, org, v.ID, false, d.ID); rec.Code != 200 || len(e.audit("compute.instances.setDisks")) != 1 {
		t.Fatalf("same list: %d", rec.Code)
	}
	if rec := e.setVMDisks(tok, org, v.ID, false, d.ID, td.ID); rec.Code != 409 {
		t.Fatalf("team disk on user VM: %d", rec.Code)
	}
	if rec := e.send("PUT", vmsPath(org)+"/"+v.ID+"/disks", tok, `{"disks":[{"diskId":"`+d.ID+`","mountPath":"/etc"}]}`); rec.Code != 400 {
		t.Fatalf("reserved path: %d", rec.Code)
	}
	if rec := e.setVMDisks(tok, org, v.ID, false, uuid.NewString()); rec.Code != 404 {
		t.Fatalf("unknown disk: %d", rec.Code)
	}
	if rec := e.do("POST", vmsPath(org)+"/"+v.ID+"/start", tok); rec.Code != 200 {
		t.Fatal(rec.Code)
	}
	if rec := e.setVMDisks(tok, org, v.ID, false); rec.Code != 409 {
		t.Fatalf("running without flag: %d", rec.Code)
	}
	if rec := e.setVMDisks(tok, org, v.ID, true); rec.Code != 200 {
		t.Fatalf("detach: %d", rec.Code)
	}
}

func TestSetVMDisksMissingDisk(t *testing.T) {
	e := newEnv(t)
	_, tok := e.user("alice")
	org := e.createOrg(tok, "Acme")
	node := e.enrolledNode(tok, org, "n1")
	d := e.createDisk(tok, org, diskBody("data", userOwner(), 1))
	v := e.createVM(tok, org, vmBody("dev", userOwner(), node))
	e.markReady()
	if err := e.disks.SetStatus(context.Background(), statusWithFiles()); err != nil {
		t.Fatal(err)
	}
	if rec := e.setVMDisks(tok, org, v.ID, false, d.ID); rec.Code != 409 || !strings.Contains(rec.Body.String(), "missing from storage") {
		t.Fatalf("missing disk: %d %s", rec.Code, rec.Body.String())
	}
}

func TestVMDisksAuthorizationDenied(t *testing.T) {
	e := newEnv(t)
	_, ownerTok := e.user("alice")
	bob, bobTok := e.user("bob")
	carol, carolTok := e.user("carol")
	admin, adminTok := e.user("dave")
	org := e.createOrg(ownerTok, "Acme")
	for u, r := range map[store.User]store.Role{bob: store.RoleMember, carol: store.RoleMember, admin: store.RoleAdmin} {
		e.addMember(org, u, r)
	}
	node := e.enrolledNode(ownerTok, org, "n1")
	bobDisk := e.createDisk(bobTok, org, diskBody("data", userOwner(), 1))
	bobVM := e.createVM(bobTok, org, vmBody("bobvm", userOwner(), node))
	adminVM := e.createVM(adminTok, org, vmBody("adminvm", userOwner(), node))
	e.stopVM(bobTok, org, bobVM.ID)
	e.stopVM(adminTok, org, adminVM.ID)

	before := e.deniedRows("compute.instances.setDisks")
	if rec := e.setVMDisks(carolTok, org, bobVM.ID, true); rec.Code != 403 || e.deniedRows("compute.instances.setDisks") != before+1 {
		t.Fatalf("other member on vm: %d", rec.Code)
	}
	if rec := e.setVMDisks(carolTok, org, uuid.NewString(), true); rec.Code != 404 {
		t.Fatalf("unknown vm: %d", rec.Code)
	}
	before = e.deniedRows("compute.disks.use")
	if rec := e.setVMDisks(adminTok, org, adminVM.ID, true, bobDisk.ID); rec.Code != 403 || e.deniedRows("compute.disks.use") != before+1 {
		t.Fatalf("admin attaches member disk: %d", rec.Code)
	}
	if d := e.listDisks(bobTok, org)[0]; d.Attachment != nil {
		t.Fatalf("attached despite denial: %+v", d)
	}
	if rec := e.setVMDisks(bobTok, org, bobVM.ID, false, bobDisk.ID); rec.Code != 200 {
		t.Fatalf("owner attach: %d", rec.Code)
	}
	if rec := e.setVMDisks(adminTok, org, bobVM.ID, false); rec.Code != 200 {
		t.Fatalf("admin detach: %d", rec.Code)
	}
	if es := e.audit("compute.instances.setDisks"); es[0].Result != store.ResultAllowed || len(es[0].Details["disks"].([]any)) != 0 {
		t.Fatalf("detach audit: %+v", es)
	}
}

func TestVMDiskErrorShown(t *testing.T) {
	e := newEnv(t)
	_, tok := e.user("alice")
	org := e.createOrg(tok, "Acme")
	node := e.enrolledNode(tok, org, "n1")
	d := e.createDisk(tok, org, diskBody("data", userOwner(), 1))
	v := e.createVM(tok, org, vmBody("dev", userOwner(), node))
	e.stopVM(tok, org, v.ID)
	if rec := e.setVMDisks(tok, org, v.ID, false, d.ID); rec.Code != 200 {
		t.Fatal(rec.Code)
	}
	e.api.diskErrors = func(vm uuid.UUID) map[string]string {
		if vm.String() == v.ID {
			return map[string]string{d.ID: "NBD connection lost"}
		}
		return nil
	}
	var got vmDisksTest
	decode(t, e.do("GET", vmsPath(org)+"/"+v.ID, tok), &got)
	if len(got.Disks) != 1 || got.Disks[0].Error != "NBD connection lost" || got.Disks[0].State != "creating" {
		t.Fatalf("%+v", got)
	}
}

func TestAutoSuspendValidation(t *testing.T) {
	e := newEnv(t)
	_, tok := e.user("alice")
	org := e.createOrg(tok, "Acme")
	v := e.createVM(tok, org, vmBody("a", userOwner(), e.enrolledNode(tok, org, "n1")))
	for minutes, code := range map[int]int{3: 400, 0: 200, 5: 200, 1440: 200, 1441: 400} {
		rec := e.send("PUT", vmsPath(org)+"/"+v.ID+"/auto-suspend", tok, `{"minutes":`+strconv.Itoa(minutes)+`}`)
		if rec.Code != code {
			t.Fatalf("minutes %d: %d %s", minutes, rec.Code, rec.Body.String())
		}
		if code == 200 {
			var got struct{ AutoSuspendMinutes int }
			decode(t, rec, &got)
			if got.AutoSuspendMinutes != minutes {
				t.Fatalf("minutes %d: %+v", minutes, got)
			}
			if d := e.audit("compute.instances.setPower")[0].Details; d["autoSuspendMinutes"] != float64(minutes) {
				t.Fatalf("audit: %+v", d)
			}
		}
	}
}

func TestAutoSuspendPermission(t *testing.T) {
	e := newEnv(t)
	_, ownerTok := e.user("alice")
	bob, bobTok := e.user("bob")
	org := e.createOrg(ownerTok, "Acme")
	e.addMember(org, bob, store.RoleMember)
	v := e.createVM(ownerTok, org, vmBody("a", userOwner(), e.enrolledNode(ownerTok, org, "n1")))
	if rec := e.send("PUT", vmsPath(org)+"/"+v.ID+"/auto-suspend", bobTok, `{"minutes":15}`); rec.Code != 403 {
		t.Fatalf("no compute.instances.setPower: %d", rec.Code)
	}
}

func TestStartWakesSuspended(t *testing.T) {
	e := newEnv(t)
	_, tok := e.user("alice")
	org := e.createOrg(tok, "Acme")
	v := e.createVM(tok, org, vmBody("a", userOwner(), e.enrolledNode(tok, org, "n1")))
	vid, node := uuid.MustParse(v.ID), uuid.MustParse(v.Node.ID)
	for i, action := range []string{"start", "resume"} {
		if err := e.store.SetVMStatus(context.Background(), node, proto.VMStatus{ID: v.ID, State: proto.StateSuspended}); err != nil {
			t.Fatal(err)
		}
		if rec := e.do("POST", vmsPath(org)+"/"+v.ID+"/"+action, tok); rec.Code != 200 {
			t.Fatalf("%s: %d %s", action, rec.Code, rec.Body.String())
		}
		got, err := e.store.VM(context.Background(), org, vid)
		if err != nil || got.WakeSeq != uint64(i+1) {
			t.Fatalf("%s: wake_seq %d %v", action, got.WakeSeq, err)
		}
	}
	if err := e.store.SetVMStatus(context.Background(), node, proto.VMStatus{ID: v.ID, State: proto.StateRunning}); err != nil {
		t.Fatal(err)
	}
	if rec := e.do("POST", vmsPath(org)+"/"+v.ID+"/start", tok); rec.Code != 200 {
		t.Fatal(rec.Code)
	}
	if got, _ := e.store.VM(context.Background(), org, vid); got.WakeSeq != 2 {
		t.Fatalf("running start must not bump wake_seq: %d", got.WakeSeq)
	}
}

func TestVMJSONImage(t *testing.T) {
	e := newEnv(t)
	_, tok := e.user("alice")
	org := e.createOrg(tok, "Acme")
	node := e.enrolledNode(tok, org, "n1")
	vm := e.createVM(tok, org, vmBody("a", userOwner(), node))
	nodeID, _ := uuid.Parse(node)
	id, _ := uuid.Parse(vm.ID)
	if err := e.store.SetVMStatus(context.Background(), nodeID, proto.VMStatus{ID: vm.ID, State: proto.StateRunning, Image: "v1"}); err != nil {
		t.Fatal(err)
	}
	var got struct{ Image string }
	decode(t, e.do("GET", vmsPath(org)+"/"+id.String(), tok), &got)
	if got.Image != "v1" {
		t.Fatalf("image %q", got.Image)
	}
}

func TestVMJSONLegacyRuntime(t *testing.T) {
	e := newEnv(t)
	_, tok := e.user("alice")
	org := e.createOrg(tok, "Acme")
	node := e.enrolledNode(tok, org, "n1")
	vm := e.createVM(tok, org, vmBody("a", userOwner(), node))
	legacy := func() bool {
		var got struct{ LegacyRuntime bool }
		decode(t, e.do("GET", vmsPath(org)+"/"+vm.ID, tok), &got)
		return got.LegacyRuntime
	}
	if legacy() {
		t.Fatal("new VM reported as legacy")
	}
	if _, err := e.pool.Exec(context.Background(), "UPDATE vms SET legacy_runtime = true WHERE id = $1", vm.ID); err != nil {
		t.Fatal(err)
	}
	if !legacy() {
		t.Fatal("legacy flag not reported")
	}
}

func TestStartLegacyVMConflict(t *testing.T) {
	e := newEnv(t)
	_, tok := e.user("alice")
	org := e.createOrg(tok, "Acme")
	node := e.enrolledNode(tok, org, "n1")
	vm := e.createVM(tok, org, vmBody("a", userOwner(), node))
	if _, err := e.pool.Exec(context.Background(), "UPDATE vms SET legacy_runtime = true WHERE id = $1", vm.ID); err != nil {
		t.Fatal(err)
	}
	for _, action := range []string{"start", "resume"} {
		rec := e.send("POST", vmsPath(org)+"/"+vm.ID+"/"+action, tok, "")
		if rec.Code != 409 || !strings.Contains(rec.Body.String(), "created with the old guest runtime; recreate this VM") {
			t.Fatalf("%s: %d %s", action, rec.Code, rec.Body)
		}
	}
	if rec := e.send("POST", vmsPath(org)+"/"+vm.ID+"/stop", tok, ""); rec.Code != 200 {
		t.Fatalf("stop: %d", rec.Code)
	}
}

func TestCreateVMDiskMinimum(t *testing.T) {
	e := newEnv(t)
	_, tok := e.user("alice")
	org := e.createOrg(tok, "Acme")
	node := e.enrolledNode(tok, org, "n1")
	body := func(gb string) string {
		return `{"name":"a","owner":{"kind":"user"},"nodeId":"` + node + `","cpus":1,"memoryMB":512,"diskGB":` + gb + `}`
	}
	if rec := e.send("POST", vmsPath(org), tok, body("9")); rec.Code != 400 {
		t.Fatalf("9 GB: %d", rec.Code)
	}
	if rec := e.send("POST", vmsPath(org), tok, body("10")); rec.Code != 201 {
		t.Fatalf("10 GB: %d %s", rec.Code, rec.Body.String())
	}
}

func imageVMBody(name, owner, node, image string) string {
	return strings.TrimSuffix(vmBody(name, owner, node), "}") + `,"image":"` + image + `"}`
}

func (e *env) vmImage(org uuid.UUID, id string) store.VM {
	e.t.Helper()
	v, err := e.store.VM(context.Background(), org, uuid.MustParse(id))
	if err != nil {
		e.t.Fatal(err)
	}
	return v
}

func TestCreateVMWithImageRef(t *testing.T) {
	e := newEnv(t)
	_, tok := e.user("alice")
	org := e.createOrg(tok, "Acme")
	node := e.enrolledNode(tok, org, "n1")
	img := e.createImage(tok, org, `{"name":"base","scope":"org"}`)
	d := e.addVersion(img, 1, "latest", "v1")

	got := e.createVM(tok, org, imageVMBody("a", userOwner(), node, "base:v1"))
	v := e.vmImage(org, got.ID)
	if got.ImageRef != "base:v1" || got.ImageSource != "explicit" || v.ImageRef != "base:v1" || v.ImageDigest != d || v.ImageID == nil || *v.ImageID != img {
		t.Fatalf("json %+v, stored %+v", got, v)
	}

	got = e.createVM(tok, org, imageVMBody("b", userOwner(), node, "observatory/standard"))
	if v = e.vmImage(org, got.ID); v.ImageRef != "observatory/standard" || v.ImageDigest != "" || v.ImageID != nil || got.ImageSource != "platform" {
		t.Fatalf("platform: %+v %+v", got, v)
	}

	for _, ref := range []string{"nope", "base:missing", "observatory/standard:v1"} {
		if rec := e.send("POST", vmsPath(org), tok, imageVMBody("c", userOwner(), node, ref)); rec.Code != 422 {
			t.Fatalf("%s: %d %s", ref, rec.Code, rec.Body.String())
		}
	}
}

func TestCreateVMUsesUserDefault(t *testing.T) {
	e := newEnv(t)
	_, tok := e.user("alice")
	org := e.createOrg(tok, "Acme")
	node := e.enrolledNode(tok, org, "n1")
	orgImg := e.createImage(tok, org, `{"name":"base","scope":"org"}`)
	e.addVersion(orgImg, 1, "latest")
	mine := e.createImage(tok, org, `{"name":"mine","scope":"user"}`)
	d := e.addVersion(mine, 2, "latest")
	defaults := "/api/orgs/" + org.String() + "/image-defaults/"
	if rec := e.send("PUT", defaults+"org", tok, `{"ref":"base"}`); rec.Code != 204 {
		t.Fatal(rec.Code, rec.Body.String())
	}
	if rec := e.send("PUT", defaults+"user", tok, `{"ref":"mine"}`); rec.Code != 204 {
		t.Fatal(rec.Code, rec.Body.String())
	}
	got := e.createVM(tok, org, vmBody("a", userOwner(), node))
	v := e.vmImage(org, got.ID)
	if got.ImageSource != "user-default" || v.ImageRef != "me/mine@"+d || v.ImageDigest != d || v.ImageID == nil || *v.ImageID != mine {
		t.Fatalf("json %+v, stored %+v", got, v)
	}
}

func TestCreateTeamVMUsesTeamDefault(t *testing.T) {
	e := newEnv(t)
	_, tok := e.user("alice")
	org := e.createOrg(tok, "Acme")
	node := e.enrolledNode(tok, org, "n1")
	team := e.createTeam(tok, org, "core")
	userImg := e.createImage(tok, org, `{"name":"mine","scope":"user"}`)
	e.addVersion(userImg, 1, "latest")
	teamImg := e.createImage(tok, org, `{"name":"shared","scope":"team","teamId":"`+team.String()+`"}`)
	d := e.addVersion(teamImg, 2, "latest")
	defaults := "/api/orgs/" + org.String() + "/image-defaults/"
	if rec := e.send("PUT", defaults+"user", tok, `{"ref":"mine"}`); rec.Code != 204 {
		t.Fatal(rec.Code, rec.Body.String())
	}
	if rec := e.send("PUT", defaults+"team/"+team.String(), tok, `{"ref":"core/shared"}`); rec.Code != 204 {
		t.Fatal(rec.Code, rec.Body.String())
	}
	got := e.createVM(tok, org, vmBody("a", teamOwner(team), node))
	v := e.vmImage(org, got.ID)
	if got.ImageSource != "team-default" || v.ImageRef != "core/shared@"+d || v.ImageDigest != d || *v.ImageID != teamImg {
		t.Fatalf("json %+v, stored %+v", got, v)
	}
}

func TestCreateVMAmbiguousImage(t *testing.T) {
	e := newEnv(t)
	_, tok := e.user("alice")
	org := e.createOrg(tok, "Acme")
	node := e.enrolledNode(tok, org, "n1")
	e.addVersion(e.createImage(tok, org, `{"name":"base","scope":"org"}`), 1, "latest")
	e.addVersion(e.createImage(tok, org, `{"name":"base","scope":"user"}`), 2, "latest")
	rec := e.send("POST", vmsPath(org), tok, imageVMBody("a", userOwner(), node, "base"))
	if rec.Code != 422 || !strings.Contains(rec.Body.String(), "me/base") || !strings.Contains(rec.Body.String(), "org/base") {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}
	if rec := e.send("POST", vmsPath(org), tok, imageVMBody("a", userOwner(), node, "org/base")); rec.Code != 201 {
		t.Fatalf("qualified: %d %s", rec.Code, rec.Body.String())
	}
}

func TestForkKeepsImage(t *testing.T) {
	e := newEnv(t)
	_, tok := e.user("alice")
	org := e.createOrg(tok, "Acme")
	node := e.enrolledNode(tok, org, "n1")
	img := e.createImage(tok, org, `{"name":"base","scope":"org"}`)
	d := e.addVersion(img, 1, "latest")
	src := e.createVM(tok, org, imageVMBody("a", userOwner(), node, "base"))
	sn := e.takeSnapshot(tok, org, src.ID, `{"name":"base"}`)
	e.snapshotsReady()
	rec := e.send("POST", snapshotsPath(org, src.ID)+"/"+sn.ID+"/fork", tok, `{"name":"b"}`)
	if rec.Code != 201 {
		t.Fatalf("fork: %d %s", rec.Code, rec.Body.String())
	}
	var fork vmJSONTest
	decode(t, rec, &fork)
	v := e.vmImage(org, fork.ID)
	if v.ImageRef != "base" || v.ImageDigest != d || v.ImageID == nil || *v.ImageID != img || fork.ImageSource != "explicit" {
		t.Fatalf("fork %+v stored %+v", fork, v)
	}
}

func TestCreateForTeamNotInPolicy404(t *testing.T) {
	e := newEnv(t)
	_, tok := e.user("alice")
	org := e.createOrg(tok, "Acme")
	node := e.enrolledNode(tok, org, "n1")
	var team uuid.UUID
	if err := e.pool.QueryRow(context.Background(), `INSERT INTO teams (org_id, name) VALUES ($1, 't') RETURNING id`, org).Scan(&team); err != nil {
		t.Fatal(err)
	}
	if rec := e.send("POST", vmsPath(org), tok, vmBody("vm1", teamOwner(team), node)); rec.Code != 404 {
		t.Fatalf("vm for unknown team: %d %s", rec.Code, rec.Body.String())
	}
	if rec := e.send("POST", disksPath(org), tok, diskBody("d1", teamOwner(team), 1)); rec.Code != 404 {
		t.Fatalf("disk for unknown team: %d %s", rec.Code, rec.Body.String())
	}
	e.reload()
	e.createVM(tok, org, vmBody("vm1", teamOwner(team), node))
	e.createDisk(tok, org, diskBody("d1", teamOwner(team), 1))
}

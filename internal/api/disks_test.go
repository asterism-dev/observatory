package api

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/asterism-dev/observatory/internal/proto"
	"github.com/asterism-dev/observatory/internal/store"
)

func disksPath(org uuid.UUID) string { return "/api/orgs/" + org.String() + "/disks" }

type diskTest struct {
	ID, Name, State, Warning string
	SizeGB                   int
	Owner                    struct{ Kind, ID, Name string }
	Attachment               *struct{ MountPath string }
	CanUpdate, CanDelete     bool
	CanUse                   bool
}

func (e *env) createDisk(tok string, org uuid.UUID, body string) diskTest {
	e.t.Helper()
	rec := e.send("POST", disksPath(org), tok, body)
	if rec.Code != 201 {
		e.t.Fatalf("create disk %s: %d %s", body, rec.Code, rec.Body.String())
	}
	var got diskTest
	decode(e.t, rec, &got)
	return got
}

func (e *env) listDisks(tok string, org uuid.UUID) []diskTest {
	e.t.Helper()
	rec := e.do("GET", disksPath(org), tok)
	if rec.Code != 200 {
		e.t.Fatalf("list disks: %d %s", rec.Code, rec.Body.String())
	}
	var got struct{ Disks []diskTest }
	decode(e.t, rec, &got)
	return got.Disks
}

func diskBody(name string, owner string, size int) string {
	b, _ := json.Marshal(size)
	return `{"name":"` + name + `","owner":` + owner + `,"sizeGB":` + string(b) + `}`
}

func (e *env) markReady() {
	e.t.Helper()
	if _, err := e.pool.Exec(context.Background(), "UPDATE disks SET ready_gb = size_gb"); err != nil {
		e.t.Fatal(err)
	}
}

func (e *env) attach(org uuid.UUID, vm, disk string) {
	e.t.Helper()
	_, _, err := e.store.SetVMDisks(context.Background(), org, uuid.MustParse(vm), []store.VMDisk{{DiskID: uuid.MustParse(disk), MountPath: "/data"}}, true)
	if err != nil {
		e.t.Fatal(err)
	}
}

func TestCreateDisk(t *testing.T) {
	e := newEnv(t)
	_, ownerTok := e.user("alice")
	bob, bobTok := e.user("bob")
	org := e.createOrg(ownerTok, "Acme")
	e.addMember(org, bob, store.RoleMember)
	team := e.createTeam(ownerTok, org, "core")

	d := e.createDisk(bobTok, org, diskBody("data", userOwner(), 10))
	if d.State != "creating" || d.SizeGB != 10 || d.Owner.Kind != "user" || d.Owner.Name != "bob" || !d.CanUse || !d.CanDelete {
		t.Fatalf("%+v", d)
	}
	for _, body := range []string{diskBody("Data", userOwner(), 1), diskBody("d", userOwner(), 0), diskBody("d", userOwner(), 4097), diskBody("d", `{"kind":"x"}`, 1)} {
		if rec := e.send("POST", disksPath(org), bobTok, body); rec.Code != 400 {
			t.Fatalf("%s: %d", body, rec.Code)
		}
	}
	if rec := e.send("POST", disksPath(org), bobTok, diskBody("t", teamOwner(team), 1)); rec.Code != 403 {
		t.Fatalf("team without create: %d", rec.Code)
	}
	if rec := e.send("POST", disksPath(org), bobTok, diskBody("data", userOwner(), 1)); rec.Code != 409 {
		t.Fatalf("duplicate: %d", rec.Code)
	}
	if es := e.audit("compute.disks.create"); len(es) != 2 || es[0].Result != store.ResultDenied || es[1].Details["name"] != "data" || es[1].Details["sizeGB"] != float64(10) {
		t.Fatalf("audit: %+v", es)
	}
}

func TestAdminCannotCopyOrAttachMemberDisk(t *testing.T) {
	e := newEnv(t)
	_, ownerTok := e.user("alice")
	bob, bobTok := e.user("bob")
	admin, adminTok := e.user("carol")
	org := e.createOrg(ownerTok, "Acme")
	e.addMember(org, bob, store.RoleMember)
	e.addMember(org, admin, store.RoleAdmin)
	node := e.enrolledNode(ownerTok, org, "n1")
	d := e.createDisk(bobTok, org, diskBody("data", userOwner(), 1))

	copyPath := disksPath(org) + "/" + d.ID + "/copy"
	if rec := e.send("POST", copyPath, adminTok, `{"name":"c","owner":{"kind":"user"}}`); rec.Code != 403 {
		t.Fatalf("admin copy: %d", rec.Code)
	}
	e.markReady()
	if rec := e.send("POST", copyPath, bobTok, `{"name":"c1","owner":{"kind":"user"}}`); rec.Code != 201 || strings.Contains(rec.Body.String(), `"warning"`) {
		t.Fatalf("idle copy: %d %s", rec.Code, rec.Body.String())
	}
	v := e.createVM(bobTok, org, vmBody("dev", userOwner(), node))
	e.attach(org, v.ID, d.ID)
	rec := e.send("POST", copyPath, bobTok, `{"name":"c2","owner":{"kind":"user"}}`)
	var got diskTest
	decode(t, rec, &got)
	if rec.Code != 201 || got.Warning == "" || got.Name != "c2" {
		t.Fatalf("attached copy: %d %s", rec.Code, rec.Body.String())
	}
	if es := e.audit("compute.disks.copy"); len(es) != 2 || es[0].Details["source"] != d.ID {
		t.Fatalf("audit: %+v", es)
	}
}

func TestCopyMissingSource(t *testing.T) {
	e := newEnv(t)
	_, tok := e.user("alice")
	org := e.createOrg(tok, "Acme")
	d := e.createDisk(tok, org, diskBody("data", userOwner(), 1))
	e.markReady()
	if err := e.disks.SetStatus(context.Background(), statusWithFiles()); err != nil {
		t.Fatal(err)
	}
	if rec := e.send("POST", disksPath(org)+"/"+d.ID+"/copy", tok, `{"name":"c","owner":{"kind":"user"}}`); rec.Code != 409 {
		t.Fatalf("missing source: %d %s", rec.Code, rec.Body.String())
	}
}

func TestGrowDiskAPI(t *testing.T) {
	e := newEnv(t)
	_, tok := e.user("alice")
	org := e.createOrg(tok, "Acme")
	node := e.enrolledNode(tok, org, "n1")
	d := e.createDisk(tok, org, diskBody("data", userOwner(), 2))
	v := e.createVM(tok, org, vmBody("dev", userOwner(), node))
	e.attach(org, v.ID, d.ID)
	if rec := e.do("POST", vmsPath(org)+"/"+v.ID+"/start", tok); rec.Code != 200 {
		t.Fatal(rec.Code)
	}
	e.markReady()
	path := disksPath(org) + "/" + d.ID + "/grow"
	if rec := e.send("POST", path, tok, `{"sizeGB":4}`); rec.Code != 409 {
		t.Fatalf("no restart flag: %d %s", rec.Code, rec.Body.String())
	}
	for _, body := range []string{`{"sizeGB":2,"restartAllowed":true}`, `{"sizeGB":4097,"restartAllowed":true}`} {
		if rec := e.send("POST", path, tok, body); rec.Code != 400 {
			t.Fatalf("%s: %d", body, rec.Code)
		}
	}
	rec := e.send("POST", path, tok, `{"sizeGB":4,"restartAllowed":true}`)
	var got diskTest
	decode(t, rec, &got)
	if rec.Code != 200 || got.SizeGB != 4 || got.State != "growing" {
		t.Fatalf("grow: %d %s", rec.Code, rec.Body.String())
	}
	if es := e.audit("compute.disks.grow"); len(es) != 1 || es[0].Details["sizeGB"] != float64(4) {
		t.Fatalf("audit: %+v", es)
	}
}

func TestForceReleaseAPI(t *testing.T) {
	e := newEnv(t)
	_, tok := e.user("alice")
	org := e.createOrg(tok, "Acme")
	node := e.enrolledNode(tok, org, "n1")
	d := e.createDisk(tok, org, diskBody("data", userOwner(), 1))
	v := e.createVM(tok, org, vmBody("dev", userOwner(), node))
	path := disksPath(org) + "/" + d.ID + "/release"
	if rec := e.do("POST", path, tok); rec.Code != 409 {
		t.Fatalf("idle: %d", rec.Code)
	}
	e.attach(org, v.ID, d.ID)
	if rec := e.do("POST", path, tok); rec.Code != 409 {
		t.Fatalf("attached: %d", rec.Code)
	}
	if _, _, err := e.store.SetVMDisks(context.Background(), org, uuid.MustParse(v.ID), nil, true); err != nil {
		t.Fatal(err)
	}
	if rec := e.do("POST", path, tok); rec.Code != 204 {
		t.Fatalf("releasing: %d %s", rec.Code, rec.Body.String())
	}
	if es := e.audit("compute.disks.release"); len(es) != 1 {
		t.Fatalf("audit: %+v", es)
	}
	if rec := e.do("DELETE", disksPath(org)+"/"+d.ID, tok); rec.Code != 204 {
		t.Fatalf("delete: %d %s", rec.Code, rec.Body.String())
	}
	if es := e.audit("compute.disks.delete"); len(es) != 1 {
		t.Fatalf("audit: %+v", es)
	}
}

func TestDiskListFiltered(t *testing.T) {
	e := newEnv(t)
	_, ownerTok := e.user("alice")
	bob, bobTok := e.user("bob")
	carol, carolTok := e.user("carol")
	admin, adminTok := e.user("dave")
	org := e.createOrg(ownerTok, "Acme")
	for _, u := range []store.User{bob, carol} {
		e.addMember(org, u, store.RoleMember)
	}
	e.addMember(org, admin, store.RoleAdmin)
	team := e.createTeam(ownerTok, org, "core")
	if rec := e.send("PUT", teamMemberPath(org, team, bob), ownerTok, `{"admin":false}`); rec.Code != 200 {
		t.Fatal(rec.Code)
	}
	e.makeTeamAdmin(ownerTok, org, team, carol)
	e.createDisk(bobTok, org, diskBody("mine", userOwner(), 1))
	e.createDisk(carolTok, org, diskBody("hers", userOwner(), 1))
	e.createDisk(carolTok, org, diskBody("shared", teamOwner(team), 1))

	got := e.listDisks(bobTok, org)
	names := []string{}
	for _, d := range got {
		names = append(names, d.Name)
	}
	if strings.Join(names, ",") != "mine,shared" {
		t.Fatalf("bob sees %v", names)
	}
	for _, d := range got {
		if d.Name == "shared" && (d.CanDelete || d.CanUpdate || !d.CanUse) {
			t.Fatalf("shared perms: %+v", d)
		}
	}
	all := e.listDisks(adminTok, org)
	if len(all) != 3 {
		t.Fatalf("admin sees %d", len(all))
	}
	for _, d := range all {
		if d.CanUse {
			t.Fatalf("admin canUse: %+v", d)
		}
	}
}

func TestDiskResponsesHaveNoKeys(t *testing.T) {
	e := newEnv(t)
	_, tok := e.user("alice")
	org := e.createOrg(tok, "Acme")
	node := e.enrolledNode(tok, org, "n1")
	d := e.createDisk(tok, org, diskBody("data", userOwner(), 1))
	v := e.createVM(tok, org, vmBody("dev", userOwner(), node))
	e.attach(org, v.ID, d.ID)
	e.markReady()
	st, err := e.store.StorageState(context.Background())
	if err != nil || len(st.Exports) != 1 {
		t.Fatalf("%v %+v", err, st)
	}
	secrets := []string{st.Exports[0].PSK}
	bodies := []string{
		e.do("GET", disksPath(org), tok).Body.String(),
		e.do("GET", disksPath(org)+"/"+d.ID, tok).Body.String(),
		e.send("POST", disksPath(org)+"/"+d.ID+"/copy", tok, `{"name":"c","owner":{"kind":"user"}}`).Body.String(),
		e.send("POST", disksPath(org)+"/"+d.ID+"/grow", tok, `{"sizeGB":3,"restartAllowed":true}`).Body.String(),
		e.do("GET", vmsPath(org), tok).Body.String(),
		e.do("GET", vmsPath(org)+"/"+v.ID, tok).Body.String(),
	}
	for _, en := range e.audit("") {
		b, _ := json.Marshal(en.Details)
		bodies = append(bodies, string(b))
	}
	for _, body := range bodies {
		for _, s := range secrets {
			if strings.Contains(body, s) || strings.Contains(body, "psk") || strings.Contains(body, "nbd://") {
				t.Fatalf("leak in %s", body)
			}
		}
	}
}

func statusWithFiles() proto.StorageStatus {
	return proto.StorageStatus{Files: []proto.StorageFile{}, Version: "v"}
}

func (e *env) deniedRows(action string) int {
	n := 0
	for _, en := range e.audit(action) {
		if en.Result == store.ResultDenied {
			n++
		}
	}
	return n
}

func TestDiskAuthorizationDenied(t *testing.T) {
	e := newEnv(t)
	_, ownerTok := e.user("alice")
	bob, bobTok := e.user("bob")
	carol, carolTok := e.user("carol")
	admin, adminTok := e.user("dave")
	org := e.createOrg(ownerTok, "Acme")
	e.addMember(org, bob, store.RoleMember)
	e.addMember(org, carol, store.RoleMember)
	e.addMember(org, admin, store.RoleAdmin)
	team := e.createTeam(ownerTok, org, "core")
	d := e.createDisk(bobTok, org, diskBody("data", userOwner(), 1))
	base := disksPath(org) + "/" + d.ID

	if rec := e.send("POST", base+"/copy", adminTok, `{"name":"c","owner":{"kind":"user"}}`); rec.Code != 403 {
		t.Fatalf("admin copy: %d", rec.Code)
	}
	if e.deniedRows("compute.disks.use") != 1 {
		t.Fatal("no denied row for admin copy")
	}
	if rec := e.send("POST", base+"/copy", bobTok, `{"name":"c","owner":`+teamOwner(team)+`}`); rec.Code != 403 {
		t.Fatalf("team copy without create: %d", rec.Code)
	}
	if e.deniedRows("compute.disks.create") != 1 {
		t.Fatal("no denied row for team copy")
	}

	for _, c := range []struct{ method, path, body, action string }{
		{"POST", base + "/grow", `{"sizeGB":2}`, "compute.disks.update"},
		{"POST", base + "/release", "", "compute.disks.update"},
		{"DELETE", base, "", "compute.disks.delete"},
	} {
		before := e.deniedRows(c.action)
		if rec := e.send(c.method, c.path, carolTok, c.body); rec.Code != 403 {
			t.Fatalf("other member %s %s: %d", c.method, c.path, rec.Code)
		}
		if e.deniedRows(c.action) != before+1 {
			t.Fatalf("no denied row for %s", c.path)
		}
	}
}

func TestDiskForeignOrgNotFound(t *testing.T) {
	e := newEnv(t)
	_, tok := e.user("alice")
	_, otherTok := e.user("bob")
	org := e.createOrg(tok, "Acme")
	other := e.createOrg(otherTok, "Other")
	d := e.createDisk(otherTok, other, diskBody("data", userOwner(), 1))
	base := disksPath(org) + "/" + d.ID

	for _, c := range []struct{ method, path, body string }{
		{"GET", base, ""},
		{"POST", base + "/copy", `{"name":"c","owner":{"kind":"user"}}`},
		{"POST", base + "/grow", `{"sizeGB":2}`},
		{"POST", base + "/release", ""},
		{"DELETE", base, ""},
	} {
		if rec := e.send(c.method, c.path, tok, c.body); rec.Code != 404 {
			t.Fatalf("%s %s: %d", c.method, c.path, rec.Code)
		}
	}
}

func TestCopyWarnsWhileSourceSettles(t *testing.T) {
	e := newEnv(t)
	_, tok := e.user("alice")
	org := e.createOrg(tok, "Acme")
	d := e.createDisk(tok, org, diskBody("data", userOwner(), 1))
	path := disksPath(org) + "/" + d.ID + "/copy"
	var got diskTest
	rec := e.send("POST", path, tok, `{"name":"c1","owner":{"kind":"user"}}`)
	decode(t, rec, &got)
	if rec.Code != 201 || got.Warning == "" {
		t.Fatalf("creating source: %d %s", rec.Code, rec.Body.String())
	}
	e.markReady()
	rec = e.send("POST", path, tok, `{"name":"c2","owner":{"kind":"user"}}`)
	if rec.Code != 201 || strings.Contains(rec.Body.String(), `"warning"`) {
		t.Fatalf("ready source: %d %s", rec.Code, rec.Body.String())
	}
}

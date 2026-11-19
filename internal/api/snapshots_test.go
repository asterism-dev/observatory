package api

import (
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/asterism-dev/observatory/internal/store"
)

type snapshotTest struct {
	ID, Name, State, Error string
	Memory                 bool
	SizeBytes              int64
	CanRestoreMemory       bool
}

func snapshotsPath(org uuid.UUID, vm string) string { return vmsPath(org) + "/" + vm + "/snapshots" }

func (e *env) takeSnapshot(tok string, org uuid.UUID, vm, body string) snapshotTest {
	e.t.Helper()
	rec := e.send("POST", snapshotsPath(org, vm), tok, body)
	if rec.Code != 201 {
		e.t.Fatalf("snapshot %s: %d %s", body, rec.Code, rec.Body.String())
	}
	var got snapshotTest
	decode(e.t, rec, &got)
	return got
}

func (e *env) snapshotsReady() {
	e.t.Helper()
	e.exec("UPDATE vm_snapshots SET state = 'ready'")
}

func TestSnapshotAPIFlow(t *testing.T) {
	e := newEnv(t)
	_, tok := e.user("alice")
	org := e.createOrg(tok, "Acme")
	v := e.createVM(tok, org, vmBody("a", userOwner(), e.enrolledNode(tok, org, "n1")))
	sn := e.takeSnapshot(tok, org, v.ID, `{"name":"before","memory":true}`)

	rec := e.do("GET", snapshotsPath(org, v.ID), tok)
	if rec.Code != 200 {
		t.Fatalf("list: %d %s", rec.Code, rec.Body.String())
	}
	var list []snapshotTest
	decode(t, rec, &list)
	if len(list) != 1 || list[0].ID != sn.ID || list[0].Name != "before" || list[0].State != "creating" || !list[0].Memory {
		t.Fatalf("list: %+v", list)
	}
	base := snapshotsPath(org, v.ID) + "/" + sn.ID
	if rec := e.send("POST", base+"/restore", tok, `{"memory":true,"restartAllowed":true}`); rec.Code != 409 {
		t.Fatalf("restore while creating: %d %s", rec.Code, rec.Body.String())
	}
	if rec := e.do("DELETE", base, tok); rec.Code != 204 {
		t.Fatalf("delete: %d %s", rec.Code, rec.Body.String())
	}
	if es := e.audit(store.ActionVMSnapshot); len(es) != 1 || es[0].Details["name"] != "before" || es[0].Details["memory"] != true {
		t.Fatalf("snapshot audit: %+v", es)
	}
	if es := e.audit(store.ActionVMSnapshotDelete); len(es) != 1 || es[0].Details["name"] != "before" {
		t.Fatalf("delete audit: %+v", es)
	}
}

func TestSnapshotNameInvalid(t *testing.T) {
	e := newEnv(t)
	_, tok := e.user("alice")
	org := e.createOrg(tok, "Acme")
	v := e.createVM(tok, org, vmBody("a", userOwner(), e.enrolledNode(tok, org, "n1")))
	if rec := e.send("POST", snapshotsPath(org, v.ID), tok, `{"name":"Before"}`); rec.Code != 400 {
		t.Fatalf("invalid name: %d %s", rec.Code, rec.Body.String())
	}
}

func TestAdminCannotFork(t *testing.T) {
	e := newEnv(t)
	_, ownerTok := e.user("alice")
	bob, bobTok := e.user("bob")
	admin, adminTok := e.user("dave")
	org := e.createOrg(ownerTok, "Acme")
	e.addMember(org, bob, store.RoleMember)
	e.addMember(org, admin, store.RoleAdmin)
	v := e.createVM(bobTok, org, vmBody("bobvm", userOwner(), e.enrolledNode(ownerTok, org, "n1")))
	sn := e.takeSnapshot(bobTok, org, v.ID, `{"name":"base"}`)
	e.snapshotsReady()
	base := snapshotsPath(org, v.ID) + "/" + sn.ID

	if rec := e.send("POST", base+"/fork", adminTok, `{"name":"stolen"}`); rec.Code != 403 {
		t.Fatalf("admin fork: %d %s", rec.Code, rec.Body.String())
	}
	other := e.takeSnapshot(adminTok, org, v.ID, `{"name":"by-admin"}`)
	if rec := e.do("DELETE", snapshotsPath(org, v.ID)+"/"+other.ID, adminTok); rec.Code != 204 {
		t.Fatalf("admin delete snapshot: %d %s", rec.Code, rec.Body.String())
	}
	rec := e.send("POST", base+"/restore", adminTok, `{"restartAllowed":true}`)
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), `"restoring":true`) {
		t.Fatalf("admin restore: %d %s", rec.Code, rec.Body.String())
	}
	if rec := e.do("GET", vmsPath(org)+"/"+v.ID, adminTok); !strings.Contains(rec.Body.String(), `"restoring":true`) {
		t.Fatalf("vm during restore: %s", rec.Body.String())
	}
	if es := e.audit(store.ActionVMRestore); len(es) != 1 || es[0].Details["snapshot"] != "base" || es[0].Details["memory"] != false {
		t.Fatalf("restore audit: %+v", es)
	}
}

func TestTeamMemberSnapshotNotRestore(t *testing.T) {
	e := newEnv(t)
	_, ownerTok := e.user("alice")
	bob, bobTok := e.user("bob")
	carol, carolTok := e.user("carol")
	org := e.createOrg(ownerTok, "Acme")
	e.addMember(org, bob, store.RoleMember)
	e.addMember(org, carol, store.RoleMember)
	core := e.createTeam(ownerTok, org, "core")
	e.makeTeamAdmin(ownerTok, org, core, carol)
	if rec := e.send("PUT", teamMemberPath(org, core, bob), ownerTok, `{"admin":false}`); rec.Code != 204 {
		t.Fatal(rec.Code)
	}
	v := e.createVM(carolTok, org, vmBody("t1", teamOwner(core), e.enrolledNode(ownerTok, org, "n1")))
	sn := e.takeSnapshot(bobTok, org, v.ID, `{"name":"base"}`)
	e.snapshotsReady()
	base := snapshotsPath(org, v.ID) + "/" + sn.ID

	if rec := e.send("POST", base+"/restore", bobTok, `{"restartAllowed":true}`); rec.Code != 403 {
		t.Fatalf("member restore: %d", rec.Code)
	}
	if rec := e.send("POST", base+"/fork", bobTok, `{"name":"t2"}`); rec.Code != 403 {
		t.Fatalf("member fork: %d", rec.Code)
	}
	rec := e.send("POST", base+"/fork", carolTok, `{"name":"t2"}`)
	if rec.Code != 201 {
		t.Fatalf("team admin fork: %d %s", rec.Code, rec.Body.String())
	}
	var fork vmJSONTest
	decode(t, rec, &fork)
	if fork.Name != "t2" || fork.Owner.Kind != "team" || fork.Owner.ID != core.String() {
		t.Fatalf("fork: %+v", fork)
	}
	if es := e.audit(store.ActionVMFork); len(es) != 2 || es[0].Result != store.ResultAllowed || es[0].Resource != "org/vms/"+v.ID || es[0].Details["vm"] != fork.ID {
		t.Fatalf("fork audit: %+v", es)
	}
	if es := e.audit(store.ActionVMCreate); len(es) != 2 || es[0].Resource != "org/vms/"+fork.ID {
		t.Fatalf("create audit: %+v", es)
	}
}

func TestForkDenyRule(t *testing.T) {
	e := newEnv(t)
	_, ownerTok := e.user("alice")
	bob, bobTok := e.user("bob")
	org := e.createOrg(ownerTok, "Acme")
	e.addMember(org, bob, store.RoleMember)
	v := e.createVM(bobTok, org, vmBody("bobvm", userOwner(), e.enrolledNode(ownerTok, org, "n1")))
	sn := e.takeSnapshot(bobTok, org, v.ID, `{"name":"base"}`)
	e.snapshotsReady()
	role := e.createRole(ownerTok, org, `{"name":"nofork","rules":[{"effect":"deny","object":"org/vms/*","action":"vm:fork"}]}`)
	e.bind(ownerTok, org, "user:"+bob.ID.String(), role)
	if rec := e.send("POST", snapshotsPath(org, v.ID)+"/"+sn.ID+"/fork", bobTok, `{"name":"copy"}`); rec.Code != 403 {
		t.Fatalf("denied fork: %d %s", rec.Code, rec.Body.String())
	}
}

func TestRestoreRequiresRestartAllowed(t *testing.T) {
	e := newEnv(t)
	_, tok := e.user("alice")
	org := e.createOrg(tok, "Acme")
	v := e.createVM(tok, org, vmBody("a", userOwner(), e.enrolledNode(tok, org, "n1")))
	sn := e.takeSnapshot(tok, org, v.ID, `{"name":"base","memory":true}`)
	e.snapshotsReady()
	var list []snapshotTest
	decode(t, e.do("GET", snapshotsPath(org, v.ID), tok), &list)
	if len(list) != 1 || !list[0].CanRestoreMemory {
		t.Fatalf("list: %+v", list)
	}
	if rec := e.send("POST", snapshotsPath(org, v.ID)+"/"+sn.ID+"/restore", tok, `{"memory":true,"restartAllowed":false}`); rec.Code != 409 {
		t.Fatalf("restore running vm: %d %s", rec.Code, rec.Body.String())
	}
}

func TestDeleteVMWithPendingFork(t *testing.T) {
	e := newEnv(t)
	_, tok := e.user("alice")
	org := e.createOrg(tok, "Acme")
	v := e.createVM(tok, org, vmBody("a", userOwner(), e.enrolledNode(tok, org, "n1")))
	sn := e.takeSnapshot(tok, org, v.ID, `{"name":"base"}`)
	e.snapshotsReady()
	if rec := e.send("POST", snapshotsPath(org, v.ID)+"/"+sn.ID+"/fork", tok, `{"name":"b"}`); rec.Code != 201 {
		t.Fatalf("fork: %d %s", rec.Code, rec.Body.String())
	}
	rec := e.do("DELETE", vmsPath(org)+"/"+v.ID, tok)
	if rec.Code != 409 || !strings.Contains(rec.Body.String(), "fork") {
		t.Fatalf("delete source of pending fork: %d %s", rec.Code, rec.Body.String())
	}
}

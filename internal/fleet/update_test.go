package fleet_test

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/asterism-dev/observatory/internal/fleet"
	"github.com/asterism-dev/observatory/internal/proto"
	"github.com/asterism-dev/observatory/internal/store"
	"github.com/asterism-dev/observatory/internal/version"
)

const oldVersion = "v0.4.0"

var hv = &proto.Hypervisor{Type: "cloud-hypervisor", Version: "v48.0"}

// The hub version is global; every fleet test sees it, and a run goroutine outliving its test must never race a write.
func init() { version.Version = "v0.5.0" }

type updNode struct {
	store.Node
	cred string
	conn *fakeNode
}

// updEnv has outdated, updatable nodes connected, with the run timeouts shortened.
func updEnv(t *testing.T, names ...string) (*env, []*updNode) {
	t.Helper()
	e := newEnv(t)
	fleet.SetUpdateTimeouts(e.f, 2*time.Second, 2*time.Second, 10*time.Millisecond)
	var out []*updNode
	for _, name := range names {
		n, cred := e.node(e.org.ID, name)
		c := e.register(cred, proto.Register{Version: oldVersion, Hypervisor: hv})
		c.desired()
		out = append(out, &updNode{Node: e.storedNode(n.ID), cred: cred, conn: c})
	}
	return e, out
}

func (e *env) storedNode(id uuid.UUID) store.Node {
	e.t.Helper()
	n, err := e.s.Node(context.Background(), e.org.ID, id)
	if err != nil {
		e.t.Fatal(err)
	}
	return n
}

func (e *env) start(nodes ...*updNode) fleet.UpdateRun {
	e.t.Helper()
	var ns []store.Node
	for _, n := range nodes {
		ns = append(ns, n.Node)
	}
	run, err := e.f.StartUpdate(context.Background(), e.org.ID, e.owner, ns)
	if err != nil {
		e.t.Fatal(err)
	}
	return run
}

// back reconnects the node as the new release would.
func (e *env) back(n *updNode, ver string, vms ...proto.VMStatus) {
	e.t.Helper()
	n.conn = e.register(n.cred, proto.Register{Version: ver, Hypervisor: hv, VMs: vms})
	n.conn.desired()
}

func (e *env) run() fleet.UpdateRun {
	e.t.Helper()
	r, ok := e.f.CurrentUpdate(e.org.ID)
	if !ok {
		e.t.Fatal("no run")
	}
	return r
}

func (e *env) ended() fleet.UpdateRun {
	e.t.Helper()
	waitFor(e.t, func() bool { return e.run().State != "running" })
	return e.run()
}

// awaitUpdate skips everything but the update message.
func (n *fakeNode) awaitUpdate() {
	n.t.Helper()
	for n.next().Type != "update" {
	}
}

func (n *fakeNode) noUpdate() {
	n.t.Helper()
	for {
		select {
		case m, ok := <-n.msgs:
			if ok && m.Type == "update" {
				n.t.Fatal("unexpected update message")
			}
			if !ok {
				return
			}
		case <-time.After(50 * time.Millisecond):
			return
		}
	}
}

func statuses(r fleet.UpdateRun) []string {
	var out []string
	for _, n := range r.Nodes {
		out = append(out, n.Status)
	}
	return out
}

func running(v store.VM) proto.VMStatus {
	return proto.VMStatus{ID: v.ID.String(), State: proto.StateRunning}
}

func TestRegisterStoresHypervisor(t *testing.T) {
	e := newEnv(t)
	n, cred := e.node(e.org.ID, "n")
	c := e.register(cred, proto.Register{Hypervisor: hv})
	c.desired()
	if got := e.storedNode(n.ID).Hypervisor; got == nil || *got != *hv {
		t.Fatalf("hypervisor %+v", got)
	}
	e.register(cred, proto.Register{}).desired()
	if got := e.storedNode(n.ID).Hypervisor; got != nil {
		t.Fatalf("hypervisor not cleared: %+v", got)
	}
}

func TestUpdateRunHappyPath(t *testing.T) {
	e, ns := updEnv(t, "n1", "n2")
	v1, v2 := e.vm(e.org.ID, ns[0].ID, "a", &e.owner, nil), e.vm(e.org.ID, ns[1].ID, "b", &e.owner, nil)
	run := e.start(ns...)
	if run.State != "running" || run.StartedBy != e.owner || len(run.Nodes) != 2 || run.Nodes[0].ID != ns[0].ID {
		t.Fatalf("run %+v", run)
	}
	ns[0].conn.awaitUpdate()
	waitFor(t, func() bool { return e.f.Updating(ns[0].ID) })
	if e.f.Updating(ns[1].ID) {
		t.Fatal("n2 updating before n1 is done")
	}
	ns[1].conn.noUpdate()
	e.back(ns[0], version.Version, running(v1))
	ns[1].conn.awaitUpdate()
	if s := statuses(e.run()); s[0] != "done" || s[1] != "updating" {
		t.Fatalf("statuses %v when n2 got its update", s)
	}
	e.back(ns[1], version.Version, running(v2))
	r := e.ended()
	if r.State != "done" || statuses(r)[0] != "done" || statuses(r)[1] != "done" || r.EndedAt == nil {
		t.Fatalf("run %+v", r)
	}
	if e.f.Updating(ns[0].ID) || e.f.Updating(ns[1].ID) {
		t.Fatal("still updating after the run")
	}
	entries, err := e.s.AuditEntries(context.Background(), store.AuditFilter{Action: store.ActionNodeUpdate, OrgID: &e.org.ID})
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 {
		t.Fatalf("%d audit entries", len(entries))
	}
	for _, a := range entries {
		if a.ActorID != e.owner.String() || a.Details["update"] != "done" || a.Details["run"] != run.ID.String() {
			t.Errorf("audit %+v", a)
		}
	}
}

func TestUpdateIgnoresOldVersionReconnect(t *testing.T) {
	e, ns := updEnv(t, "n1")
	e.start(ns...)
	ns[0].conn.awaitUpdate()
	e.back(ns[0], oldVersion)
	time.Sleep(100 * time.Millisecond)
	if r := e.run(); r.State != "running" || statuses(r)[0] != "updating" {
		t.Fatalf("run %+v after an old-version reconnect", r)
	}
	e.back(ns[0], version.Version)
	if r := e.ended(); r.State != "done" {
		t.Fatalf("run %+v", r)
	}
}

func TestUpdateWaitsForVMStatusesAfterReconnect(t *testing.T) {
	e, ns := updEnv(t, "n1")
	v := e.vm(e.org.ID, ns[0].ID, "a", &e.owner, nil)
	if err := e.s.SetVMStatus(context.Background(), ns[0].ID, running(v)); err != nil {
		t.Fatal(err)
	}
	e.start(ns...)
	ns[0].conn.awaitUpdate()
	e.back(ns[0], version.Version)
	time.Sleep(100 * time.Millisecond)
	if r := e.run(); r.State != "running" || statuses(r)[0] != "updating" {
		t.Fatalf("run %+v before any VM status", r)
	}
	ns[0].conn.send(proto.Message{Type: "status", Status: &proto.VMStatus{ID: v.ID.String(), State: proto.StateRunning}})
	if r := e.ended(); r.State != "done" {
		t.Fatalf("run %+v", r)
	}
}

// A status landing between vmsBack's two reads must not let the VM's stale stored state count.
func TestUpdateJudgesOnlyStatesWrittenOnNewConnection(t *testing.T) {
	e, ns := updEnv(t, "n1")
	fleet.SetVMErrorGrace(e.f, 50*time.Millisecond)
	v := e.vm(e.org.ID, ns[0].ID, "web", &e.owner, nil)
	if err := e.s.SetVMStatus(context.Background(), ns[0].ID, running(v)); err != nil {
		t.Fatal(err)
	}
	conns := make(chan *fakeNode, 1)
	var once sync.Once
	fleet.SetBetweenVMReads(e.f, func() {
		once.Do(func() {
			(<-conns).send(proto.Message{Type: "status", Status: &proto.VMStatus{ID: v.ID.String(), State: proto.StateError, Error: "boom"}})
			for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline) && !fleet.Seen(e.f, ns[0].ID, v.ID); time.Sleep(5 * time.Millisecond) {
			}
		})
	})
	e.start(ns...)
	ns[0].conn.awaitUpdate()
	e.back(ns[0], version.Version)
	conns <- ns[0].conn
	if r := e.ended(); r.State != "failed" || !strings.Contains(r.Nodes[0].Reason, "boom") {
		t.Fatalf("run %+v", r)
	}
}

func TestStatusSeenOnlyWhenStored(t *testing.T) {
	e, ns := updEnv(t, "n1")
	bad := e.vm(e.org.ID, ns[0].ID, "bad", &e.owner, nil)
	good := e.vm(e.org.ID, ns[0].ID, "good", &e.owner, nil)
	// Postgres rejects NUL bytes in text, so this status write fails.
	ns[0].conn.send(proto.Message{Type: "status", Status: &proto.VMStatus{ID: bad.ID.String(), State: proto.StateError, Error: "a\x00b"}})
	ns[0].conn.send(proto.Message{Type: "status", Status: &proto.VMStatus{ID: good.ID.String(), State: proto.StateRunning}})
	waitFor(t, func() bool { return fleet.Seen(e.f, ns[0].ID, good.ID) })
	if fleet.Seen(e.f, ns[0].ID, bad.ID) {
		t.Fatal("vm seen although its status was not stored")
	}
}

func TestUpdateFailsOnReport(t *testing.T) {
	e, ns := updEnv(t, "n1", "n2")
	e.start(ns...)
	ns[0].conn.awaitUpdate()
	ns[0].conn.send(proto.Message{Type: "update", Update: &proto.UpdateReport{Error: "apt failed"}})
	r := e.ended()
	if r.State != "failed" || r.Nodes[0].Status != "failed" || r.Nodes[0].Reason != "apt failed" || r.Nodes[1].Status != "skipped" {
		t.Fatalf("run %+v", r)
	}
	ns[1].conn.noUpdate()
	if _, ok := fleet.UpdateErr(e.f, ns[0].ID); ok {
		t.Fatal("report kept after the node's update ended")
	}
	entries, err := e.s.AuditEntries(context.Background(), store.AuditFilter{Action: store.ActionNodeUpdate, OrgID: &e.org.ID})
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Details["update"] != "failed" || entries[0].Details["reason"] != "apt failed" {
		t.Fatalf("audit %+v", entries)
	}
}

func TestUpdateFailsOnEmptyReport(t *testing.T) {
	e, ns := updEnv(t, "n1")
	e.start(ns...)
	ns[0].conn.awaitUpdate()
	ns[0].conn.send(proto.Message{Type: "update", Update: &proto.UpdateReport{}})
	if r := e.ended(); r.State != "failed" || r.Nodes[0].Reason == "" {
		t.Fatalf("run %+v", r)
	}
}

func TestUpdateFailsOnVMError(t *testing.T) {
	e, ns := updEnv(t, "n1")
	fleet.SetVMErrorGrace(e.f, 50*time.Millisecond)
	v := e.vm(e.org.ID, ns[0].ID, "web", &e.owner, nil)
	e.start(ns...)
	ns[0].conn.awaitUpdate()
	e.back(ns[0], version.Version, proto.VMStatus{ID: v.ID.String(), State: proto.StateError, Error: "boom"})
	r := e.ended()
	if r.State != "failed" || r.Nodes[0].Reason != "vm web: boom" {
		t.Fatalf("run %+v", r)
	}
}

// VMs with disks report errors while the underlay comes up after a restart, then recover.
func TestUpdateToleratesTransientVMError(t *testing.T) {
	e, ns := updEnv(t, "n1")
	fleet.SetVMErrorGrace(e.f, time.Second)
	v := e.vm(e.org.ID, ns[0].ID, "web", &e.owner, nil)
	e.start(ns...)
	ns[0].conn.awaitUpdate()
	e.back(ns[0], version.Version, proto.VMStatus{ID: v.ID.String(), State: proto.StateError, Error: "storage not ready"})
	time.Sleep(100 * time.Millisecond)
	if r := e.run(); r.State != "running" {
		t.Fatalf("run %+v within the error grace", r)
	}
	ns[0].conn.send(proto.Message{Type: "status", Status: &proto.VMStatus{ID: v.ID.String(), State: proto.StateRunning}})
	if r := e.ended(); r.State != "done" {
		t.Fatalf("run %+v", r)
	}
}

func TestUpdateTimeoutNamesLastVMError(t *testing.T) {
	e, ns := updEnv(t, "n1")
	fleet.SetUpdateTimeouts(e.f, 2*time.Second, 50*time.Millisecond, 10*time.Millisecond)
	v := e.vm(e.org.ID, ns[0].ID, "web", &e.owner, nil)
	e.start(ns...)
	ns[0].conn.awaitUpdate()
	e.back(ns[0], version.Version, proto.VMStatus{ID: v.ID.String(), State: proto.StateError, Error: "storage unreachable"})
	if r := e.ended(); r.State != "failed" || !strings.Contains(r.Nodes[0].Reason, "timed out") || !strings.Contains(r.Nodes[0].Reason, "storage unreachable") {
		t.Fatalf("run %+v", r)
	}
}

func TestUpdateTimesOut(t *testing.T) {
	e, ns := updEnv(t, "n1")
	fleet.SetUpdateTimeouts(e.f, 50*time.Millisecond, 50*time.Millisecond, 10*time.Millisecond)
	e.start(ns...)
	r := e.ended()
	if r.State != "failed" || !strings.Contains(r.Nodes[0].Reason, "timed out") {
		t.Fatalf("run %+v", r)
	}
}

func TestUpdateTimesOutWaitingForVMs(t *testing.T) {
	e, ns := updEnv(t, "n1")
	fleet.SetUpdateTimeouts(e.f, 2*time.Second, 50*time.Millisecond, 10*time.Millisecond)
	e.vm(e.org.ID, ns[0].ID, "web", &e.owner, nil)
	e.start(ns...)
	ns[0].conn.awaitUpdate()
	e.back(ns[0], version.Version)
	r := e.ended()
	if r.State != "failed" || !strings.Contains(r.Nodes[0].Reason, "timed out") || !strings.Contains(r.Nodes[0].Reason, "web") {
		t.Fatalf("run %+v", r)
	}
}

func TestUpdateFailsWhenNodeOffline(t *testing.T) {
	e, ns := updEnv(t, "n1", "n2")
	e.start(ns...)
	ns[0].conn.awaitUpdate()
	ns[1].conn.sess.Close()
	waitFor(t, func() bool { return !e.f.Online(ns[1].ID) })
	e.back(ns[0], version.Version)
	r := e.ended()
	if r.State != "failed" || r.Nodes[0].Status != "done" || r.Nodes[1].Status != "failed" || r.Nodes[1].Reason != "node offline" {
		t.Fatalf("run %+v", r)
	}
}

func TestUpdateFailsFastOnDeadConnection(t *testing.T) {
	e, ns := updEnv(t, "n1")
	fleet.AttachDeadConn(e.f, ns[0].ID, e.org.ID)
	e.start(ns...)
	if r := e.ended(); r.State != "failed" || r.Nodes[0].Reason != "node offline" {
		t.Fatalf("run %+v", r)
	}
}

func TestUpdateFailsWhenRequestNotTaken(t *testing.T) {
	e, ns := updEnv(t, "n1")
	fleet.SetUpdateTimeouts(e.f, 100*time.Millisecond, 2*time.Second, 10*time.Millisecond)
	fleet.AttachStalledConn(e.f, ns[0].ID, e.org.ID)
	e.start(ns...)
	if r := e.ended(); r.State != "failed" || !strings.Contains(r.Nodes[0].Reason, "did not take the update request") {
		t.Fatalf("run %+v", r)
	}
}

func TestUpdateRequestFollowsReconnect(t *testing.T) {
	e, ns := updEnv(t, "n1")
	end := fleet.AttachStalledConn(e.f, ns[0].ID, e.org.ID)
	e.start(ns...)
	waitFor(t, func() bool { return e.f.Updating(ns[0].ID) })
	e.back(ns[0], oldVersion)
	end()
	ns[0].conn.awaitUpdate()
	e.back(ns[0], version.Version)
	if r := e.ended(); r.State != "done" {
		t.Fatalf("run %+v", r)
	}
}

func TestStartUpdateAfterClose(t *testing.T) {
	e, ns := updEnv(t, "n1")
	e.f.Close()
	if _, err := e.f.StartUpdate(context.Background(), e.org.ID, e.owner, []store.Node{ns[0].Node}); !errors.Is(err, fleet.ErrClosed) {
		t.Fatalf("err %v", err)
	}
}

func TestCloseStopsUpdateRun(t *testing.T) {
	e, ns := updEnv(t, "n1")
	e.start(ns...)
	ns[0].conn.awaitUpdate()
	start := time.Now()
	e.f.Close()
	if d := time.Since(start); d > time.Second {
		t.Fatalf("Close took %s", d)
	}
}

func TestUpdateCancel(t *testing.T) {
	e, ns := updEnv(t, "n1", "n2")
	e.start(ns...)
	ns[0].conn.awaitUpdate()
	if !e.f.CancelUpdate(e.org.ID) {
		t.Fatal("cancel refused")
	}
	e.back(ns[0], version.Version)
	r := e.ended()
	if r.State != "cancelled" || r.Nodes[0].Status != "done" || r.Nodes[1].Status != "skipped" {
		t.Fatalf("run %+v", r)
	}
	ns[1].conn.noUpdate()
	if e.f.CancelUpdate(e.org.ID) {
		t.Fatal("cancelled an ended run")
	}
}

func TestStartUpdateRejects(t *testing.T) {
	ctx := context.Background()
	e, ns := updEnv(t, "n1", "n2")
	fleet.SetUpdateTimeouts(e.f, 500*time.Millisecond, 500*time.Millisecond, 10*time.Millisecond)

	plain, cred := e.node(e.org.ID, "plain")
	e.register(cred, proto.Register{Version: oldVersion}).desired()
	current, cred := e.node(e.org.ID, "current")
	e.register(cred, proto.Register{Version: version.Version, Hypervisor: hv}).desired()
	gone, cred := e.node(e.org.ID, "gone")
	c := e.register(cred, proto.Register{Version: oldVersion, Hypervisor: hv})
	c.desired()
	c.sess.Close()
	waitFor(t, func() bool { return !e.f.Online(gone.ID) })
	for _, id := range []uuid.UUID{plain.ID, current.ID, gone.ID} {
		n := e.storedNode(id)
		if e.f.Updatable(n) {
			t.Errorf("%s updatable", n.Name)
		}
		if _, err := e.f.StartUpdate(ctx, e.org.ID, e.owner, []store.Node{ns[0].Node, n}); !errors.Is(err, fleet.ErrNotUpdatable) {
			t.Errorf("%s: err %v", n.Name, err)
		}
	}
	if _, ok := e.f.CurrentUpdate(e.org.ID); ok {
		t.Fatal("rejected start left a run")
	}

	e.start(ns[0])
	for _, n := range []store.Node{ns[1].Node, e.storedNode(plain.ID)} {
		if _, err := e.f.StartUpdate(ctx, e.org.ID, e.owner, []store.Node{n}); !errors.Is(err, fleet.ErrUpdateRunning) {
			t.Errorf("%s during a run: err %v", n.Name, err)
		}
	}
}

func (e *env) power(v store.VM, p proto.Power) {
	e.t.Helper()
	if _, err := e.s.SetVMPower(context.Background(), e.org.ID, v.ID, p, ""); err != nil {
		e.t.Fatal(err)
	}
}

func TestUpdateSettlesVMsByPower(t *testing.T) {
	e, ns := updEnv(t, "n1")
	stopped, paused := e.vm(e.org.ID, ns[0].ID, "stopped", &e.owner, nil), e.vm(e.org.ID, ns[0].ID, "paused", &e.owner, nil)
	hib, hibStopped := e.vm(e.org.ID, ns[0].ID, "hib", &e.owner, nil), e.vm(e.org.ID, ns[0].ID, "hib-stopped", &e.owner, nil)
	e.power(paused, proto.PowerPaused)
	e.power(hib, proto.PowerHibernated)
	e.power(hibStopped, proto.PowerHibernated)
	e.power(stopped, proto.PowerStopped)
	e.start(ns...)
	ns[0].conn.awaitUpdate()
	e.back(ns[0], version.Version,
		proto.VMStatus{ID: stopped.ID.String(), State: proto.StateStopped},
		proto.VMStatus{ID: paused.ID.String(), State: proto.StatePaused},
		proto.VMStatus{ID: hib.ID.String(), State: proto.StateHibernated},
		proto.VMStatus{ID: hibStopped.ID.String(), State: proto.StateStopped})
	if r := e.ended(); r.State != "done" {
		t.Fatalf("run %+v", r)
	}
}

func TestUpdateWaitsForVMInItsPowerState(t *testing.T) {
	e, ns := updEnv(t, "n1")
	fleet.SetUpdateTimeouts(e.f, 2*time.Second, 50*time.Millisecond, 10*time.Millisecond)
	v := e.vm(e.org.ID, ns[0].ID, "paused", &e.owner, nil)
	e.power(v, proto.PowerPaused)
	e.start(ns...)
	ns[0].conn.awaitUpdate()
	e.back(ns[0], version.Version, running(v))
	if r := e.ended(); r.State != "failed" || !strings.Contains(r.Nodes[0].Reason, "waiting for vm paused") {
		t.Fatalf("run %+v", r)
	}
}

func TestUpdateIgnoresVMDeletedMidUpdate(t *testing.T) {
	e, ns := updEnv(t, "n1")
	v := e.vm(e.org.ID, ns[0].ID, "web", &e.owner, nil)
	e.start(ns...)
	ns[0].conn.awaitUpdate()
	e.back(ns[0], version.Version)
	if err := e.s.DeleteVM(context.Background(), e.org.ID, v.ID); err != nil {
		t.Fatal(err)
	}
	if r := e.ended(); r.State != "done" {
		t.Fatalf("run %+v", r)
	}
}

func TestUpdateCancelDuringLastNodeEndsDone(t *testing.T) {
	e, ns := updEnv(t, "n1")
	e.start(ns...)
	ns[0].conn.awaitUpdate()
	if !e.f.CancelUpdate(e.org.ID) {
		t.Fatal("cancel refused")
	}
	e.back(ns[0], version.Version)
	if r := e.ended(); r.State != "done" || r.Nodes[0].Status != "done" {
		t.Fatalf("run %+v", r)
	}
}

func TestUpdateSurvivesReconnectDuringVMWait(t *testing.T) {
	e, ns := updEnv(t, "n1")
	v := e.vm(e.org.ID, ns[0].ID, "web", &e.owner, nil)
	e.start(ns...)
	ns[0].conn.awaitUpdate()
	e.back(ns[0], version.Version)
	ns[0].conn.sess.Close()
	waitFor(t, func() bool { return !e.f.Online(ns[0].ID) })
	e.back(ns[0], version.Version, running(v))
	if r := e.ended(); r.State != "done" {
		t.Fatalf("run %+v", r)
	}
}

package node

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/asterism-dev/observatory/internal/proto"
)

func dvSuspend(power proto.Power, gen uint64, minutes int) proto.DesiredVM {
	d := dv("a", "a", power, gen)
	d.AutoSuspendMinutes = minutes
	return d
}

// suspendedVM runs VM "a" with auto-suspend and lets it go idle.
func suspendedVM(t *testing.T, a *Agent, p *fakeProvider) {
	t.Helper()
	freeSpace(t, 1<<40)
	a.reconcile(context.Background(), 0, []proto.DesiredVM{dvSuspend(proto.PowerRunning, 1, 15)})
	waitState(t, a, "a", proto.StateRunning, 1)
	p.idle <- struct{}{}
	waitState(t, a, "a", proto.StateSuspended, 1)
	eventually(t, "waiting for wake", func() bool { return p.hasEvent("wait-wake") })
}

func TestAutoSuspendHibernatesWhenIdle(t *testing.T) {
	a, p := newReconcileAgent(t)
	suspendedVM(t, a, p)
	dir := VMDir(a.VMsDir, "a")
	ev := strings.Join(p.gotEvents(), ",")
	if s, k := strings.Index(ev, "savestate:"+filepath.Join(dir, "state.tmp")), strings.Index(ev, "kill-guest"); s < 0 || k < s {
		t.Fatalf("events = %s", ev)
	}
	b, err := os.ReadFile(filepath.Join(dir, "vm.json"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), `"suspended": true`) || !strings.Contains(string(b), `"hibernated": true`) {
		t.Fatalf("vm.json = %s", b)
	}
}

func TestWakeOnTraffic(t *testing.T) {
	a, p := newReconcileAgent(t)
	suspendedVM(t, a, p)
	p.wake <- struct{}{}
	waitState(t, a, "a", proto.StateRunning, 1)
	if !p.hasEvent("restore:" + filepath.Join(VMDir(a.VMsDir, "a"), StateDir)) {
		t.Fatalf("events = %v", p.gotEvents())
	}
	if f := loadVM(t, a, "a"); f.Suspended || f.Hibernated {
		t.Fatalf("vm.json = %+v", f)
	}
}

func TestWakeSeqResumes(t *testing.T) {
	a, p := newReconcileAgent(t)
	suspendedVM(t, a, p)
	d := dvSuspend(proto.PowerRunning, 1, 15)
	d.WakeSeq = 1
	a.reconcile(context.Background(), 0, []proto.DesiredVM{d})
	eventually(t, "wake cancelled", func() bool { return p.hasEvent("wait-wake-cancelled") })
	waitState(t, a, "a", proto.StateRunning, 1)
	if !p.hasEvent("restore:" + filepath.Join(VMDir(a.VMsDir, "a"), StateDir)) {
		t.Fatalf("events = %v", p.gotEvents())
	}
}

func TestAutoSuspendOffStopsWatcher(t *testing.T) {
	a, p := newReconcileAgent(t)
	a.reconcile(context.Background(), 0, []proto.DesiredVM{dvSuspend(proto.PowerRunning, 1, 15)})
	waitState(t, a, "a", proto.StateRunning, 1)
	eventually(t, "watching", func() bool { return p.hasEvent("wait-idle") })
	a.reconcile(context.Background(), 0, []proto.DesiredVM{dvSuspend(proto.PowerRunning, 1, 0)})
	eventually(t, "watcher cancelled", func() bool { return p.hasEvent("wait-idle-cancelled") })
	if ev := strings.Join(p.gotEvents(), ","); strings.Contains(ev, "savestate:") {
		t.Fatalf("events = %s", ev)
	}
}

func TestExplicitHibernateNoWake(t *testing.T) {
	a, p := newReconcileAgent(t)
	freeSpace(t, 1<<40)
	a.reconcile(context.Background(), 0, []proto.DesiredVM{dvSuspend(proto.PowerRunning, 1, 15)})
	waitState(t, a, "a", proto.StateRunning, 1)
	a.reconcile(context.Background(), 0, []proto.DesiredVM{dvSuspend(proto.PowerHibernated, 2, 15)})
	waitState(t, a, "a", proto.StateHibernated, 2)
	eventually(t, "watcher cancelled", func() bool { return p.hasEvent("wait-idle-cancelled") })
	time.Sleep(100 * time.Millisecond)
	if p.hasEvent("wait-wake") || loadVM(t, a, "a").Suspended {
		t.Fatalf("events = %v", p.gotEvents())
	}
}

func TestAutoSuspendSkipsPausedVM(t *testing.T) {
	a, p := newReconcileAgent(t)
	freeSpace(t, 1<<40)
	a.reconcile(context.Background(), 0, []proto.DesiredVM{dvSuspend(proto.PowerRunning, 1, 15)})
	waitState(t, a, "a", proto.StateRunning, 1)
	a.reconcile(context.Background(), 0, []proto.DesiredVM{dvSuspend(proto.PowerPaused, 2, 15)})
	waitState(t, a, "a", proto.StatePaused, 2)
	p.idle <- struct{}{}
	eventually(t, "watching again", func() bool {
		n := 0
		for _, e := range p.gotEvents() {
			if e == "wait-idle" {
				n++
			}
		}
		return n >= 2
	})
	if ev := strings.Join(p.gotEvents(), ","); strings.Contains(ev, "savestate:") {
		t.Fatalf("events = %s", ev)
	}
}

func TestNodeStopKeepsSuspended(t *testing.T) {
	a, p := newReconcileAgent(t)
	suspendedVM(t, a, p)
	a.stopAll()
	if f := loadVM(t, a, "a"); !f.Suspended || !f.Hibernated {
		t.Fatalf("vm.json = %+v", f)
	}
	if _, err := os.Stat(filepath.Join(VMDir(a.VMsDir, "a"), StateDir, MemFile)); err != nil {
		t.Fatal(err)
	}
	if p.hasEvent("restore:" + filepath.Join(VMDir(a.VMsDir, "a"), StateDir)) {
		t.Fatalf("resumed: %v", p.gotEvents())
	}
}

func TestSuspendedWaitsForWakeAfterNodeStart(t *testing.T) {
	a, p := newReconcileAgent(t)
	f := cachedVM("a", "a")
	f.Suspended, f.AutoSuspendMinutes = true, 15
	state := saveHibernated(t, a, f)
	if err := a.restoreAll(context.Background()); err != nil {
		t.Fatal(err)
	}
	waitState(t, a, "a", proto.StateSuspended, 1)
	eventually(t, "waiting for wake", func() bool { return p.hasEvent("wait-wake") })
	if p.hasEvent("restore:" + state) {
		t.Fatalf("resumed without traffic: %v", p.gotEvents())
	}
	p.wake <- struct{}{}
	waitState(t, a, "a", proto.StateRunning, 1)
	if !p.hasEvent("restore:" + state) {
		t.Fatalf("events = %v", p.gotEvents())
	}
	eventually(t, "watching idle again", func() bool {
		ev := p.gotEvents()
		return ev[len(ev)-1] == "wait-idle"
	})
}

func TestDesiredWakeWaitsForTapTeardown(t *testing.T) {
	a, p := newReconcileAgent(t)
	suspendedVM(t, a, p)
	block := make(chan struct{})
	p.mu.Lock()
	p.wakeTeardown = block
	p.mu.Unlock()
	done := make(chan struct{})
	go func() {
		a.reconcile(context.Background(), 0, []proto.DesiredVM{dvSuspend(proto.PowerRunning, 2, 15)})
		close(done)
	}()
	eventually(t, "teardown started", func() bool { return p.hasEvent("wake-teardown-begin") })
	time.Sleep(100 * time.Millisecond)
	state := "restore:" + filepath.Join(VMDir(a.VMsDir, "a"), StateDir)
	if p.hasEvent(state) {
		t.Fatalf("restored during the tap teardown: %v", p.gotEvents())
	}
	close(block)
	<-done
	waitState(t, a, "a", proto.StateRunning, 2)
	ev := strings.Join(p.gotEvents(), ",")
	if e, r := strings.Index(ev, "wake-teardown-end"), strings.Index(ev, state); e < 0 || r < e {
		t.Fatalf("events = %s", ev)
	}
}

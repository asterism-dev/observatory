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

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func withSnaps(d proto.DesiredVM, refs ...proto.SnapshotRef) proto.DesiredVM {
	d.Snapshots = refs
	return d
}

// runningVM boots VM "a" with a root.img holding root.
func runningVM(t *testing.T, a *Agent, root string) string {
	t.Helper()
	freeSpace(t, 1<<40)
	a.reconcile(context.Background(), 0, []proto.DesiredVM{dv("a", "a", proto.PowerRunning, 1)})
	waitState(t, a, "a", proto.StateRunning, 1)
	dir := VMDir(a.VMsDir, "a")
	writeFile(t, filepath.Join(dir, "root.img"), root)
	return dir
}

func waitSnapshot(t *testing.T, a *Agent, id string) proto.SnapshotInfo {
	t.Helper()
	var got proto.SnapshotInfo
	eventually(t, "snapshot "+id+" reported", func() bool {
		st, _ := a.vmStatus("a")
		for _, s := range st.Snapshots {
			if s.ID == id {
				got = s
				return true
			}
		}
		return false
	})
	return got
}

func TestSnapshotLiveVMPausesUntilComplete(t *testing.T) {
	a, p := newReconcileAgent(t)
	dir := runningVM(t, a, "old")
	s1 := proto.SnapshotRef{ID: "s1", Memory: true}
	a.reconcile(context.Background(), 0, []proto.DesiredVM{withSnaps(dv("a", "a", proto.PowerRunning, 1), s1)})
	if info := waitSnapshot(t, a, "s1"); !info.Memory || info.Error != "" {
		t.Fatalf("info = %+v", info)
	}
	if !p.hasEvent("savestate:" + filepath.Join(dir, SnapshotsDir, "s1.tmp")) {
		t.Fatalf("events = %v", p.gotEvents())
	}
	if got := readFile(t, filepath.Join(dir, SnapshotsDir, "s1", "root.img")); got != "old" {
		t.Fatalf("snapshot root = %q", got)
	}
	time.Sleep(50 * time.Millisecond)
	if n := countEvent(p, "resume"); n != 0 {
		t.Fatalf("resumed %d times before complete", n)
	}
	s1.Complete = true
	a.reconcile(context.Background(), 0, []proto.DesiredVM{withSnaps(dv("a", "a", proto.PowerRunning, 1), s1)})
	eventually(t, "resume", func() bool { return countEvent(p, "resume") == 1 })
	a.reconcile(context.Background(), 0, []proto.DesiredVM{withSnaps(dv("a", "a", proto.PowerRunning, 1), s1)})
	time.Sleep(50 * time.Millisecond)
	if n := countEvent(p, "resume"); n != 1 {
		t.Fatalf("resumed %d times", n)
	}
}

func TestSnapshotWaitDoesNotBlockReconcile(t *testing.T) {
	a, p := newReconcileAgent(t)
	runningVM(t, a, "old")
	s1 := proto.SnapshotRef{ID: "s1", Memory: true}
	a.reconcile(context.Background(), 0, []proto.DesiredVM{withSnaps(dv("a", "a", proto.PowerRunning, 1), s1)})
	waitSnapshot(t, a, "s1")
	go a.reconcile(context.Background(), 0, []proto.DesiredVM{withSnaps(dv("a", "a", proto.PowerRunning, 1), s1), dv("b", "b", proto.PowerRunning, 1)})
	deadline := time.Now().Add(time.Second)
	for {
		p.mu.Lock()
		n := p.starts["b"]
		p.mu.Unlock()
		if n == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("B not started while a snapshot waits")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestSnapshotResumesAfterNodeTimeout(t *testing.T) {
	old := snapshotWait
	snapshotWait = 50 * time.Millisecond
	t.Cleanup(func() { snapshotWait = old })
	a, p := newReconcileAgent(t)
	runningVM(t, a, "old")
	a.reconcile(context.Background(), 0, []proto.DesiredVM{withSnaps(dv("a", "a", proto.PowerRunning, 1), proto.SnapshotRef{ID: "s1"})})
	eventually(t, "resume", func() bool { return countEvent(p, "resume") == 1 })
	if !p.hasEvent("pause") || !p.hasEvent("flush") {
		t.Fatalf("events = %v", p.gotEvents())
	}
}

func TestSnapshotStoppedVMHasNoMemory(t *testing.T) {
	a, _ := newReconcileAgent(t)
	a.reconcile(context.Background(), 0, []proto.DesiredVM{dv("a", "a", proto.PowerStopped, 1)})
	waitState(t, a, "a", proto.StateStopped, 1)
	dir := VMDir(a.VMsDir, "a")
	writeFile(t, filepath.Join(dir, "root.img"), "old")
	a.reconcile(context.Background(), 0, []proto.DesiredVM{withSnaps(dv("a", "a", proto.PowerStopped, 1), proto.SnapshotRef{ID: "s1", Memory: true})})
	if info := waitSnapshot(t, a, "s1"); info.Memory || info.Error != "" {
		t.Fatalf("info = %+v", info)
	}
	if _, err := os.Stat(filepath.Join(dir, SnapshotsDir, "s1", StateFile)); !os.IsNotExist(err) {
		t.Fatalf("vmstate: %v", err)
	}
}

func TestSnapshotHibernatedLinksState(t *testing.T) {
	a, _ := newReconcileAgent(t)
	hibernatedVM(t, a)
	dir := VMDir(a.VMsDir, "a")
	writeFile(t, filepath.Join(dir, "root.img"), "old")
	a.reconcile(context.Background(), 0, []proto.DesiredVM{withSnaps(dv("a", "a", proto.PowerHibernated, 2), proto.SnapshotRef{ID: "s1", Memory: true})})
	if info := waitSnapshot(t, a, "s1"); !info.Memory {
		t.Fatalf("info = %+v", info)
	}
	snap, err := os.Stat(filepath.Join(dir, SnapshotsDir, "s1", MemFile))
	if err != nil {
		t.Fatal(err)
	}
	state, err := os.Stat(filepath.Join(dir, StateDir, MemFile))
	if err != nil {
		t.Fatal(err)
	}
	if !os.SameFile(snap, state) {
		t.Fatal("snapshot mem is not a link of state/mem")
	}
}

func TestSnapshotGC(t *testing.T) {
	a, _ := newReconcileAgent(t)
	dir := runningVM(t, a, "old")
	snaps := filepath.Join(dir, SnapshotsDir)
	for _, n := range []string{"s1", "s2", "s3.tmp"} {
		writeFile(t, filepath.Join(snaps, n, "root.img"), "x")
	}
	a.gcSnapshots("a", []proto.SnapshotRef{{ID: "s1"}})
	entries, err := os.ReadDir(snaps)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != "s1" {
		t.Fatalf("left: %v", entries)
	}
}

// snapshotOf writes snapshot s1 of VM "a" holding root, with memory files when memory is set.
func snapshotOf(t *testing.T, a *Agent, vm, root string, memory bool) string {
	t.Helper()
	snap := filepath.Join(VMDir(a.VMsDir, vm), SnapshotsDir, "s1")
	writeFile(t, filepath.Join(snap, "root.img"), root)
	if memory {
		writeFile(t, filepath.Join(snap, StateFile), "vmstate")
		writeFile(t, filepath.Join(snap, MemFile), "mem")
	}
	return snap
}

func withRestore(d proto.DesiredVM, seq uint64, memory, ready bool) proto.DesiredVM {
	d.Restore = &proto.Restore{SnapshotID: "s1", Seq: seq, Memory: memory, Ready: ready}
	return withSnaps(d, proto.SnapshotRef{ID: "s1", Memory: memory, Complete: true})
}

func TestRestoreLocalReplacesRoot(t *testing.T) {
	for _, memory := range []bool{false, true} {
		t.Run(map[bool]string{false: "disk", true: "memory"}[memory], func(t *testing.T) {
			a, p := newReconcileAgent(t)
			dir := runningVM(t, a, "new")
			snapshotOf(t, a, "a", "old", memory)
			a.reconcile(context.Background(), 0, []proto.DesiredVM{withRestore(dv("a", "a", proto.PowerRunning, 2), 1, memory, false)})
			eventually(t, "restored seq", func() bool { st, _ := a.vmStatus("a"); return st.RestoredSeq == 1 })
			if got := readFile(t, filepath.Join(dir, "root.img")); got != "old" {
				t.Fatalf("root.img = %q", got)
			}
			if memory && !loadVM(t, a, "a").Hibernated {
				t.Fatal("vm.json not hibernated after a memory restore")
			}
			time.Sleep(50 * time.Millisecond)
			p.mu.Lock()
			starts, restores := p.starts["a"], p.restores["a"]
			p.mu.Unlock()
			if starts != 1 || restores != 0 {
				t.Fatalf("booted before ready: starts %d, restores %d", starts, restores)
			}
			a.reconcile(context.Background(), 0, []proto.DesiredVM{withRestore(dv("a", "a", proto.PowerRunning, 2), 1, memory, true)})
			waitState(t, a, "a", proto.StateRunning, 2)
			p.mu.Lock()
			starts, restores = p.starts["a"], p.restores["a"]
			p.mu.Unlock()
			if memory && (starts != 1 || restores != 1) || !memory && (starts != 2 || restores != 0) {
				t.Fatalf("starts %d, restores %d", starts, restores)
			}
			if memory && !p.hasEvent("restore:"+filepath.Join(dir, StateDir)) {
				t.Fatalf("events = %v", p.gotEvents())
			}
		})
	}
}

func TestRestoreDiscardsCurrentState(t *testing.T) {
	a, _ := newReconcileAgent(t)
	hibernatedVM(t, a)
	dir := VMDir(a.VMsDir, "a")
	snapshotOf(t, a, "a", "old", false)
	a.reconcile(context.Background(), 0, []proto.DesiredVM{withRestore(dv("a", "a", proto.PowerHibernated, 3), 1, false, false)})
	eventually(t, "restored seq", func() bool { st, _ := a.vmStatus("a"); return st.RestoredSeq == 1 })
	if _, err := os.Stat(filepath.Join(dir, StateDir)); !os.IsNotExist(err) {
		t.Fatalf("state/ left: %v", err)
	}
	if loadVM(t, a, "a").Hibernated {
		t.Fatal("vm.json still hibernated")
	}
}

func TestRestoreSeqAppliedOnce(t *testing.T) {
	a, _ := newReconcileAgent(t)
	dir := runningVM(t, a, "new")
	snapshotOf(t, a, "a", "old", false)
	d := withRestore(dv("a", "a", proto.PowerRunning, 2), 1, false, false)
	a.reconcile(context.Background(), 0, []proto.DesiredVM{d})
	eventually(t, "restored seq", func() bool { st, _ := a.vmStatus("a"); return st.RestoredSeq == 1 })
	writeFile(t, filepath.Join(dir, "root.img"), "new")
	a.reconcile(context.Background(), 0, []proto.DesiredVM{d})
	time.Sleep(100 * time.Millisecond)
	if got := readFile(t, filepath.Join(dir, "root.img")); got != "new" {
		t.Fatalf("root.img replaced again: %q", got)
	}
}

func TestForkLocalMemory(t *testing.T) {
	a, p := newReconcileAgent(t)
	snap := snapshotOf(t, a, "src", "root", true)
	srcMem, err := os.Stat(filepath.Join(snap, MemFile))
	if err != nil {
		t.Fatal(err)
	}
	linked := make(chan bool, 1)
	p.beforeRestore = func(dir string) {
		st, err := os.Stat(filepath.Join(dir, StateDir, MemFile))
		linked <- err != nil || os.SameFile(st, srcMem)
	}
	d := dv("f", "f", proto.PowerRunning, 1)
	d.Fork = &proto.Fork{SourceVMID: "src", SnapshotID: "s1", Memory: true}
	a.reconcile(context.Background(), 0, []proto.DesiredVM{d})
	waitState(t, a, "f", proto.StateRunning, 1)
	if got := readFile(t, filepath.Join(VMDir(a.VMsDir, "f"), "root.img")); got != "root" {
		t.Fatalf("root.img = %q", got)
	}
	if <-linked {
		t.Fatal("fork's mem missing or a link of the snapshot's")
	}
	if !p.hasEvent("restore-takeoff") {
		t.Fatalf("events = %v", p.gotEvents())
	}
}

func TestForkMissingSnapshot(t *testing.T) {
	a, _ := newReconcileAgent(t)
	d := dv("f", "f", proto.PowerRunning, 1)
	d.Fork = &proto.Fork{SourceVMID: "src", SnapshotID: "s1"}
	a.reconcile(context.Background(), 0, []proto.DesiredVM{d})
	eventually(t, "error", func() bool {
		st, _ := a.vmStatus("f")
		return st.State == proto.StateError && st.Error == "snapshot files missing"
	})
}

func TestRestoreFailureAfterKillKeepsFileInSync(t *testing.T) {
	a, _ := newReconcileAgent(t)
	hibernatedVM(t, a)
	dir := VMDir(a.VMsDir, "a")
	writeFile(t, filepath.Join(dir, "root.img"), "new")
	snap := snapshotOf(t, a, "a", "old", false)
	if err := os.Chmod(filepath.Join(snap, "root.img"), 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(filepath.Join(snap, "root.img"), 0o600) })
	a.reconcile(context.Background(), 0, []proto.DesiredVM{withRestore(dv("a", "a", proto.PowerHibernated, 3), 1, false, false)})
	eventually(t, "restore error", func() bool { st, _ := a.vmStatus("a"); return strings.HasPrefix(st.Error, "restore: ") })
	a.mu.Lock()
	mem := a.vms["a"].file
	a.mu.Unlock()
	if disk := loadVM(t, a, "a"); mem.Hibernated || disk.Hibernated || mem.Power != disk.Power {
		t.Fatalf("memory %+v, vm.json %+v", mem, disk)
	}
	s1 := proto.SnapshotRef{ID: "s1", Complete: true}
	a.reconcile(context.Background(), 0, []proto.DesiredVM{withSnaps(dv("a", "a", proto.PowerHibernated, 3), s1, proto.SnapshotRef{ID: "s2", Memory: true})})
	if info := waitSnapshot(t, a, "s2"); info.Error != "" || info.Memory {
		t.Fatalf("info = %+v", info)
	}
}

package node

import (
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"time"

	"github.com/asterism-dev/observatory/internal/proto"
)

const SnapshotsDir = "snapshots"

// snapshotWait bounds how long a VM stays paused for a snapshot the hub never completes.
var snapshotWait = 180 * time.Second

var errSnapshotMissing = errors.New("snapshot files missing")

const errResumedEarly = "VM resumed before the snapshot completed"

type snapKey struct{ vm, snap string }

// snapshotJob is a snapshot this node process took; release ends its wait for the hub.
type snapshotJob struct {
	release  chan struct{}
	released bool
	err      string
}

// releaseLocked lets the snapshot's VM go on; a.mu must be held.
func (j *snapshotJob) releaseLocked() {
	if !j.released {
		j.released = true
		close(j.release)
	}
}

// syncSnapshots starts the snapshots d adds, releases those the hub completed and removes those it dropped.
func (a *Agent) syncSnapshots(d proto.DesiredVM) {
	id := d.Spec.ID
	snaps := filepath.Join(VMDir(a.VMsDir, id), SnapshotsDir)
	var start []proto.SnapshotRef
	a.mu.Lock()
	for _, ref := range d.Snapshots {
		k := snapKey{id, ref.ID}
		j := a.snapJobs[k]
		switch {
		case j != nil && ref.Complete:
			j.releaseLocked()
		case j == nil && !ref.Complete:
			start = append(start, ref)
		}
	}
	a.mu.Unlock()
	// A snapshot on disk without a job was taken before the node restarted, which resumed its VM: the hub must not copy the disks now.
	var orphans []string
	start = slices.DeleteFunc(start, func(ref proto.SnapshotRef) bool {
		if !exists(filepath.Join(snaps, ref.ID)) {
			return false
		}
		if err := os.RemoveAll(filepath.Join(snaps, ref.ID)); err != nil {
			slog.Warn("remove snapshot failed", "vm", id, "snapshot", ref.ID, "err", err)
		}
		orphans = append(orphans, ref.ID)
		return true
	})
	a.mu.Lock()
	if a.snapJobs == nil {
		a.snapJobs = map[snapKey]*snapshotJob{}
	}
	for _, ref := range start {
		a.snapJobs[snapKey{id, ref.ID}] = &snapshotJob{release: make(chan struct{})}
	}
	for _, snap := range orphans {
		a.snapJobs[snapKey{id, snap}] = &snapshotJob{release: make(chan struct{}), released: true, err: errResumedEarly}
	}
	a.mu.Unlock()
	a.gcSnapshots(id, d.Snapshots)
	if len(orphans) > 0 {
		a.reportCurrent(id)
	}
	for _, ref := range start {
		go a.takeSnapshot(id, ref)
	}
}

func exists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// gcSnapshots removes the snapshot dirs (complete or not) of VM id that refs no longer lists.
func (a *Agent) gcSnapshots(id string, refs []proto.SnapshotRef) {
	keep := map[string]bool{}
	for _, r := range refs {
		keep[r.ID] = true
	}
	a.mu.Lock()
	for k, j := range a.snapJobs {
		if k.vm == id && !keep[k.snap] {
			j.releaseLocked()
			delete(a.snapJobs, k)
		}
	}
	a.mu.Unlock()
	snaps := filepath.Join(VMDir(a.VMsDir, id), SnapshotsDir)
	entries, _ := os.ReadDir(snaps)
	removed := false
	for _, en := range entries {
		if keep[strings.TrimSuffix(en.Name(), ".tmp")] {
			continue
		}
		if err := os.RemoveAll(filepath.Join(snaps, en.Name())); err != nil {
			slog.Warn("remove snapshot failed", "vm", id, "snapshot", en.Name(), "err", err)
		}
		removed = true
	}
	if removed {
		a.reportCurrent(id)
	}
}

// takeSnapshot writes snapshot ref of VM id and keeps a live VM paused until the hub completes it, drops it or snapshotWait passes.
func (a *Agent) takeSnapshot(id string, ref proto.SnapshotRef) {
	k := snapKey{id, ref.ID}
	e := a.lockCurrent(id)
	if e == nil {
		a.mu.Lock()
		delete(a.snapJobs, k)
		a.mu.Unlock()
		return
	}
	defer e.op.Unlock()
	a.mu.Lock()
	job := a.snapJobs[k]
	f, inst, wasPaused := e.file, e.inst, e.paused
	a.mu.Unlock()
	if job == nil {
		return
	}
	dir := VMDir(a.VMsDir, id)
	snaps := filepath.Join(dir, SnapshotsDir)
	tmp := filepath.Join(snaps, ref.ID+".tmp")
	live := alive(inst)
	started := time.Now()
	err := os.RemoveAll(tmp)
	if err == nil {
		err = os.MkdirAll(snaps, 0o700)
	}
	if err == nil {
		switch {
		case live && ref.Memory:
			if err = CheckSpace(a.VMsDir, int64(f.Spec.MemoryMB)<<20+1<<30); err == nil {
				err = inst.SaveState(tmp)
			}
		case live:
			if err = os.Mkdir(tmp, 0o700); err == nil {
				if err = inst.Pause(); err == nil {
					err = inst.Flush()
				}
			}
		// An unstarted fork's state is its source's memory, which must never resume under the source's identity.
		case f.Hibernated && ref.Memory && !f.Takeoff:
			err = linkState(filepath.Join(dir, StateDir), tmp)
		default:
			err = os.Mkdir(tmp, 0o700)
		}
	}
	root := filepath.Join(tmp, "root.img")
	if err == nil {
		err = cloneFile(filepath.Join(dir, "root.img"), root)
	}
	if err == nil {
		err = os.Chmod(root, 0o600)
	}
	for _, p := range []string{root, tmp} {
		if err == nil {
			err = syncDir(p)
		}
	}
	if err == nil {
		err = os.Rename(tmp, filepath.Join(snaps, ref.ID))
	}
	if err == nil {
		err = syncDir(snaps)
	}
	if err != nil {
		os.RemoveAll(tmp)
		slog.Error("snapshot failed", "vm", id, "snapshot", ref.ID, "err", err)
		a.mu.Lock()
		job.err = err.Error()
		a.mu.Unlock()
	} else {
		slog.Info("snapshot written", "vm", id, "snapshot", ref.ID, "took", time.Since(started).Round(time.Millisecond))
	}
	a.reportCurrent(id)
	if err == nil {
		select {
		case <-job.release:
		case <-time.After(snapshotWait):
			slog.Warn("snapshot not completed in time, going on", "vm", id, "snapshot", ref.ID, "wait", snapshotWait)
			// Disk copies the hub queues from now on would postdate the memory.
			if err := os.RemoveAll(filepath.Join(snaps, ref.ID)); err != nil {
				slog.Warn("remove snapshot failed", "vm", id, "snapshot", ref.ID, "err", err)
			}
			a.mu.Lock()
			job.err = errResumedEarly
			a.mu.Unlock()
			a.reportCurrent(id)
		}
	}
	if live && !wasPaused && alive(inst) {
		if err := inst.Resume(); err != nil {
			slog.Warn("resume after snapshot failed", "vm", id, "err", err)
		} else {
			slog.Info("vm paused for snapshot", "vm", id, "snapshot", ref.ID, "took", time.Since(started).Round(time.Millisecond))
			go a.guestResume(id, inst)
		}
	}
}

func linkState(state, dst string) error {
	if err := os.Mkdir(dst, 0o700); err != nil {
		return err
	}
	for _, n := range []string{StateFile, MemFile} {
		if err := os.Link(filepath.Join(state, n), filepath.Join(dst, n)); err != nil {
			return err
		}
	}
	return nil
}

// lockCurrent returns VM id's current entry with its op held, or nil when the VM is gone.
func (a *Agent) lockCurrent(id string) *vmEntry {
	for {
		a.mu.Lock()
		e, ok := a.vms[id]
		a.mu.Unlock()
		if !ok || e.placeholder {
			return nil
		}
		<-e.booted
		e.op.Lock()
		a.mu.Lock()
		same := a.vms[id] == e
		a.mu.Unlock()
		if same {
			return e
		}
		// Replaced (halted or restored) while waiting.
		e.op.Unlock()
	}
}

// reportCurrent re-sends VM id's status, which picks up its snapshots.
func (a *Agent) reportCurrent(id string) {
	a.mu.Lock()
	e, ok := a.vms[id]
	var st proto.VMStatus
	if ok {
		st = e.status
	}
	a.mu.Unlock()
	if ok {
		a.reportEntry(e, st)
	}
}

// snapshotInfos reports the snapshots this process took: an earlier process's dir is either already reported or failed.
func (a *Agent) snapshotInfos(id string) []proto.SnapshotInfo {
	var out []proto.SnapshotInfo
	taken := map[string]bool{}
	a.mu.Lock()
	for k, j := range a.snapJobs {
		switch {
		case k.vm != id:
		case j.err != "":
			out = append(out, proto.SnapshotInfo{ID: k.snap, Error: j.err})
		default:
			taken[k.snap] = true
		}
	}
	a.mu.Unlock()
	snaps := filepath.Join(VMDir(a.VMsDir, id), SnapshotsDir)
	entries, _ := os.ReadDir(snaps)
	for _, en := range entries {
		if !en.IsDir() || !taken[en.Name()] {
			continue
		}
		info := proto.SnapshotInfo{ID: en.Name()}
		files, _ := os.ReadDir(filepath.Join(snaps, en.Name()))
		for _, fl := range files {
			info.Memory = info.Memory || fl.Name() == StateFile
			if fi, err := fl.Info(); err == nil {
				// Reflinked and linked files share blocks with the VM; the allocation is still the honest upper bound.
				if sys, ok := fi.Sys().(*syscall.Stat_t); ok {
					info.SizeBytes += int64(sys.Blocks) * 512
				}
			}
		}
		out = append(out, info)
	}
	slices.SortFunc(out, func(x, y proto.SnapshotInfo) int { return strings.Compare(x.ID, y.ID) })
	return out
}

// releaseSnapshots ends every snapshot wait, so a stopping node can hibernate the paused VMs.
func (a *Agent) releaseSnapshots() {
	a.mu.Lock()
	defer a.mu.Unlock()
	for _, j := range a.snapJobs {
		j.releaseLocked()
	}
}

// restoreDue reports whether d asks f for a restore f has not applied yet.
func restoreDue(f VMFile, d proto.DesiredVM) bool {
	return d.Restore != nil && d.Restore.Seq > f.RestoredSeq
}

// restoreLocal powers e off hard and rolls it back to snapshot r; it stays off until the hub marks the restore ready. The caller holds e.op.
func (a *Agent) restoreLocal(e *vmEntry, r proto.Restore) error {
	a.mu.Lock()
	f := e.file
	a.mu.Unlock()
	id, dir := f.Spec.ID, VMDir(a.VMsDir, f.Spec.ID)
	snap := filepath.Join(dir, SnapshotsDir, r.SnapshotID)
	need := []string{"root.img"}
	if r.Memory {
		need = append(need, StateFile, MemFile)
	}
	for _, n := range need {
		if !exists(filepath.Join(snap, n)) {
			return errSnapshotMissing
		}
	}
	e.stopRequested.Store(true)
	if inst := a.instance(e); alive(inst) {
		if err := inst.Kill(); err != nil {
			return err
		}
		<-inst.Done()
	}
	e.cancel()
	// Off and without memory first, so a crash midway leaves a VM that neither boots nor resumes stale memory.
	f.Hibernated, f.Takeoff, f.Suspended, f.Power = false, false, false, proto.PowerStopped
	if err := SaveVM(a.VMsDir, f); err != nil {
		return err
	}
	// A failure below must leave memory agreeing with vm.json: off and without state.
	a.mu.Lock()
	e.file = f
	a.mu.Unlock()
	state := filepath.Join(dir, StateDir)
	if err := os.RemoveAll(state); err != nil {
		return err
	}
	root, tmp := filepath.Join(dir, "root.img"), filepath.Join(dir, "root.img.tmp")
	os.Remove(tmp)
	err := cloneFile(filepath.Join(snap, "root.img"), tmp)
	if err == nil {
		err = syncDir(tmp)
	}
	if err == nil {
		err = os.Rename(tmp, root)
	}
	if err == nil && r.Memory {
		// The snapshot's files are only read: a load maps mem privately and a later hibernate writes new files.
		if err = linkState(snap, state); err == nil {
			err = syncDir(state)
		}
	}
	if err == nil {
		err = syncDir(dir)
	}
	if err != nil {
		os.Remove(tmp)
		os.RemoveAll(state)
		return err
	}
	f.Hibernated, f.RestoredSeq = r.Memory, r.Seq
	if r.Memory {
		f.Power = proto.PowerHibernated
	}
	if err := SaveVM(a.VMsDir, f); err != nil {
		return err
	}
	booted := make(chan struct{})
	close(booted)
	idle := &vmEntry{file: f, status: proto.VMStatus{ID: id, State: restingState(f)}, cancel: func() {}, booted: booted}
	a.mu.Lock()
	a.vms[id] = idle
	a.mu.Unlock()
	a.report(idle.status)
	return nil
}

// forkLocal builds the dir of new VM d from a snapshot of its source; with memory it starts by restoring that memory under its own identity.
func (a *Agent) forkLocal(d proto.DesiredVM) (VMFile, error) {
	fk := d.Fork
	snap := filepath.Join(VMDir(a.VMsDir, fk.SourceVMID), SnapshotsDir, fk.SnapshotID)
	files := []string{"root.img"}
	if fk.Memory {
		files = append(files, StateFile, MemFile)
	}
	for _, n := range files {
		if !exists(filepath.Join(snap, n)) {
			return VMFile{}, errSnapshotMissing
		}
	}
	f := VMFile{Spec: d.Spec, MAC: RandomMAC(), Power: d.Power, StopMode: d.StopMode, Generation: d.Generation, Boot: d.Boot,
		AutoSuspendMinutes: d.AutoSuspendMinutes, WakeSeq: d.WakeSeq}
	a.mu.Lock()
	if src, ok := a.vms[fk.SourceVMID]; ok {
		f.Image, f.ImageConfig = src.file.Image, src.file.ImageConfig
	}
	a.mu.Unlock()
	dir := VMDir(a.VMsDir, f.Spec.ID)
	state := filepath.Join(dir, StateDir)
	err := os.MkdirAll(dir, 0o700)
	if err == nil {
		err = cloneFile(filepath.Join(snap, "root.img"), filepath.Join(dir, "root.img"))
	}
	if err == nil && fk.Memory {
		err = os.Mkdir(state, 0o700)
		// ponytail: a copy (the fork's jail chowns it to its own UID) made inside reconcile; move to boot if paths.vms lacks reflinks.
		for _, n := range files[1:] {
			if err == nil {
				err = cloneFile(filepath.Join(snap, n), filepath.Join(state, n))
			}
			if err == nil {
				err = os.Chmod(filepath.Join(state, n), 0o600)
			}
			if err == nil {
				err = syncDir(filepath.Join(state, n))
			}
		}
		if err == nil {
			err = syncDir(state)
		}
		f.Hibernated, f.Takeoff = true, true
	}
	if err == nil {
		err = SaveVM(a.VMsDir, f)
	}
	if err != nil {
		DeleteVMDir(a.VMsDir, f.Spec.ID)
		return VMFile{}, err
	}
	return f, nil
}

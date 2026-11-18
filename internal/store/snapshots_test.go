package store_test

import (
	"context"
	"errors"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/asterism-dev/observatory/internal/proto"
	"github.com/asterism-dev/observatory/internal/store"
)

func mustSnapshot(t *testing.T, e vmEnv, vm uuid.UUID, name string, memory bool) store.Snapshot {
	t.Helper()
	s, err := e.s.CreateSnapshot(context.Background(), e.org.ID, vm, name, memory, e.user)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func reportSnapshot(t *testing.T, e vmEnv, node, vm, snap uuid.UUID) bool {
	t.Helper()
	changed, err := e.s.ApplySnapshotStatus(context.Background(), node, proto.VMStatus{ID: vm.String(), Snapshots: []proto.SnapshotInfo{{ID: snap.String(), SizeBytes: 42}}})
	if err != nil {
		t.Fatal(err)
	}
	return changed
}

// storeAllFiles reports every file the storage server is asked to hold at full size.
func storeAllFiles(t *testing.T, e vmEnv) {
	t.Helper()
	ctx := context.Background()
	st, err := e.s.StorageState(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var files []proto.StorageFile
	for _, d := range st.Disks {
		files = append(files, proto.StorageFile{Name: proto.StorageFileName(d.ID), SizeBytes: d.SizeBytes})
	}
	if _, err := e.s.ApplyStorageFiles(ctx, files); err != nil {
		t.Fatal(err)
	}
}

// vmWithDisk is a stopped VM with a ready 1 GB disk "data" at /data.
func vmWithDisk(t *testing.T, e vmEnv, name string) (store.VM, store.Disk) {
	t.Helper()
	ctx := context.Background()
	vm := mustStoppedVM(t, e, name, nil)
	d := mustCreateDisk(t, e, e.disk(name+"-data", nil, 1))
	if _, _, err := e.s.SetVMDisks(ctx, e.org.ID, vm.ID, []store.VMDisk{{DiskID: d.ID, MountPath: "/data"}}, false); err != nil {
		t.Fatal(err)
	}
	execSQL(t, e.pool, "UPDATE disks SET ready_gb = size_gb WHERE id = $1", d.ID)
	return vm, d
}

func readySnapshot(t *testing.T, e vmEnv, vm uuid.UUID, name string) store.Snapshot {
	t.Helper()
	s := mustSnapshot(t, e, vm, name, false)
	reportSnapshot(t, e, e.node.ID, vm, s.ID)
	storeAllFiles(t, e)
	got, err := e.s.Snapshot(context.Background(), e.org.ID, vm, s.ID)
	if err != nil || got.State != "ready" {
		t.Fatalf("snapshot not ready: %+v %v", got, err)
	}
	return got
}

func restoreRow(t *testing.T, e vmEnv, vm uuid.UUID) (seq int64, state string) {
	t.Helper()
	if err := e.pool.QueryRow(context.Background(), "SELECT restore_seq, restore_state FROM vms WHERE id = $1", vm).Scan(&seq, &state); err != nil {
		t.Fatal(err)
	}
	return
}

func storageDisk(t *testing.T, e vmEnv, id uuid.UUID) *proto.StorageDisk {
	t.Helper()
	st, err := e.s.StorageState(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range st.Disks {
		if d.ID == id.String() {
			return &d
		}
	}
	return nil
}

func TestCreateSnapshotLimits(t *testing.T) {
	ctx := context.Background()
	e := newDiskEnv(t)
	v1 := mustStoppedVM(t, e, "v1", nil)
	for i := range store.MaxSnapshotsPerVM {
		mustSnapshot(t, e, v1.ID, "s"+string(rune('a'+i)), false)
	}
	if _, err := e.s.CreateSnapshot(ctx, e.org.ID, v1.ID, "eleven", false, e.user); !errors.Is(err, store.ErrLimitExceeded) {
		t.Fatalf("11th: %v", err)
	}
	if _, err := e.s.CreateSnapshot(ctx, e.org.ID, v1.ID, "sa", false, e.user); !errors.Is(err, store.ErrLimitExceeded) {
		t.Fatalf("cap before duplicate: %v", err)
	}

	e2 := newDiskEnv(t)
	a, b := mustStoppedVM(t, e2, "a", nil), mustStoppedVM(t, e2, "b", nil)
	if err := e2.s.SetLimits(ctx, e2.org.ID, "user:"+e2.user.String(), store.Limits{SnapshotCount: ip(2)}); err != nil {
		t.Fatal(err)
	}
	first := mustSnapshot(t, e2, a.ID, "one", false)
	if _, err := e2.s.CreateSnapshot(ctx, e2.org.ID, a.ID, "one", false, e2.user); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("duplicate: %v", err)
	}
	mustSnapshot(t, e2, b.ID, "two", false)
	if _, err := e2.s.CreateSnapshot(ctx, e2.org.ID, b.ID, "three", false, e2.user); !errors.Is(err, store.ErrLimitExceeded) {
		t.Fatalf("user limit: %v", err)
	}
	if err := e2.s.DeleteSnapshot(ctx, e2.org.ID, a.ID, first.ID); err != nil {
		t.Fatal(err)
	}
	mustSnapshot(t, e2, b.ID, "three", false)
}

func TestSnapshotMemoryOffWhenStopped(t *testing.T) {
	e := newDiskEnv(t)
	vm := mustStoppedVM(t, e, "v", nil)
	if s := mustSnapshot(t, e, vm.ID, "s", true); s.Memory || s.State != "creating" || s.CPUs != 1 || s.DiskGB != 1 {
		t.Fatalf("snapshot: %+v", s)
	}
	running, err := e.s.CreateVM(context.Background(), e.vm("r", nil, 1, 1, 1))
	if err != nil {
		t.Fatal(err)
	}
	if s := mustSnapshot(t, e, running.ID, "s", true); !s.Memory {
		t.Fatalf("running VM: %+v", s)
	}
}

func TestApplySnapshotStatusAddsStorageCopies(t *testing.T) {
	ctx := context.Background()
	e := newDiskEnv(t)
	vm, d := vmWithDisk(t, e, "v")
	s := mustSnapshot(t, e, vm.ID, "s", false)
	if len(s.Attachments) != 1 || s.Attachments[0] != (store.SnapshotAttachment{DiskID: d.ID, MountPath: "/data"}) {
		t.Fatalf("attachments: %+v", s.Attachments)
	}
	if !reportSnapshot(t, e, e.node.ID, vm.ID, s.ID) {
		t.Fatal("first report: no change")
	}
	if reportSnapshot(t, e, e.node.ID, vm.ID, s.ID) {
		t.Fatal("second report changed something")
	}
	got, err := e.s.Snapshot(ctx, e.org.ID, vm.ID, s.ID)
	if err != nil || got.State != "creating" || got.ReportedAt == nil || got.SizeBytes != 42 || len(got.Disks) != 1 {
		t.Fatalf("snapshot: %+v %v", got, err)
	}
	copyDisk := got.Disks[0]
	if copyDisk.SourceDiskID != d.ID || copyDisk.DiskName != "v-data" || copyDisk.MountPath != "/data" || copyDisk.SizeGB != 1 || copyDisk.ReadyGB != 0 {
		t.Fatalf("copy: %+v", copyDisk)
	}
	if sd := storageDisk(t, e, copyDisk.ID); sd == nil || sd.CopyFrom != d.ID.String() || !sd.Create || sd.SizeBytes != 1<<30 {
		t.Fatalf("storage copy: %+v", sd)
	}
	storeAllFiles(t, e)
	if got, _ = e.s.Snapshot(ctx, e.org.ID, vm.ID, s.ID); got.State != "ready" || got.Disks[0].ReadyGB != 1 {
		t.Fatalf("after files: %+v", got)
	}
	if sd := storageDisk(t, e, copyDisk.ID); sd == nil || sd.CopyFrom != "" || sd.Create {
		t.Fatalf("storage copy after files: %+v", sd)
	}
}

func TestApplySnapshotStatusNoDisksReady(t *testing.T) {
	e := newDiskEnv(t)
	vm := mustStoppedVM(t, e, "v", nil)
	s := mustSnapshot(t, e, vm.ID, "s", false)
	reportSnapshot(t, e, e.node.ID, vm.ID, s.ID)
	if got, err := e.s.Snapshot(context.Background(), e.org.ID, vm.ID, s.ID); err != nil || got.State != "ready" || len(got.Disks) != 0 {
		t.Fatalf("snapshot: %+v %v", got, err)
	}
}

func TestApplySnapshotStatusNodeError(t *testing.T) {
	ctx := context.Background()
	e := newDiskEnv(t)
	vm, _ := vmWithDisk(t, e, "v")
	s := mustSnapshot(t, e, vm.ID, "s", false)
	if _, err := e.s.ApplySnapshotStatus(ctx, e.node.ID, proto.VMStatus{ID: vm.ID.String(), Snapshots: []proto.SnapshotInfo{{ID: s.ID.String(), Error: "not enough space on node"}}}); err != nil {
		t.Fatal(err)
	}
	if got, _ := e.s.Snapshot(ctx, e.org.ID, vm.ID, s.ID); got.State != "error" || got.Error != "not enough space on node" || len(got.Disks) != 0 {
		t.Fatalf("snapshot: %+v", got)
	}
}

func TestApplySnapshotStatusOtherNodeIgnored(t *testing.T) {
	ctx := context.Background()
	e := newDiskEnv(t)
	other, tok, err := e.s.CreateNode(ctx, store.Node{OrgID: e.org.ID, Name: "n2", CreatedBy: e.user})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := e.s.EnrollNode(ctx, tok); err != nil {
		t.Fatal(err)
	}
	vm, _ := vmWithDisk(t, e, "v")
	s := mustSnapshot(t, e, vm.ID, "s", false)
	if reportSnapshot(t, e, other.ID, vm.ID, s.ID) {
		t.Fatal("foreign node changed something")
	}
	if got, _ := e.s.Snapshot(ctx, e.org.ID, vm.ID, s.ID); got.ReportedAt != nil || len(got.Disks) != 0 {
		t.Fatalf("snapshot: %+v", got)
	}
}

func TestFailStaleSnapshots(t *testing.T) {
	ctx := context.Background()
	e := newDiskEnv(t)
	vm, _ := vmWithDisk(t, e, "v")
	s := mustSnapshot(t, e, vm.ID, "s", false)
	fresh := mustSnapshot(t, e, vm.ID, "fresh", false)
	reportSnapshot(t, e, e.node.ID, vm.ID, s.ID)
	reportSnapshot(t, e, e.node.ID, vm.ID, fresh.ID)
	execSQL(t, e.pool, "UPDATE vm_snapshots SET reported_at = now() - interval '3 minutes' WHERE id = $1", s.ID)
	got, _ := e.s.Snapshot(ctx, e.org.ID, vm.ID, s.ID)
	copyID := got.Disks[0].ID

	orgs, err := e.s.FailStaleSnapshots(ctx, 2*time.Minute)
	if err != nil || len(orgs) != 1 || orgs[0] != e.org.ID {
		t.Fatalf("orgs: %v %v", orgs, err)
	}
	if got, _ = e.s.Snapshot(ctx, e.org.ID, vm.ID, s.ID); got.State != "error" || got.Error != "storage copy timed out" || len(got.Disks) != 0 {
		t.Fatalf("stale: %+v", got)
	}
	if got, _ = e.s.Snapshot(ctx, e.org.ID, vm.ID, fresh.ID); got.State != "creating" {
		t.Fatalf("fresh: %+v", got)
	}
	st, err := e.s.StorageState(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if storageDisk(t, e, copyID) != nil || len(st.Deleted) != 1 || st.Deleted[0] != copyID.String() {
		t.Fatalf("deleted: %v", st.Deleted)
	}
	storeAllFiles(t, e)
	var n int
	if err := e.pool.QueryRow(ctx, "SELECT count(*) FROM snapshot_disks WHERE id = $1", copyID).Scan(&n); err != nil || n != 0 {
		t.Fatalf("copy not purged: %d %v", n, err)
	}
}

func TestRestoreSequence(t *testing.T) {
	ctx := context.Background()
	e := newDiskEnv(t)
	vm, d := vmWithDisk(t, e, "v")
	snap := readySnapshot(t, e, vm.ID, "s")
	got, err := e.s.RequestRestore(ctx, e.org.ID, vm.ID, snap.ID, false, false)
	if err != nil || got.Generation <= vm.Generation {
		t.Fatalf("restore: %+v %v", got, err)
	}
	if v, err := e.s.VM(ctx, e.org.ID, vm.ID); err != nil || v.RestoreState != "node" {
		t.Fatalf("VM restore state: %+v %v", v, err)
	}
	if seq, state := restoreRow(t, e, vm.ID); seq != 1 || state != "node" {
		t.Fatalf("after request: %d %q", seq, state)
	}
	if sd := storageDisk(t, e, d.ID); sd.RestoreFrom != "" || sd.RestoreToken != "" {
		t.Fatalf("restore before the node: %+v", sd)
	}
	status := func(seq uint64) bool {
		changed, err := e.s.ApplySnapshotStatus(ctx, e.node.ID, proto.VMStatus{ID: vm.ID.String(), RestoredSeq: seq})
		if err != nil {
			t.Fatal(err)
		}
		return changed
	}
	if status(0) {
		t.Fatal("older seq changed something")
	}
	if !status(1) {
		t.Fatal("restored seq: no change")
	}
	sd := storageDisk(t, e, d.ID)
	if sd.RestoreFrom != proto.StorageFileName(snap.Disks[0].ID.String()) || sd.RestoreToken == "" {
		t.Fatalf("restore: %+v", sd)
	}
	if _, state := restoreRow(t, e, vm.ID); state != "storage" {
		t.Fatalf("state %q", state)
	}
	if status(1) || storageDisk(t, e, d.ID).RestoreToken != sd.RestoreToken {
		t.Fatal("repeated status replaced the token")
	}
	if err := e.s.DeleteSnapshot(ctx, e.org.ID, vm.ID, snap.ID); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("delete during restore: %v", err)
	}
	if _, err := e.s.RequestRestore(ctx, e.org.ID, vm.ID, snap.ID, false, false); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("restore during storage restore: %v", err)
	}
	before, _ := e.s.VM(ctx, e.org.ID, vm.ID)
	if orgs, err := e.s.ApplyStorageRestored(ctx, []string{"unknown"}); err != nil || len(orgs) != 0 {
		t.Fatalf("unknown token: %v %v", orgs, err)
	}
	orgs, err := e.s.ApplyStorageRestored(ctx, []string{sd.RestoreToken})
	if err != nil || len(orgs) != 1 || orgs[0] != e.org.ID {
		t.Fatalf("restored: %v %v", orgs, err)
	}
	if _, state := restoreRow(t, e, vm.ID); state != "done" {
		t.Fatalf("state %q", state)
	}
	if after, _ := e.s.VM(ctx, e.org.ID, vm.ID); after.RestoreState != "done" || after.Generation <= before.Generation {
		t.Fatal("done without a generation bump")
	}
	if sd := storageDisk(t, e, d.ID); sd.RestoreFrom != "" || sd.RestoreToken != "" {
		t.Fatalf("restore left: %+v", sd)
	}
	if err := e.s.DeleteSnapshot(ctx, e.org.ID, vm.ID, snap.ID); err != nil {
		t.Fatalf("delete after restore: %v", err)
	}
}

func TestRestoreWithoutDisksDone(t *testing.T) {
	ctx := context.Background()
	e := newDiskEnv(t)
	vm := mustStoppedVM(t, e, "v", nil)
	snap := readySnapshot(t, e, vm.ID, "s")
	got, err := e.s.RequestRestore(ctx, e.org.ID, vm.ID, snap.ID, false, false)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.s.ApplySnapshotStatus(ctx, e.node.ID, proto.VMStatus{ID: vm.ID.String(), RestoredSeq: 1}); err != nil {
		t.Fatal(err)
	}
	if _, state := restoreRow(t, e, vm.ID); state != "done" {
		t.Fatalf("state %q", state)
	}
	if after, _ := e.s.VM(ctx, e.org.ID, vm.ID); after.Generation <= got.Generation {
		t.Fatal("done without a generation bump")
	}
}

func TestRestoreTargetDiskDeleted(t *testing.T) {
	ctx := context.Background()
	e := newDiskEnv(t)
	vm, d := vmWithDisk(t, e, "v")
	snap := readySnapshot(t, e, vm.ID, "s")
	if _, err := e.s.RequestRestore(ctx, e.org.ID, vm.ID, snap.ID, false, false); err != nil {
		t.Fatal(err)
	}
	if _, err := e.s.ApplySnapshotStatus(ctx, e.node.ID, proto.VMStatus{ID: vm.ID.String(), RestoredSeq: 1}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := e.s.SetVMDisks(ctx, e.org.ID, vm.ID, []store.VMDisk{}, true); err != nil {
		t.Fatal(err)
	}
	if err := e.s.ForceRelease(ctx, e.org.ID, d.ID); err != nil {
		t.Fatal(err)
	}
	before, _ := e.s.VM(ctx, e.org.ID, vm.ID)
	if err := e.s.DeleteDisk(ctx, e.org.ID, d.ID); err != nil {
		t.Fatal(err)
	}
	var token *string
	if err := e.pool.QueryRow(ctx, "SELECT restore_token FROM disks WHERE id = $1", d.ID).Scan(&token); err != nil || token != nil {
		t.Fatalf("token left on deleted disk: %v %v", token, err)
	}
	if _, state := restoreRow(t, e, vm.ID); state != "done" {
		t.Fatalf("state %q", state)
	}
	if after, _ := e.s.VM(ctx, e.org.ID, vm.ID); after.Generation <= before.Generation {
		t.Fatal("done without a generation bump")
	}
}

func TestRequestRestoreShapeChanged(t *testing.T) {
	ctx := context.Background()
	e := newDiskEnv(t)
	vm := mustStoppedVM(t, e, "v", nil)
	snap := readySnapshot(t, e, vm.ID, "s")
	execSQL(t, e.pool, "UPDATE vm_snapshots SET memory = true WHERE id = $1", snap.ID)
	n := mustCreateNetwork(t, e.s, e.org.ID, e.user, "net", "")
	if _, err := e.s.SetVMNetworks(ctx, e.org.ID, vm.ID, []uuid.UUID{n.ID}, false); err != nil {
		t.Fatal(err)
	}
	if _, err := e.s.RequestRestore(ctx, e.org.ID, vm.ID, snap.ID, true, false); !errors.Is(err, store.ErrShapeChanged) {
		t.Fatalf("memory: %v", err)
	}
	if _, err := e.s.RequestRestore(ctx, e.org.ID, vm.ID, snap.ID, false, false); err != nil {
		t.Fatalf("disk only: %v", err)
	}
}

func TestRequestRestoreRefusals(t *testing.T) {
	ctx := context.Background()
	e := newDiskEnv(t)
	vm := mustStoppedVM(t, e, "v", nil)
	creating := mustSnapshot(t, e, vm.ID, "c", false)
	if _, err := e.s.RequestRestore(ctx, e.org.ID, vm.ID, creating.ID, false, true); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("not ready: %v", err)
	}
	// A snapshot still being taken would block the restore.
	if _, err := e.s.ApplySnapshotStatus(ctx, e.node.ID, proto.VMStatus{ID: vm.ID.String(), Snapshots: []proto.SnapshotInfo{{ID: creating.ID.String(), Error: "failed"}}}); err != nil {
		t.Fatal(err)
	}
	ready := readySnapshot(t, e, vm.ID, "r")
	if _, err := e.s.SetVMPower(ctx, e.org.ID, vm.ID, proto.PowerRunning, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := e.s.RequestRestore(ctx, e.org.ID, vm.ID, ready.ID, false, false); !errors.Is(err, store.ErrRestartRequired) {
		t.Fatalf("running: %v", err)
	}
	if _, err := e.s.RequestRestore(ctx, e.org.ID, vm.ID, uuid.New(), false, true); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("unknown: %v", err)
	}
	if _, err := e.s.RequestRestore(ctx, e.org.ID, vm.ID, ready.ID, false, true); err != nil {
		t.Fatalf("with restart: %v", err)
	}
}

func TestDeleteSnapshotPendingFork(t *testing.T) {
	ctx := context.Background()
	e := newDiskEnv(t)
	vm, _ := vmWithDisk(t, e, "v")
	snap := readySnapshot(t, e, vm.ID, "s")
	fork, err := e.s.ForkVM(ctx, e.org.ID, snap, "f", false, e.user)
	if err != nil {
		t.Fatal(err)
	}
	if err := e.s.DeleteSnapshot(ctx, e.org.ID, vm.ID, snap.ID); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("fork disk not ready: %v", err)
	}
	storeAllFiles(t, e)
	if err := e.s.DeleteSnapshot(ctx, e.org.ID, vm.ID, snap.ID); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("fork not started: %v", err)
	}
	if err := e.s.SetVMStatus(ctx, e.node.ID, proto.VMStatus{ID: fork.ID.String(), State: proto.VMState("running")}); err != nil {
		t.Fatal(err)
	}
	if err := e.s.DeleteSnapshot(ctx, e.org.ID, vm.ID, snap.ID); err != nil {
		t.Fatalf("fork started: %v", err)
	}
	if _, err := e.s.Snapshot(ctx, e.org.ID, vm.ID, snap.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("deleted snapshot: %v", err)
	}
	st, _ := e.s.StorageState(ctx)
	if len(st.Deleted) != 1 || st.Deleted[0] != snap.Disks[0].ID.String() {
		t.Fatalf("deleted copies: %v", st.Deleted)
	}
}

func TestForkVM(t *testing.T) {
	ctx := context.Background()
	e := newDiskEnv(t)
	vm, _ := vmWithDisk(t, e, "v")
	n := mustCreateNetwork(t, e.s, e.org.ID, e.user, "net", "")
	if _, err := e.s.SetVMNetworks(ctx, e.org.ID, vm.ID, []uuid.UUID{n.ID}, false); err != nil {
		t.Fatal(err)
	}
	vm, _ = e.s.VM(ctx, e.org.ID, vm.ID)
	snap := readySnapshot(t, e, vm.ID, "s")

	f, err := e.s.ForkVM(ctx, e.org.ID, snap, "f", true, e.user)
	if err != nil {
		t.Fatal(err)
	}
	if f.UserID == nil || *f.UserID != e.user || f.NodeID != vm.NodeID || f.CPUs != vm.CPUs || f.MemoryMB != vm.MemoryMB || f.DiskGB != vm.DiskGB ||
		!f.MgmtIP.IsValid() || f.MgmtIP == vm.MgmtIP || f.Name != "f" {
		t.Fatalf("fork: %+v (source %+v)", f, vm)
	}
	var forkSnap, forkSource uuid.UUID
	var forkMemory bool
	if err := e.pool.QueryRow(ctx, "SELECT fork_snapshot_id, fork_source_vm, fork_memory FROM vms WHERE id = $1", f.ID).Scan(&forkSnap, &forkSource, &forkMemory); err != nil ||
		forkSnap != snap.ID || forkSource != vm.ID || forkMemory {
		t.Fatalf("fork columns: %v %v %v %v", forkSnap, forkSource, forkMemory, err)
	}
	if nics, _ := e.s.VMNetworks(ctx, f.ID); len(nics) != 1 || nics[0].NetworkID != n.ID {
		t.Fatalf("nics: %+v", nics)
	}
	disks, _ := e.s.VMDisks(ctx, f.ID)
	if len(disks) != 1 || disks[0].MountPath != "/data" {
		t.Fatalf("disks: %+v", disks)
	}
	d, _ := e.s.Disk(ctx, e.org.ID, disks[0].DiskID)
	if !regexp.MustCompile(`^v-data-[0-9a-f]{4}$`).MatchString(d.Name) || d.SizeGB != 1 || d.ReadyGB != 0 {
		t.Fatalf("disk: %+v", d)
	}
	var copyFrom uuid.UUID
	if err := e.pool.QueryRow(ctx, "SELECT copy_from_snapshot FROM disks WHERE id = $1", d.ID).Scan(&copyFrom); err != nil || copyFrom != snap.Disks[0].ID {
		t.Fatalf("copy_from_snapshot: %v %v", copyFrom, err)
	}
	if exportOf(t, e, d.ID) == nil {
		t.Fatal("no export")
	}
	if sd := storageDisk(t, e, d.ID); sd == nil || sd.CopyFrom != snap.Disks[0].ID.String() || !sd.Create {
		t.Fatalf("storage: %+v", sd)
	}

	// memory with another NIC count than the snapshot
	n2 := mustCreateNetwork(t, e.s, e.org.ID, e.user, "net2", "")
	if _, err := e.s.SetVMNetworks(ctx, e.org.ID, vm.ID, []uuid.UUID{n.ID, n2.ID}, true); err != nil {
		t.Fatal(err)
	}
	execSQL(t, e.pool, "UPDATE vm_snapshots SET memory = true WHERE id = $1", snap.ID)
	if _, err := e.s.ForkVM(ctx, e.org.ID, snap, "g", true, e.user); !errors.Is(err, store.ErrShapeChanged) {
		t.Fatalf("shape: %v", err)
	}

	if err := e.s.SetLimits(ctx, e.org.ID, "user:"+e.user.String(), store.Limits{VMCount: ip(2)}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.s.ForkVM(ctx, e.org.ID, snap, "g", false, e.user); !errors.Is(err, store.ErrLimitExceeded) {
		t.Fatalf("vm limit: %v", err)
	}
	if err := e.pool.QueryRow(ctx, "SELECT id FROM vms WHERE name = 'g'").Scan(new(uuid.UUID)); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("vm created: %v", err)
	}
	if all, _ := e.s.Disks(ctx, e.org.ID); len(all) != 2 {
		t.Fatalf("disks created: %+v", all)
	}
}

func TestLimitsSnapshotCountRoundTrip(t *testing.T) {
	ctx := context.Background()
	e := newVMEnv(t)
	if err := e.s.SetLimits(ctx, e.org.ID, "org", store.Limits{SnapshotCount: ip(5)}); err != nil {
		t.Fatal(err)
	}
	if err := e.s.SetLimits(ctx, e.org.ID, "user:"+e.user.String(), store.Limits{SnapshotCount: ip(6)}); !errors.Is(err, store.ErrLimitExceedsOrg) {
		t.Fatalf("user above org: %v", err)
	}
	if err := e.s.SetLimits(ctx, e.org.ID, "user:"+e.user.String(), store.Limits{SnapshotCount: ip(3)}); err != nil {
		t.Fatal(err)
	}
	if err := e.s.SetLimits(ctx, e.org.ID, "org", store.Limits{SnapshotCount: ip(2)}); !errors.Is(err, store.ErrLimitExceedsOrg) {
		t.Fatalf("org below user: %v", err)
	}
	ls, err := e.s.Limits(ctx, e.org.ID)
	if err != nil || *ls["org"].SnapshotCount != 5 || *ls["user:"+e.user.String()].SnapshotCount != 3 || ls["org"].VCPU != nil {
		t.Fatalf("limits: %+v %v", ls, err)
	}
}

func TestStorageStateNeverExportsSnapshotDisks(t *testing.T) {
	ctx := context.Background()
	e := newDiskEnv(t)
	vm, d := vmWithDisk(t, e, "v")
	snap := readySnapshot(t, e, vm.ID, "s")
	st, err := e.s.StorageState(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(st.Exports) != 1 || st.Exports[0].DiskID != d.ID.String() {
		t.Fatalf("exports: %+v", st.Exports)
	}
	if storageDisk(t, e, snap.Disks[0].ID) == nil {
		t.Fatal("copy missing from disks")
	}
}

func TestDeleteDiskWithSnapshotCopyInFlight(t *testing.T) {
	ctx := context.Background()
	e := newDiskEnv(t)
	vm, d := vmWithDisk(t, e, "v")
	s := mustSnapshot(t, e, vm.ID, "s", false)
	reportSnapshot(t, e, e.node.ID, vm.ID, s.ID)
	if _, _, err := e.s.SetVMDisks(ctx, e.org.ID, vm.ID, []store.VMDisk{}, true); err != nil {
		t.Fatal(err)
	}
	if err := e.s.ForceRelease(ctx, e.org.ID, d.ID); err != nil {
		t.Fatal(err)
	}
	if err := e.s.DeleteDisk(ctx, e.org.ID, d.ID); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("copy in flight: %v", err)
	}
	storeAllFiles(t, e)
	if err := e.s.DeleteDisk(ctx, e.org.ID, d.ID); err != nil {
		t.Fatalf("copy ready: %v", err)
	}
}

func TestForkVMTakesSnapshotShape(t *testing.T) {
	ctx := context.Background()
	e := newDiskEnv(t)
	vm := mustStoppedVM(t, e, "v", nil)
	snap := readySnapshot(t, e, vm.ID, "s")
	execSQL(t, e.pool, "UPDATE vms SET cpus = 2, memory_mb = 2, disk_gb = 2 WHERE id = $1", vm.ID)
	f, err := e.s.ForkVM(ctx, e.org.ID, snap, "f", false, e.user)
	if err != nil {
		t.Fatal(err)
	}
	if f.CPUs != snap.CPUs || f.MemoryMB != snap.MemoryMB || f.DiskGB != snap.DiskGB {
		t.Fatalf("fork %+v, snapshot %+v", f, snap)
	}
}

func TestApplySnapshotStatusSanitizesNodeValues(t *testing.T) {
	ctx := context.Background()
	e := newDiskEnv(t)
	vm := mustStoppedVM(t, e, "v", nil)
	neg := mustSnapshot(t, e, vm.ID, "neg", false)
	bad := mustSnapshot(t, e, vm.ID, "bad", false)
	long := strings.Repeat("x", 1000)
	if _, err := e.s.ApplySnapshotStatus(ctx, e.node.ID, proto.VMStatus{ID: vm.ID.String(), Snapshots: []proto.SnapshotInfo{
		{ID: neg.ID.String(), SizeBytes: -5}, {ID: bad.ID.String(), Error: long}}}); err != nil {
		t.Fatal(err)
	}
	if got, _ := e.s.Snapshot(ctx, e.org.ID, vm.ID, neg.ID); got.SizeBytes != 0 {
		t.Fatalf("size %d", got.SizeBytes)
	}
	if got, _ := e.s.Snapshot(ctx, e.org.ID, vm.ID, bad.ID); got.State != "error" || len(got.Error) != 500 {
		t.Fatalf("error length %d", len(got.Error))
	}
}

func TestDeleteVMPendingForkSource(t *testing.T) {
	ctx := context.Background()
	e := newDiskEnv(t)
	vm, _ := vmWithDisk(t, e, "v")
	snap := readySnapshot(t, e, vm.ID, "s")
	fork, err := e.s.ForkVM(ctx, e.org.ID, snap, "f", false, e.user)
	if err != nil {
		t.Fatal(err)
	}
	if err := e.s.DeleteVM(ctx, e.org.ID, vm.ID); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("source of a pending fork: %v", err)
	}
	if err := e.s.SetVMStatus(ctx, e.node.ID, proto.VMStatus{ID: fork.ID.String(), State: proto.StateRunning}); err != nil {
		t.Fatal(err)
	}
	if err := e.s.DeleteVM(ctx, e.org.ID, vm.ID); err != nil {
		t.Fatalf("fork provisioned: %v", err)
	}
}

func TestRequestRestoreWhileSnapshotting(t *testing.T) {
	ctx := context.Background()
	e := newDiskEnv(t)
	vm := mustStoppedVM(t, e, "v", nil)
	ready := readySnapshot(t, e, vm.ID, "r")
	creating := mustSnapshot(t, e, vm.ID, "c", false)
	if _, err := e.s.RequestRestore(ctx, e.org.ID, vm.ID, ready.ID, false, true); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("restore during a snapshot: %v", err)
	}
	if err := e.s.DeleteSnapshot(ctx, e.org.ID, vm.ID, creating.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := e.s.RequestRestore(ctx, e.org.ID, vm.ID, ready.ID, false, true); err != nil {
		t.Fatalf("after the snapshot is gone: %v", err)
	}
}

func snapshotErrorReport(t *testing.T, e vmEnv, vm, snap uuid.UUID, msg string) {
	t.Helper()
	if _, err := e.s.ApplySnapshotStatus(context.Background(), e.node.ID, proto.VMStatus{ID: vm.String(), Snapshots: []proto.SnapshotInfo{{ID: snap.String(), Error: msg}}}); err != nil {
		t.Fatal(err)
	}
}

func TestApplySnapshotStatusErrorAfterReport(t *testing.T) {
	ctx := context.Background()
	e := newDiskEnv(t)
	vm, _ := vmWithDisk(t, e, "v")
	s := mustSnapshot(t, e, vm.ID, "s", false)
	reportSnapshot(t, e, e.node.ID, vm.ID, s.ID)
	snapshotErrorReport(t, e, vm.ID, s.ID, "VM resumed before the snapshot completed")
	got, _ := e.s.Snapshot(ctx, e.org.ID, vm.ID, s.ID)
	if got.State != "error" || got.Error != "VM resumed before the snapshot completed" || len(got.Disks) != 0 {
		t.Fatalf("creating: %+v", got)
	}
	storeAllFiles(t, e)
	if got, _ = e.s.Snapshot(ctx, e.org.ID, vm.ID, s.ID); got.State != "error" {
		t.Fatalf("after files: %+v", got)
	}

	ready := readySnapshot(t, e, vm.ID, "r")
	snapshotErrorReport(t, e, vm.ID, ready.ID, "VM resumed before the snapshot completed")
	if got, _ = e.s.Snapshot(ctx, e.org.ID, vm.ID, ready.ID); got.State != "error" || len(got.Disks) != 0 {
		t.Fatalf("ready: %+v", got)
	}
}

func TestLateStorageCopyDoesNotReadySnapshot(t *testing.T) {
	ctx := context.Background()
	e := newDiskEnv(t)
	vm, _ := vmWithDisk(t, e, "v")
	s := mustSnapshot(t, e, vm.ID, "s", false)
	reportSnapshot(t, e, e.node.ID, vm.ID, s.ID)
	execSQL(t, e.pool, "UPDATE vm_snapshots SET reported_at = now() - interval '3 minutes' WHERE id = $1", s.ID)
	storeAllFiles(t, e)
	if got, _ := e.s.Snapshot(ctx, e.org.ID, vm.ID, s.ID); got.State != "creating" {
		t.Fatalf("late copy: %+v", got)
	}
}

func TestFailUnreportedSnapshots(t *testing.T) {
	ctx := context.Background()
	e := newDiskEnv(t)
	vm := mustStoppedVM(t, e, "v", nil)
	lost := mustSnapshot(t, e, vm.ID, "lost", false)
	fresh := mustSnapshot(t, e, vm.ID, "fresh", false)
	execSQL(t, e.pool, "UPDATE vm_snapshots SET created_at = now() - interval '11 minutes' WHERE id = $1", lost.ID)
	if orgs, err := e.s.FailStaleSnapshots(ctx, 2*time.Minute); err != nil || len(orgs) != 1 {
		t.Fatalf("orgs: %v %v", orgs, err)
	}
	if got, _ := e.s.Snapshot(ctx, e.org.ID, vm.ID, lost.ID); got.State != "error" || got.Error != "snapshot never reached the node" {
		t.Fatalf("lost: %+v", got)
	}
	if got, _ := e.s.Snapshot(ctx, e.org.ID, vm.ID, fresh.ID); got.State != "creating" {
		t.Fatalf("fresh: %+v", got)
	}
}

func TestCreateSnapshotDuringRestore(t *testing.T) {
	ctx := context.Background()
	e := newDiskEnv(t)
	vm, _ := vmWithDisk(t, e, "v")
	snap := readySnapshot(t, e, vm.ID, "s")
	if _, err := e.s.RequestRestore(ctx, e.org.ID, vm.ID, snap.ID, false, false); err != nil {
		t.Fatal(err)
	}
	if _, err := e.s.CreateSnapshot(ctx, e.org.ID, vm.ID, "n", false, e.user); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("restore on the node: %v", err)
	}
	if _, err := e.s.ApplySnapshotStatus(ctx, e.node.ID, proto.VMStatus{ID: vm.ID.String(), RestoredSeq: 1}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.s.CreateSnapshot(ctx, e.org.ID, vm.ID, "n", false, e.user); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("restore on storage: %v", err)
	}
}

func TestDiskWithPendingRestoreNotAttachable(t *testing.T) {
	ctx := context.Background()
	e := newDiskEnv(t)
	vm, d := vmWithDisk(t, e, "v")
	other := mustStoppedVM(t, e, "w", nil)
	snap := readySnapshot(t, e, vm.ID, "s")
	if _, err := e.s.RequestRestore(ctx, e.org.ID, vm.ID, snap.ID, false, false); err != nil {
		t.Fatal(err)
	}
	if _, err := e.s.ApplySnapshotStatus(ctx, e.node.ID, proto.VMStatus{ID: vm.ID.String(), RestoredSeq: 1}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := e.s.SetVMDisks(ctx, e.org.ID, vm.ID, []store.VMDisk{}, true); err != nil {
		t.Fatal(err)
	}
	if err := e.s.ForceRelease(ctx, e.org.ID, d.ID); err != nil {
		t.Fatal(err)
	}
	if _, _, err := e.s.SetVMDisks(ctx, e.org.ID, other.ID, []store.VMDisk{{DiskID: d.ID, MountPath: "/data"}}, true); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("attach during restore: %v", err)
	}
}

func TestHibernatedForkIsProvisioned(t *testing.T) {
	ctx := context.Background()
	e := newDiskEnv(t)
	vm, _ := vmWithDisk(t, e, "v")
	snap := readySnapshot(t, e, vm.ID, "s")
	fork, err := e.s.ForkVM(ctx, e.org.ID, snap, "f", false, e.user)
	if err != nil {
		t.Fatal(err)
	}
	storeAllFiles(t, e)
	if err := e.s.SetVMStatus(ctx, e.node.ID, proto.VMStatus{ID: fork.ID.String(), State: proto.StateHibernated}); err != nil {
		t.Fatal(err)
	}
	plans, err := e.s.NodeVMPlans(ctx, e.node.ID)
	if err != nil {
		t.Fatal(err)
	}
	if p := plans[fork.ID]; p.Fork != nil || p.Hold {
		t.Fatalf("plan: %+v", p)
	}
}

func TestForkDiskNamesStayValid(t *testing.T) {
	ctx := context.Background()
	e := newDiskEnv(t)
	long := strings.Repeat("a", 58)
	for _, tc := range []struct{ vm, disk, want string }{
		{long, long + "-data", "^" + long + "-[0-9a-f]{4}$"},
		{"v", "v-data-ab12", "^v-data-[0-9a-f]{4}$"},
	} {
		vm, d := vmWithDisk(t, e, tc.vm)
		execSQL(t, e.pool, "UPDATE disks SET name = $2 WHERE id = $1", d.ID, tc.disk)
		snap := readySnapshot(t, e, vm.ID, "s")
		fork, err := e.s.ForkVM(ctx, e.org.ID, snap, tc.vm[:1]+"-fork", false, e.user)
		if err != nil {
			t.Fatal(err)
		}
		disks, _ := e.s.VMDisks(ctx, fork.ID)
		got, _ := e.s.Disk(ctx, e.org.ID, disks[0].DiskID)
		if !regexp.MustCompile(tc.want).MatchString(got.Name) {
			t.Fatalf("name %q, want %s", got.Name, tc.want)
		}
	}
}

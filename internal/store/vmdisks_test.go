package store_test

import (
	"context"
	"encoding/hex"
	"errors"
	"testing"

	"github.com/google/uuid"

	"github.com/asterism-dev/observatory/internal/netaddr"
	"github.com/asterism-dev/observatory/internal/proto"
	"github.com/asterism-dev/observatory/internal/store"
)

func newDiskEnv(t *testing.T) vmEnv {
	e := newVMEnv(t)
	e.s = e.s.WithMasterKey(testKey(1))
	return e
}

func mustStoppedVM(t *testing.T, e vmEnv, name string, owner *uuid.UUID) store.VM {
	t.Helper()
	ctx := context.Background()
	v, err := e.s.CreateVM(ctx, e.vm(name, owner, 1, 1, 1))
	if err != nil {
		t.Fatal(err)
	}
	if v, err = e.s.SetVMPower(ctx, e.org.ID, v.ID, proto.PowerStopped, proto.StopHard); err != nil {
		t.Fatal(err)
	}
	return v
}

type export struct {
	port      int
	sealed    []byte
	releasing bool
	seq       *int64
}

func exportOf(t *testing.T, e vmEnv, disk uuid.UUID) *export {
	t.Helper()
	var x export
	err := e.pool.QueryRow(context.Background(), "SELECT port, sealed, releasing, release_seq FROM disk_exports WHERE disk_id = $1", disk).
		Scan(&x.port, &x.sealed, &x.releasing, &x.seq)
	if err != nil {
		return nil
	}
	return &x
}

func attachment(t *testing.T, e vmEnv, node, vm, disk uuid.UUID) proto.DiskAttachment {
	t.Helper()
	all, err := e.s.NodeAttachments(context.Background(), node)
	if err != nil {
		t.Fatal(err)
	}
	for _, a := range all[vm] {
		if a.DiskID == disk.String() {
			return a
		}
	}
	t.Fatalf("no attachment of %s on %s", disk, vm)
	return proto.DiskAttachment{}
}

func TestSetVMDisksAttach(t *testing.T) {
	ctx := context.Background()
	e := newDiskEnv(t)
	vm := mustStoppedVM(t, e, "v", nil)
	d1 := mustCreateDisk(t, e, e.disk("d1", nil, 1))
	d2 := mustCreateDisk(t, e, e.disk("d2", nil, 1))

	got, err := e.s.SetVMDisks(ctx, e.org.ID, vm.ID, []store.VMDisk{{DiskID: d1.ID, MountPath: "/data"}}, false)
	if err != nil {
		t.Fatal(err)
	}
	if got.Generation != vm.Generation+1 {
		t.Fatalf("generation %d, want %d", got.Generation, vm.Generation+1)
	}
	if x := exportOf(t, e, d1.ID); x == nil || x.port != 10810 || x.releasing {
		t.Fatalf("export: %+v", x)
	}
	execSQL(t, e.pool, "UPDATE disks SET ready_gb = size_gb WHERE id = $1", d1.ID)
	if psk, err := hex.DecodeString(attachment(t, e, e.node.ID, vm.ID, d1.ID).PSK); err != nil || len(psk) != 32 {
		t.Fatalf("psk: %x, %v", psk, err)
	}

	list := []store.VMDisk{{DiskID: d1.ID, MountPath: "/data"}, {DiskID: d2.ID, MountPath: "/data2"}}
	got, err = e.s.SetVMDisks(ctx, e.org.ID, vm.ID, list, false)
	if err != nil {
		t.Fatal(err)
	}
	if x := exportOf(t, e, d2.ID); x == nil || x.port != 10811 {
		t.Fatalf("second export: %+v", x)
	}
	again, err := e.s.SetVMDisks(ctx, e.org.ID, vm.ID, list, false)
	if err != nil || again.Generation != got.Generation {
		t.Fatalf("same list: generation %d -> %d, %v", got.Generation, again.Generation, err)
	}
	if d, _ := e.s.Disk(ctx, e.org.ID, d2.ID); d.Attachment == nil || d.Attachment.MountPath != "/data2" {
		t.Fatalf("attachment: %+v", d.Attachment)
	}
}

func TestSetVMDisksRefusals(t *testing.T) {
	ctx := context.Background()
	e := newDiskEnv(t)
	vmA := mustStoppedVM(t, e, "a", nil)
	vmB := mustStoppedVM(t, e, "b", nil)
	teamDisk := mustCreateDisk(t, e, e.disk("team", &e.team.ID, 1))
	onB := mustCreateDisk(t, e, e.disk("onb", nil, 1))
	releasing := mustCreateDisk(t, e, e.disk("rel", nil, 1))
	for _, list := range [][]store.VMDisk{
		{{DiskID: onB.ID, MountPath: "/data"}, {DiskID: releasing.ID, MountPath: "/rel"}},
		{{DiskID: onB.ID, MountPath: "/data"}},
	} {
		if _, err := e.s.SetVMDisks(ctx, e.org.ID, vmB.ID, list, false); err != nil {
			t.Fatal(err)
		}
	}
	free := mustCreateDisk(t, e, e.disk("free", nil, 1))
	running, err := e.s.CreateVM(ctx, e.vm("run", nil, 1, 1, 1))
	if err != nil {
		t.Fatal(err)
	}
	other, err := e.s.CreateOrg(ctx, "B", e.user)
	if err != nil {
		t.Fatal(err)
	}
	foreign := mustCreateDisk(t, e, store.Disk{OrgID: other.ID, UserID: &e.user, Name: "x", SizeGB: 1, CreatedBy: e.user})

	for _, c := range []struct {
		name string
		vm   uuid.UUID
		disk uuid.UUID
		want error
	}{
		{"team disk on user vm", vmA.ID, teamDisk.ID, store.ErrOwnerMismatch},
		{"attached to b", vmA.ID, onB.ID, store.ErrConflict},
		{"releasing", vmA.ID, releasing.ID, store.ErrConflict},
		{"running without flag", running.ID, free.ID, store.ErrRestartRequired},
		{"other org", vmA.ID, foreign.ID, store.ErrNotFound},
	} {
		if _, err := e.s.SetVMDisks(ctx, e.org.ID, c.vm, []store.VMDisk{{DiskID: c.disk, MountPath: "/data"}}, false); !errors.Is(err, c.want) {
			t.Errorf("%s: %v, want %v", c.name, err, c.want)
		}
	}
	if x := exportOf(t, e, free.ID); x != nil {
		t.Fatalf("export after refusal: %+v", x)
	}
	if _, err := e.s.SetVMDisks(ctx, e.org.ID, running.ID, []store.VMDisk{{DiskID: free.ID, MountPath: "/data"}}, true); err != nil {
		t.Fatalf("running with flag: %v", err)
	}
}

func TestDetachMarksReleasing(t *testing.T) {
	ctx := context.Background()
	e := newDiskEnv(t)
	vm := mustStoppedVM(t, e, "v", nil)
	d := mustCreateDisk(t, e, e.disk("d", nil, 1))
	if _, err := e.s.SetVMDisks(ctx, e.org.ID, vm.ID, []store.VMDisk{{DiskID: d.ID, MountPath: "/data"}}, false); err != nil {
		t.Fatal(err)
	}
	if _, err := e.s.SetVMDisks(ctx, e.org.ID, vm.ID, nil, false); err != nil {
		t.Fatal(err)
	}
	if x := exportOf(t, e, d.ID); x == nil || !x.releasing || x.seq != nil {
		t.Fatalf("export: %+v", x)
	}
	if ids, err := e.s.ReleasingDisks(ctx, e.node.ID); err != nil || len(ids) != 1 || ids[0] != d.ID {
		t.Fatalf("releasing: %v, %v", ids, err)
	}

	vm2 := mustStoppedVM(t, e, "v2", nil)
	d2 := mustCreateDisk(t, e, e.disk("d2", nil, 1))
	if _, err := e.s.SetVMDisks(ctx, e.org.ID, vm2.ID, []store.VMDisk{{DiskID: d2.ID, MountPath: "/data"}}, false); err != nil {
		t.Fatal(err)
	}
	if err := e.s.DeleteVM(ctx, e.org.ID, vm2.ID); err != nil {
		t.Fatal(err)
	}
	if x := exportOf(t, e, d2.ID); x == nil || !x.releasing {
		t.Fatalf("export after delete: %+v", x)
	}
	if ids, err := e.s.ReleasingDisks(ctx, e.node.ID); err != nil || len(ids) != 2 {
		t.Fatalf("releasing after delete: %v, %v", ids, err)
	}
}

func TestConfirmReleases(t *testing.T) {
	ctx := context.Background()
	e := newDiskEnv(t)
	vm := mustStoppedVM(t, e, "v", nil)
	d := mustCreateDisk(t, e, e.disk("d", nil, 1))
	unmarked := mustCreateDisk(t, e, e.disk("u", nil, 1))
	both := []store.VMDisk{{DiskID: d.ID, MountPath: "/data"}, {DiskID: unmarked.ID, MountPath: "/data2"}}
	if _, err := e.s.SetVMDisks(ctx, e.org.ID, vm.ID, both, false); err != nil {
		t.Fatal(err)
	}
	if _, err := e.s.SetVMDisks(ctx, e.org.ID, vm.ID, nil, false); err != nil {
		t.Fatal(err)
	}
	if err := e.s.MarkReleaseSeq(ctx, e.node.ID, []uuid.UUID{d.ID}, 100); err != nil {
		t.Fatal(err)
	}
	if err := e.s.MarkReleaseSeq(ctx, e.node.ID, []uuid.UUID{d.ID}, 200); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		applied uint64
		inUse   []string
		want    int
	}{{99, nil, 0}, {100, []string{d.ID.String()}, 0}, {100, nil, 1}} {
		got, err := e.s.ConfirmReleases(ctx, e.node.ID, c.inUse, c.applied)
		if err != nil || len(got) != c.want || (c.want == 1 && got[0] != d.ID) {
			t.Fatalf("confirm(%d, %v) = %v, %v", c.applied, c.inUse, got, err)
		}
	}
	if x := exportOf(t, e, d.ID); x != nil {
		t.Fatalf("export left: %+v", x)
	}
	if x := exportOf(t, e, unmarked.ID); x == nil || !x.releasing {
		t.Fatalf("unmarked export confirmed: %+v", x)
	}
}

func TestForceReleaseThenReattachNewKey(t *testing.T) {
	ctx := context.Background()
	e := newDiskEnv(t)
	vmA := mustStoppedVM(t, e, "a", nil)
	vmB := mustStoppedVM(t, e, "b", nil)
	blocker := mustCreateDisk(t, e, e.disk("blocker", nil, 1))
	d := mustCreateDisk(t, e, e.disk("d", nil, 1))
	if _, err := e.s.SetVMDisks(ctx, e.org.ID, vmA.ID, []store.VMDisk{{DiskID: d.ID, MountPath: "/data"}}, false); err != nil {
		t.Fatal(err)
	}
	if err := e.s.ForceRelease(ctx, e.org.ID, d.ID); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("force release of attached disk: %v", err)
	}
	old := exportOf(t, e, d.ID)
	if _, err := e.s.SetVMDisks(ctx, e.org.ID, vmA.ID, nil, false); err != nil {
		t.Fatal(err)
	}
	if err := e.s.ForceRelease(ctx, e.org.ID, d.ID); err != nil {
		t.Fatal(err)
	}
	if x := exportOf(t, e, d.ID); x != nil {
		t.Fatalf("export left: %+v", x)
	}
	// blocker takes the freed port first, so the port must change along with the key
	if _, err := e.s.SetVMDisks(ctx, e.org.ID, vmB.ID, []store.VMDisk{{DiskID: blocker.ID, MountPath: "/b"}, {DiskID: d.ID, MountPath: "/data"}}, false); err != nil {
		t.Fatal(err)
	}
	fresh := exportOf(t, e, d.ID)
	if fresh == nil || fresh.port == old.port || string(fresh.sealed) == string(old.sealed) {
		t.Fatalf("reattach: old %+v, new %+v", old, fresh)
	}
	if err := e.s.ForceRelease(ctx, e.org.ID, d.ID); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("force release of non-releasing disk: %v", err)
	}
}

func TestNodeAttachmentsScoped(t *testing.T) {
	ctx := context.Background()
	e := newDiskEnv(t)
	n2, tok, err := e.s.CreateNode(ctx, store.Node{OrgID: e.org.ID, Name: "n2", CreatedBy: e.user})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := e.s.EnrollNode(ctx, tok); err != nil {
		t.Fatal(err)
	}
	v1 := mustStoppedVM(t, e, "v1", nil)
	spec := e.vm("v2", nil, 1, 1, 1)
	spec.NodeID = n2.ID
	v2, err := e.s.CreateVM(ctx, spec)
	if err != nil {
		t.Fatal(err)
	}
	pending := mustCreateDisk(t, e, e.disk("pending", nil, 1))
	ready := mustCreateDisk(t, e, e.disk("ready", nil, 1))
	onN2 := mustCreateDisk(t, e, e.disk("onn2", nil, 1))
	if _, err := e.s.SetVMDisks(ctx, e.org.ID, v1.ID, []store.VMDisk{{DiskID: pending.ID, MountPath: "/p"}, {DiskID: ready.ID, MountPath: "/r"}}, false); err != nil {
		t.Fatal(err)
	}
	if _, err := e.s.SetVMDisks(ctx, e.org.ID, v2.ID, []store.VMDisk{{DiskID: onN2.ID, MountPath: "/data"}}, true); err != nil {
		t.Fatal(err)
	}
	execSQL(t, e.pool, "UPDATE disks SET ready_gb = size_gb WHERE id = ANY($1)", []uuid.UUID{ready.ID, onN2.ID})

	all, err := e.s.NodeAttachments(ctx, e.node.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 1 || len(all[v1.ID]) != 2 {
		t.Fatalf("attachments: %+v", all)
	}
	p, r := all[v1.ID][0], all[v1.ID][1]
	if p.DiskID != pending.ID.String() || p.URL != "" || p.PSK != "" || p.SizeGB != 0 {
		t.Fatalf("pending: %+v", p)
	}
	x := exportOf(t, e, ready.ID)
	if r.DiskID != ready.ID.String() || r.MountPath != "/r" || r.SizeGB != 1 ||
		r.URL != netaddr.StorageURL(x.port, ready.ID.String()) || len(r.PSK) != 64 {
		t.Fatalf("ready: %+v", r)
	}
}

func TestStoragePortsExhausted(t *testing.T) {
	ctx := context.Background()
	e := newDiskEnv(t)
	vm := mustStoppedVM(t, e, "v", nil)
	d := mustCreateDisk(t, e, e.disk("d", nil, 1))
	execSQL(t, e.pool, `
		WITH fill AS (INSERT INTO disks (org_id, user_id, name, size_gb, created_by)
			SELECT $1, $2, 'fill-' || p, 1, $2 FROM generate_series($3::int, $4::int) p RETURNING id, name)
		INSERT INTO disk_exports (disk_id, node_id, port, key_id, nonce, sealed)
		SELECT id, $5, substr(name, 6)::int, 'k', '', '' FROM fill`,
		e.org.ID, e.user, netaddr.StoragePortFirst, netaddr.StoragePortLast, e.node.ID)
	if _, err := e.s.SetVMDisks(ctx, e.org.ID, vm.ID, []store.VMDisk{{DiskID: d.ID, MountPath: "/data"}}, false); !errors.Is(err, store.ErrNoStoragePort) {
		t.Fatalf("attach: %v", err)
	}
}

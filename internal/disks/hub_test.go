package disks

import (
	"bytes"
	"context"
	"slices"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/asterism-dev/observatory/internal/db/dbtest"
	"github.com/asterism-dev/observatory/internal/proto"
	"github.com/asterism-dev/observatory/internal/seal"
	"github.com/asterism-dev/observatory/internal/store"
)

func TestState(t *testing.T) {
	file := &proto.StorageFile{}
	for _, c := range []struct {
		ready, size int
		releasing   bool
		f           *proto.StorageFile
		reported    bool
		want        string
	}{
		{0, 10, false, nil, false, "creating"},
		{0, 10, false, nil, true, "creating"},
		{0, 10, true, nil, true, "releasing"},
		{10, 10, false, nil, true, "missing"},
		{10, 10, true, nil, true, "missing"},
		{10, 20, false, nil, true, "missing"},
		{10, 10, false, nil, false, "ready"},
		{10, 10, true, file, true, "releasing"},
		{10, 20, true, file, true, "releasing"},
		{10, 20, false, file, true, "growing"},
		{10, 20, false, nil, false, "growing"},
		{10, 10, false, file, true, "ready"},
	} {
		d := store.Disk{ReadyGB: c.ready, SizeGB: c.size, Releasing: c.releasing}
		if got := State(d, c.f, c.reported); got != c.want {
			t.Errorf("%+v: %s", c, got)
		}
	}
}

type env struct {
	s        *store.Store
	h        *Hub
	org      uuid.UUID
	user     uuid.UUID
	node     uuid.UUID
	notified []uuid.UUID
}

func newEnv(t *testing.T) *env {
	t.Helper()
	ctx := context.Background()
	s := store.New(dbtest.New(t)).WithMasterKey(seal.Key(bytes.Repeat([]byte{1}, 32)))
	res, err := s.Login(ctx, store.Identity{Subject: "a", Email: "a@x.io", Name: "a"})
	if err != nil {
		t.Fatal(err)
	}
	org, err := s.CreateOrg(ctx, "A", res.User.ID)
	if err != nil {
		t.Fatal(err)
	}
	node, tok, err := s.CreateNode(ctx, store.Node{OrgID: org.ID, Name: "n1", CreatedBy: res.User.ID})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.EnrollNode(ctx, tok); err != nil {
		t.Fatal(err)
	}
	e := &env{s: s, org: org.ID, user: res.User.ID, node: node.ID}
	e.h = New(s, "0123456789abcdef0123456789abcdef", func(org uuid.UUID) { e.notified = append(e.notified, org) })
	return e
}

func (e *env) disk(t *testing.T, name string, size int, copyFrom *uuid.UUID) store.Disk {
	t.Helper()
	d, err := e.s.CreateDisk(context.Background(), store.Disk{OrgID: e.org, UserID: &e.user, Name: name, SizeGB: size, CopyFrom: copyFrom, CreatedBy: e.user})
	if err != nil {
		t.Fatal(err)
	}
	return d
}

func (e *env) get(t *testing.T, id uuid.UUID) store.Disk {
	t.Helper()
	d, err := e.s.Disk(context.Background(), e.org, id)
	if err != nil {
		t.Fatal(err)
	}
	return d
}

func (e *env) report(t *testing.T, files ...proto.StorageFile) {
	t.Helper()
	if err := e.h.SetStatus(context.Background(), proto.StorageStatus{Files: files, Version: "v"}); err != nil {
		t.Fatal(err)
	}
}

func (e *env) state(t *testing.T) proto.StorageState {
	t.Helper()
	st, err := e.h.State(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return st
}

func file(d store.Disk, gb int64) proto.StorageFile {
	return proto.StorageFile{Name: proto.StorageFileName(d.ID.String()), SizeBytes: gb << 30, AllocatedBytes: 4096}
}

func stateDisk(st proto.StorageState, id uuid.UUID) *proto.StorageDisk {
	for _, d := range st.Disks {
		if d.ID == id.String() {
			return &d
		}
	}
	return nil
}

func TestSeenDiskNeverRecreated(t *testing.T) {
	e := newEnv(t)
	d := e.disk(t, "data", 10, nil)
	if sd := stateDisk(e.state(t), d.ID); sd == nil || !sd.Create || sd.SizeBytes != 10<<30 {
		t.Fatalf("before report: %+v", sd)
	}
	e.report(t, file(d, 10))
	if d = e.get(t, d.ID); d.ReadyGB != 10 {
		t.Fatalf("ready %d", d.ReadyGB)
	}
	if info := e.h.Info(d); info.State != "ready" || info.AllocatedBytes != 4096 {
		t.Fatalf("%+v", info)
	}
	if sd := stateDisk(e.state(t), d.ID); sd == nil || sd.Create {
		t.Fatalf("after report: %+v", sd)
	}
	e.report(t)
	if info := e.h.Info(e.get(t, d.ID)); info.State != "missing" {
		t.Fatalf("%+v", info)
	}
	if sd := stateDisk(e.state(t), d.ID); sd == nil || sd.Create {
		t.Fatalf("missing disk recreated: %+v", sd)
	}
}

func TestGrowReadyBumpsVM(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	vm, err := e.s.CreateVM(ctx, store.VM{OrgID: e.org, NodeID: e.node, UserID: &e.user, Name: "v", CPUs: 1, MemoryMB: 1, DiskGB: 1, CreatedBy: e.user})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = e.s.SetVMPower(ctx, e.org, vm.ID, proto.PowerStopped, proto.StopHard); err != nil {
		t.Fatal(err)
	}
	d := e.disk(t, "data", 10, nil)
	if _, _, err := e.s.SetVMDisks(ctx, e.org, vm.ID, []store.VMDisk{{DiskID: d.ID, MountPath: "/data"}}, false); err != nil {
		t.Fatal(err)
	}
	e.report(t, file(d, 10))
	if _, err := e.s.GrowDisk(ctx, e.org, d.ID, 20, false); err != nil {
		t.Fatal(err)
	}
	before, err := e.s.VM(ctx, e.org, vm.ID)
	if err != nil {
		t.Fatal(err)
	}
	e.notified = nil

	e.report(t, file(d, 10))
	if v, err := e.s.VM(ctx, e.org, vm.ID); err != nil || v.Generation != before.Generation || len(e.notified) != 0 {
		t.Fatalf("generation %d -> %d, notified %v, %v", before.Generation, v.Generation, e.notified, err)
	}
	if info := e.h.Info(e.get(t, d.ID)); info.State != "growing" {
		t.Fatalf("%+v", info)
	}

	e.report(t, file(d, 20))
	if v, err := e.s.VM(ctx, e.org, vm.ID); err != nil || v.Generation != before.Generation+1 || !slices.Equal(e.notified, []uuid.UUID{e.org}) {
		t.Fatalf("generation %d -> %d, notified %v, %v", before.Generation, v.Generation, e.notified, err)
	}
	atts, err := e.s.NodeAttachments(ctx, e.node)
	if err != nil || len(atts[vm.ID]) != 1 || atts[vm.ID][0].SizeGB != 20 {
		t.Fatalf("%+v, %v", atts, err)
	}
	st := e.state(t)
	if len(st.Exports) != 1 || st.Exports[0].DiskID != d.ID.String() || len(st.Exports[0].PSK) != 64 {
		t.Fatalf("exports %d", len(st.Exports))
	}
}

func TestDeletedDiskPurged(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	d := e.disk(t, "data", 1, nil)
	e.report(t, file(d, 1))
	if err := e.s.DeleteDisk(ctx, e.org, d.ID); err != nil {
		t.Fatal(err)
	}
	if st := e.state(t); !slices.Equal(st.Deleted, []string{d.ID.String()}) || stateDisk(st, d.ID) != nil {
		t.Fatalf("%+v", st)
	}
	e.report(t, file(d, 1))
	if st := e.state(t); len(st.Deleted) != 1 {
		t.Fatalf("purged while the file exists: %+v", st)
	}
	e.report(t)
	if st := e.state(t); len(st.Deleted) != 0 {
		t.Fatalf("%+v", st)
	}
	// A purged id can't be re-deleted: the row is gone, not only soft-deleted.
	if err := e.s.DeleteDisk(ctx, e.org, d.ID); err != store.ErrNotFound {
		t.Fatal(err)
	}
}

func TestCopyClearedWhenFilePresent(t *testing.T) {
	e := newEnv(t)
	src := e.disk(t, "src", 2, nil)
	e.report(t, file(src, 2))
	c := e.disk(t, "copy", 0, &src.ID)
	if sd := stateDisk(e.state(t), c.ID); sd == nil || sd.CopyFrom != src.ID.String() || !sd.Create || sd.SizeBytes != 2<<30 {
		t.Fatalf("%+v", sd)
	}
	e.report(t, file(src, 2))
	if c = e.get(t, c.ID); c.CopyFrom == nil {
		t.Fatal("copy_from cleared before the copy exists")
	}
	e.report(t, file(src, 2), file(c, 2))
	if c = e.get(t, c.ID); c.CopyFrom != nil || c.ReadyGB != 2 {
		t.Fatalf("%+v", c)
	}
	if sd := stateDisk(e.state(t), c.ID); sd.CopyFrom != "" || sd.Create {
		t.Fatalf("%+v", sd)
	}
}

func TestOnline(t *testing.T) {
	e := newEnv(t)
	now := time.Now()
	e.h.now = func() time.Time { return now }
	if e.h.Online() {
		t.Fatal("online without status")
	}
	e.report(t)
	if !e.h.Online() {
		t.Fatal("offline right after status")
	}
	now = now.Add(11 * time.Second)
	if e.h.Online() {
		t.Fatal("online after 11 s")
	}
}

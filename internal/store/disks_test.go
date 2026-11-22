package store_test

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"

	"github.com/asterism-dev/observatory/internal/store"
)

func (e vmEnv) disk(name string, owner *uuid.UUID, size int) store.Disk {
	d := store.Disk{OrgID: e.org.ID, Name: name, SizeGB: size, CreatedBy: e.user}
	if owner == nil || *owner == e.user {
		d.UserID = &e.user
	} else {
		d.TeamID = owner
	}
	return d
}

func mustCreateDisk(t *testing.T, e vmEnv, d store.Disk) store.Disk {
	t.Helper()
	out, err := e.s.CreateDisk(context.Background(), d)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func TestDiskLimits(t *testing.T) {
	ctx := context.Background()
	e := newVMEnv(t)
	if err := e.s.SetLimits(ctx, e.org.ID, "org", store.Limits{DiskGB: ip(50)}, nil); err != nil {
		t.Fatal(err)
	}
	if err := e.s.SetLimits(ctx, e.org.ID, "user:"+e.user.String(), store.Limits{DiskGB: ip(30)}, policyKnown(t, e.s, e.org.ID)); err != nil {
		t.Fatal(err)
	}
	if _, err := e.s.CreateVM(ctx, e.vm("root", nil, 1, 1, 20)); err != nil {
		t.Fatal(err)
	}
	ten := mustCreateDisk(t, e, e.disk("ten", nil, 10))

	copyOf := e.disk("copy", nil, 0)
	copyOf.CopyFrom = &ten.ID
	for _, c := range []struct {
		name string
		op   func() error
	}{
		{"one more", func() error { _, err := e.s.CreateDisk(ctx, e.disk("one", nil, 1)); return err }},
		{"copy", func() error { _, err := e.s.CreateDisk(ctx, copyOf); return err }},
		{"grow", func() error { _, err := e.s.GrowDisk(ctx, e.org.ID, ten.ID, 11, true); return err }},
		{"team 21", func() error { _, err := e.s.CreateDisk(ctx, e.disk("team21", &e.team.ID, 21)); return err }},
	} {
		if err := c.op(); !errors.Is(err, store.ErrLimitExceeded) {
			t.Errorf("%s: %v", c.name, err)
		}
	}
	mustCreateDisk(t, e, e.disk("team20", &e.team.ID, 20))

	if err := e.s.DeleteDisk(ctx, e.org.ID, ten.ID); err != nil {
		t.Fatal(err)
	}
	mustCreateDisk(t, e, e.disk("again", nil, 10))
}

func TestVMCreateCountsDisks(t *testing.T) {
	ctx := context.Background()
	e := newVMEnv(t)
	if err := e.s.SetLimits(ctx, e.org.ID, "user:"+e.user.String(), store.Limits{DiskGB: ip(30)}, policyKnown(t, e.s, e.org.ID)); err != nil {
		t.Fatal(err)
	}
	mustCreateDisk(t, e, e.disk("data", nil, 25))
	if _, err := e.s.CreateVM(ctx, e.vm("v", nil, 1, 1, 6)); !errors.Is(err, store.ErrLimitExceeded) {
		t.Fatalf("vm over: %v", err)
	}
	if _, err := e.s.CreateVM(ctx, e.vm("v", nil, 1, 1, 5)); err != nil {
		t.Fatalf("vm at limit: %v", err)
	}
}

func TestDiskNamesLive(t *testing.T) {
	ctx := context.Background()
	e := newVMEnv(t)
	other := mustCreateOrg(t, e.s, "B", e.user)
	d := mustCreateDisk(t, e, e.disk("data", nil, 1))
	if _, err := e.s.CreateDisk(ctx, e.disk("data", &e.team.ID, 1)); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("duplicate: %v", err)
	}
	if _, err := e.s.CreateDisk(ctx, store.Disk{OrgID: other.ID, UserID: &e.user, Name: "data", SizeGB: 1, CreatedBy: e.user}); err != nil {
		t.Fatalf("other org: %v", err)
	}
	if err := e.s.DeleteDisk(ctx, e.org.ID, d.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := e.s.Disk(ctx, e.org.ID, d.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("deleted disk visible: %v", err)
	}
	again := mustCreateDisk(t, e, e.disk("data", nil, 2))
	list, err := e.s.Disks(ctx, e.org.ID)
	if err != nil || len(list) != 1 || list[0].ID != again.ID || list[0].SizeGB != 2 || *list[0].UserID != e.user {
		t.Fatalf("list %+v %v", list, err)
	}
	if _, err := e.s.Disk(ctx, other.ID, again.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("foreign org: %v", err)
	}
}

func TestDeleteDiskConflicts(t *testing.T) {
	ctx := context.Background()
	e := newVMEnv(t)
	vm, err := e.s.CreateVM(ctx, e.vm("v", nil, 1, 1, 1))
	if err != nil {
		t.Fatal(err)
	}
	attached := mustCreateDisk(t, e, e.disk("attached", nil, 1))
	execSQL(t, e.pool, "INSERT INTO vm_disks (vm_id, disk_id, mount_path, position) VALUES ($1, $2, '/data', 0)", vm.ID, attached.ID)
	got, err := e.s.Disk(ctx, e.org.ID, attached.ID)
	if err != nil || got.Attachment == nil || got.Attachment.VMID != vm.ID || got.Attachment.VMName != "v" || got.Attachment.MountPath != "/data" {
		t.Fatalf("attachment %+v %v", got, err)
	}
	if err := e.s.DeleteDisk(ctx, e.org.ID, attached.ID); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("attached: %v", err)
	}

	src := mustCreateDisk(t, e, e.disk("src", nil, 3))
	c := e.disk("copy", nil, 0)
	c.CopyFrom = &src.ID
	cp := mustCreateDisk(t, e, c)
	if cp.SizeGB != 3 || cp.CopyFrom == nil || *cp.CopyFrom != src.ID {
		t.Fatalf("copy %+v", cp)
	}
	if err := e.s.DeleteDisk(ctx, e.org.ID, src.ID); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("copy source: %v", err)
	}
	if err := e.s.DeleteDisk(ctx, e.org.ID, cp.ID); err != nil {
		t.Fatal(err)
	}
	if err := e.s.DeleteDisk(ctx, e.org.ID, src.ID); err != nil {
		t.Fatalf("source after copy deleted: %v", err)
	}
	gone := e.disk("gone", nil, 0)
	gone.CopyFrom = &src.ID
	if _, err := e.s.CreateDisk(ctx, gone); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("copy of deleted: %v", err)
	}
}

func TestGrowDiskRestart(t *testing.T) {
	ctx := context.Background()
	e := newVMEnv(t)
	vm, err := e.s.CreateVM(ctx, e.vm("v", nil, 1, 1, 1))
	if err != nil {
		t.Fatal(err)
	}
	d := mustCreateDisk(t, e, e.disk("data", nil, 5))
	execSQL(t, e.pool, "INSERT INTO vm_disks (vm_id, disk_id, mount_path, position) VALUES ($1, $2, '/data', 0)", vm.ID, d.ID)
	if _, err := e.s.GrowDisk(ctx, e.org.ID, d.ID, 6, false); !errors.Is(err, store.ErrRestartRequired) {
		t.Fatalf("running: %v", err)
	}
	for _, p := range []string{"paused", "hibernated"} {
		execSQL(t, e.pool, "UPDATE vms SET power = $2 WHERE id = $1", vm.ID, p)
		if _, err := e.s.GrowDisk(ctx, e.org.ID, d.ID, 6, false); !errors.Is(err, store.ErrRestartRequired) {
			t.Fatalf("%s: %v", p, err)
		}
	}
	execSQL(t, e.pool, "UPDATE vms SET power = 'running' WHERE id = $1", vm.ID)
	if got, _ := e.s.Disk(ctx, e.org.ID, d.ID); got.SizeGB != 5 {
		t.Fatalf("size changed: %d", got.SizeGB)
	}
	if got, err := e.s.GrowDisk(ctx, e.org.ID, d.ID, 6, true); err != nil || got.SizeGB != 6 {
		t.Fatalf("grow %+v %v", got, err)
	}
	if _, err := e.s.GrowDisk(ctx, e.org.ID, d.ID, 6, true); !errors.Is(err, store.ErrInvalidSize) {
		t.Fatalf("equal: %v", err)
	}
	execSQL(t, e.pool, "UPDATE vms SET power = 'stopped' WHERE id = $1", vm.ID)
	if got, err := e.s.GrowDisk(ctx, e.org.ID, d.ID, 7, false); err != nil || got.SizeGB != 7 {
		t.Fatalf("stopped vm: %+v %v", got, err)
	}
	free := mustCreateDisk(t, e, e.disk("free", nil, 1))
	if got, err := e.s.GrowDisk(ctx, e.org.ID, free.ID, 2, false); err != nil || got.SizeGB != 2 {
		t.Fatalf("unattached: %+v %v", got, err)
	}
	execSQL(t, e.pool, `INSERT INTO disk_exports (disk_id, node_id, port, key_id, nonce, sealed, releasing)
		VALUES ($1, $2, 10810, 'k', '\x00', '\x00', true)`, free.ID, e.node.ID)
	if _, err := e.s.GrowDisk(ctx, e.org.ID, free.ID, 3, true); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("releasing: %v", err)
	}
}

func TestDiskLimitsIgnoreOtherDimensions(t *testing.T) {
	ctx := context.Background()
	e := newVMEnv(t)
	for _, name := range []string{"a", "b"} {
		if _, err := e.s.CreateVM(ctx, e.vm(name, nil, 1, 1, 1)); err != nil {
			t.Fatal(err)
		}
	}
	if err := e.s.SetLimits(ctx, e.org.ID, "user:"+e.user.String(), store.Limits{VMCount: ip(1), DiskGB: ip(5)}, policyKnown(t, e.s, e.org.ID)); err != nil {
		t.Fatal(err)
	}
	d := mustCreateDisk(t, e, e.disk("data", nil, 2))
	if _, err := e.s.GrowDisk(ctx, e.org.ID, d.ID, 3, true); err != nil {
		t.Fatalf("grow under vm count overrun: %v", err)
	}
	if _, err := e.s.CreateDisk(ctx, e.disk("big", nil, 1)); !errors.Is(err, store.ErrLimitExceeded) {
		t.Fatalf("disk over: %v", err)
	}
}

func TestOwnsLiveDisks(t *testing.T) {
	ctx := context.Background()
	e := newVMEnv(t)
	b := login(t, e.s, "b")
	addMember(e.s, e.org.ID, b.User.ID, store.Role("member"))
	bd := mustCreateDisk(t, e, store.Disk{OrgID: e.org.ID, UserID: &b.User.ID, Name: "b", SizeGB: 1, CreatedBy: e.user})
	td := mustCreateDisk(t, e, e.disk("t", &e.team.ID, 1))
	owns := func(scope store.EnvScope, id uuid.UUID) bool {
		t.Helper()
		vms, disks, err := e.s.Owns(ctx, e.org.ID, scope, id)
		if err != nil || vms {
			t.Fatalf("owns %s %s: %v %v", scope, id, vms, err)
		}
		return disks
	}
	if !owns(store.EnvScopeUser, b.User.ID) || !owns(store.EnvScopeTeam, e.team.ID) {
		t.Fatal("disk owners not reported")
	}
	for _, id := range []uuid.UUID{bd.ID, td.ID} {
		if err := e.s.DeleteDisk(ctx, e.org.ID, id); err != nil {
			t.Fatal(err)
		}
	}
	if owns(store.EnvScopeUser, b.User.ID) || owns(store.EnvScopeTeam, e.team.ID) {
		t.Fatal("deleted disks still owned")
	}
}

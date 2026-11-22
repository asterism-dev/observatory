package store_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	quotav1 "github.com/asterism-dev/observatory/gen/go/observatory/quota/v1"
	"github.com/asterism-dev/observatory/internal/authz"
	"github.com/asterism-dev/observatory/internal/db/dbtest"
	"github.com/asterism-dev/observatory/internal/platform/quotatest"
	"github.com/asterism-dev/observatory/internal/proto"
	"github.com/asterism-dev/observatory/internal/store"
	"github.com/asterism-dev/observatory/internal/store/storetest"
)

type vmEnv struct {
	s     *store.Store
	quota *quotatest.Fake
	pool  *pgxpool.Pool
	org  testOrg
	user uuid.UUID
	team testTeam
	node store.Node
}

func newVMEnv(t *testing.T) vmEnv {
	t.Helper()
	ctx := context.Background()
	pool := dbtest.New(t)
	quota := quotatest.NewFake()
	s := store.New(pool, storetest.Quota(t, quota))
	a := login(t, s, "a")
	org, _ := createOrg(s, ctx, "A", a.User.ID)
	team, _ := createTeam(s, ctx, org.ID, "core")
	node, tok, err := s.CreateNode(ctx, store.Node{OrgID: org.ID, Name: "n1", CreatedBy: a.User.ID})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.EnrollNode(ctx, tok); err != nil {
		t.Fatal(err)
	}
	return vmEnv{s, quota, pool, org, a.User.ID, team, node}
}

func ip(n int) *int { return &n }

// setLimits sets scope's limits in the fake quota service.
func (e vmEnv) setLimits(scope string, l store.Limits) {
	p := func(v *int) *int32 {
		if v == nil {
			return nil
		}
		n := int32(*v)
		return &n
	}
	e.quota.SetLimits(e.org.ID, scope, &quotav1.Limits{Vcpu: p(l.VCPU), RamMb: p(l.RAMMB), DiskGb: p(l.DiskGB), VmCount: p(l.VMCount), SnapshotCount: p(l.SnapshotCount)})
}

func (e vmEnv) vm(name string, owner *uuid.UUID, cpus, mem, disk int) store.VM {
	v := store.VM{OrgID: e.org.ID, NodeID: e.node.ID, Name: name, CPUs: cpus, MemoryMB: mem, DiskGB: disk, CreatedBy: e.user}
	if owner == nil || *owner == e.user {
		v.UserID = &e.user
	} else {
		v.TeamID = owner
	}
	return v
}

func TestVMLimitsBoundary(t *testing.T) {
	ctx := context.Background()
	dims := []string{"vcpu", "ram", "disk", "count"}
	for _, scopeKind := range []string{"org", "user", "team"} {
		for _, dim := range dims {
			t.Run(scopeKind+"/"+dim, func(t *testing.T) {
				e := newVMEnv(t)
				owner, scope := &e.user, "user:"+e.user.String()
				switch scopeKind {
				case "org":
					scope = "org"
				case "team":
					owner, scope = &e.team.ID, "team:"+e.team.ID.String()
				}
				const limit = 3
				var l store.Limits
				sizes := [3]int{1, 1, 1}
				switch dim {
				case "vcpu":
					l.VCPU, sizes[0] = ip(limit), limit
				case "ram":
					l.RAMMB, sizes[1] = ip(limit), limit
				case "disk":
					l.DiskGB, sizes[2] = ip(limit), limit
				case "count":
					l.VMCount = ip(1)
				}
				e.setLimits(scope, l)
				if _, err := e.s.CreateVM(ctx, e.vm("a", owner, sizes[0], sizes[1], sizes[2])); err != nil {
					t.Fatalf("exactly at limit: %v", err)
				}
				if _, err := e.s.CreateVM(ctx, e.vm("b", owner, 1, 1, 1)); !errors.Is(err, store.ErrLimitExceeded) {
					t.Fatalf("one over: %v", err)
				}
			})
		}
	}
}

func TestVMOwnerScopes(t *testing.T) {
	ctx := context.Background()
	e := newVMEnv(t)
	e.setLimits("team:"+e.team.ID.String(), store.Limits{VMCount: ip(1)})
	e.setLimits("org", store.Limits{VMCount: ip(3)})
	for _, name := range []string{"u1", "u2"} {
		if _, err := e.s.CreateVM(ctx, e.vm(name, nil, 1, 1, 1)); err != nil {
			t.Fatalf("user vm %s must not count against the team: %v", name, err)
		}
	}
	if _, err := e.s.CreateVM(ctx, e.vm("t1", &e.team.ID, 1, 1, 1)); err != nil {
		t.Fatal(err)
	}
	if _, err := e.s.CreateVM(ctx, e.vm("t2", &e.team.ID, 1, 1, 1)); !errors.Is(err, store.ErrLimitExceeded) {
		t.Fatalf("team over: %v", err)
	}
	e.setLimits("org", store.Limits{VMCount: ip(3)})
	if _, err := e.s.CreateVM(ctx, e.vm("u3", nil, 1, 1, 1)); !errors.Is(err, store.ErrLimitExceeded) {
		t.Fatalf("org over: %v", err)
	}
}

func TestVMLifecycleAndNodeScoping(t *testing.T) {
	ctx := context.Background()
	e := newVMEnv(t)
	other, _ := createOrg(e.s, ctx, "B", login(t, e.s, "b").User.ID)
	foreign, _, _ := e.s.CreateNode(ctx, store.Node{OrgID: other.ID, Name: "fn", CreatedBy: e.user})
	v := e.vm("a", nil, 1, 1, 1)
	v.NodeID = foreign.ID
	if _, err := e.s.CreateVM(ctx, v); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("foreign node: %v", err)
	}

	pending, _, _ := e.s.CreateNode(ctx, store.Node{OrgID: e.org.ID, Name: "pending", CreatedBy: e.user})
	v.NodeID = pending.ID
	if _, err := e.s.CreateVM(ctx, v); !errors.Is(err, store.ErrNotEnrolled) {
		t.Fatalf("pending node: %v", err)
	}

	vm, err := e.s.CreateVM(ctx, e.vm("a", nil, 1, 1, 1))
	if err != nil || vm.Power != proto.PowerRunning || vm.State != proto.StatePending || vm.Provisioned {
		t.Fatalf("create: %+v %v", vm, err)
	}
	if _, err := e.s.CreateVM(ctx, e.vm("a", nil, 1, 1, 1)); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("dup name: %v", err)
	}
	if _, err := e.s.VM(ctx, other.ID, vm.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("cross-org: %v", err)
	}

	got, err := e.s.SetVMPower(ctx, e.org.ID, vm.ID, proto.PowerStopped, proto.StopHard)
	if err != nil || got.Power != proto.PowerStopped || got.StopMode != proto.StopHard || got.Generation != vm.Generation+1 {
		t.Fatalf("power: %+v %v", got, err)
	}
	if _, err := e.s.SetVMPower(ctx, e.org.ID, vm.ID, proto.PowerHibernated, ""); !errors.Is(err, store.ErrNotRunning) {
		t.Fatalf("hibernate stopped: %v", err)
	}
	if _, err := e.s.SetVMPower(ctx, e.org.ID, uuid.New(), proto.PowerPaused, ""); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("pause missing: %v", err)
	}

	st := proto.VMStatus{ID: vm.ID.String(), State: proto.StateRunning, IP: "10.0.0.2"}
	if err := e.s.SetVMStatus(ctx, foreign.ID, st); err != nil {
		t.Fatal(err)
	}
	if got, _ := e.s.VM(ctx, e.org.ID, vm.ID); got.State != proto.StatePending || got.IP != "" {
		t.Fatalf("other node's status applied: %+v", got)
	}
	if err := e.s.SetVMStatus(ctx, e.node.ID, st); err != nil {
		t.Fatal(err)
	}
	got, _ = e.s.VM(ctx, e.org.ID, vm.ID)
	if got.State != proto.StateRunning || got.IP != "10.0.0.2" || !got.Provisioned {
		t.Fatalf("status: %+v", got)
	}
	if err := e.s.SetVMStatus(ctx, e.node.ID, proto.VMStatus{ID: vm.ID.String(), State: proto.StateError, Error: "boom"}); err != nil {
		t.Fatal(err)
	}
	if got, _ := e.s.VM(ctx, e.org.ID, vm.ID); !got.Provisioned || got.Error != "boom" {
		t.Fatalf("provisioned must stay: %+v", got)
	}
	if on, _ := e.s.VMsOnNode(ctx, e.node.ID); len(on) != 1 {
		t.Fatalf("on node: %+v", on)
	}

	if err := e.s.DeleteVM(ctx, other.ID, vm.ID, nil, store.DeleteReasonUser); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("cross-org delete: %v", err)
	}
	if err := e.s.DeleteVM(ctx, e.org.ID, vm.ID, nil, store.DeleteReasonUser); err != nil {
		t.Fatal(err)
	}
	if vms, _ := e.s.VMs(ctx, e.org.ID); len(vms) != 0 {
		t.Fatalf("left: %+v", vms)
	}
}

func TestOwnsVMs(t *testing.T) {
	ctx := context.Background()
	e := newVMEnv(t)
	b := login(t, e.s, "b")
	addMember(e.s, e.org.ID, b.User.ID, store.Role("member"))
	v := e.vm("bvm", nil, 1, 1, 1)
	v.UserID = &b.User.ID
	vm, err := e.s.CreateVM(ctx, v)
	if err != nil {
		t.Fatal(err)
	}
	tvm, err := e.s.CreateVM(ctx, e.vm("tvm", &e.team.ID, 1, 1, 1))
	if err != nil {
		t.Fatal(err)
	}
	owns := func(scope store.EnvScope, id uuid.UUID) bool {
		t.Helper()
		vms, disks, err := e.s.Owns(ctx, e.org.ID, scope, id)
		if err != nil || disks {
			t.Fatalf("owns %s %s: %v %v", scope, id, disks, err)
		}
		return vms
	}
	if !owns(store.EnvScopeUser, b.User.ID) || !owns(store.EnvScopeTeam, e.team.ID) {
		t.Fatal("VM owners not reported")
	}
	if _, _, err := e.s.Owns(ctx, e.org.ID, store.EnvScopeOrg, e.org.ID); err == nil {
		t.Fatal("org scope accepted")
	}
	_ = e.s.DeleteVM(ctx, e.org.ID, vm.ID, nil, store.DeleteReasonUser)
	_ = e.s.DeleteVM(ctx, e.org.ID, tvm.ID, nil, store.DeleteReasonUser)
	if owns(store.EnvScopeUser, b.User.ID) || owns(store.EnvScopeTeam, e.team.ID) {
		t.Fatal("deleted VMs still owned")
	}
}

func TestOwnerRefsLoadVMs(t *testing.T) {
	ctx := context.Background()
	e := newVMEnv(t)
	u, err := e.s.CreateVM(ctx, e.vm("u", nil, 1, 1, 1))
	if err != nil {
		t.Fatal(err)
	}
	tv, err := e.s.CreateVM(ctx, e.vm("t", &e.team.ID, 1, 1, 1))
	if err != nil {
		t.Fatal(err)
	}
	refs, err := e.s.OwnerRefs(ctx)
	if err != nil {
		t.Fatal(err)
	}
	got := map[uuid.UUID]authz.OwnedRef{}
	for _, v := range refs {
		if v.Kind == "vms" {
			got[v.ID] = v
		}
	}
	if r := got[u.ID]; r.OrgID != e.org.ID || r.UserID == nil || *r.UserID != e.user || r.TeamID != nil {
		t.Errorf("user vm: %+v", r)
	}
	if r := got[tv.ID]; r.OrgID != e.org.ID || r.TeamID == nil || *r.TeamID != e.team.ID || r.UserID != nil {
		t.Errorf("team vm: %+v", r)
	}
}

func TestSetVMStatusImage(t *testing.T) {
	ctx := context.Background()
	e := newVMEnv(t)
	vm, err := e.s.CreateVM(ctx, e.vm("a", nil, 1, 512, 10))
	if err != nil {
		t.Fatal(err)
	}
	for _, image := range []string{"v1", ""} {
		st := proto.VMStatus{ID: vm.ID.String(), State: proto.StateRunning, Image: image}
		if err := e.s.SetVMStatus(ctx, e.node.ID, st); err != nil {
			t.Fatal(err)
		}
		if got, _ := e.s.VM(ctx, e.org.ID, vm.ID); got.Image != "v1" {
			t.Fatalf("after status %q: image %q", image, got.Image)
		}
	}
}

func TestLockHubOrgSerializesVMCreate(t *testing.T) {
	ctx := context.Background()
	e := newVMEnv(t)
	e.setLimits("org", store.Limits{VMCount: ip(1)})
	locked, release := make(chan struct{}), make(chan struct{})
	held := make(chan error, 1)
	go func() {
		held <- e.s.InTx(ctx, func(tx *store.Store) error {
			if err := tx.LockHubOrg(ctx, e.org.ID); err != nil {
				return err
			}
			close(locked)
			<-release
			return nil
		})
	}()
	<-locked
	errs := make([]error, 2)
	var wg sync.WaitGroup
	for i, name := range []string{"x", "y"} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, errs[i] = e.s.CreateVM(ctx, e.vm(name, nil, 1, 1, 1))
		}()
	}
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
		t.Fatalf("creates ran while the hub org lock was held: %v", errs)
	case <-time.After(200 * time.Millisecond):
	}
	close(release)
	if err := <-held; err != nil {
		t.Fatal(err)
	}
	<-done
	ok := 0
	for _, err := range errs {
		if err == nil {
			ok++
		} else if !errors.Is(err, store.ErrLimitExceeded) {
			t.Fatalf("unexpected: %v", err)
		}
	}
	if ok != 1 {
		t.Fatalf("want exactly one success, got %d (%v)", ok, errs)
	}
}

func TestLockHubOrgCreatesRowForUnknownOrg(t *testing.T) {
	ctx := context.Background()
	e := newVMEnv(t)
	org := uuid.New()
	for range 2 {
		if err := e.s.LockHubOrg(ctx, org); err != nil {
			t.Fatal(err)
		}
	}
	var n int
	if err := e.pool.QueryRow(ctx, "SELECT count(*) FROM hub_orgs WHERE org_id = $1 AND mgmt_slice IS NULL", org).Scan(&n); err != nil || n != 1 {
		t.Fatalf("hub_orgs rows: %d %v", n, err)
	}
}

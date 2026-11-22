package store_test

import (
	"errors"
	"slices"
	"sync"
	"testing"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/proto"

	eventsv1 "github.com/asterism-dev/observatory/gen/go/observatory/events/v1"
	quotav1 "github.com/asterism-dev/observatory/gen/go/observatory/quota/v1"
	"github.com/asterism-dev/observatory/internal/platform/usage"
	"github.com/asterism-dev/observatory/internal/store"
	"github.com/asterism-dev/observatory/internal/store/storetest"
)

// lifecycle returns the outbox's lifecycle and committed events, oldest first.
func (e vmEnv) lifecycle(t *testing.T) []*eventsv1.Event {
	t.Helper()
	var out []*eventsv1.Event
	for _, ev := range storetest.AuditEvents(t, e.pool) {
		if !ev.GetAudit() {
			out = append(out, ev)
		}
	}
	slices.Reverse(out)
	return out
}

func eventTypes(evs []*eventsv1.Event) []string {
	out := make([]string, len(evs))
	for i, ev := range evs {
		out[i] = ev.GetType()
	}
	return out
}

func lastReserve(t *testing.T, e vmEnv) *quotav1.ReserveRequest {
	t.Helper()
	rs := e.quota.Reserves()
	if len(rs) == 0 {
		t.Fatal("no reserve")
	}
	return rs[len(rs)-1]
}

func i32(n int32) *int32 { return &n }

func TestCreateVMReservesAndEmits(t *testing.T) {
	e := newVMEnv(t)
	v, err := e.s.CreateVM(t.Context(), e.vm("a", nil, 2, 512, 10))
	if err != nil {
		t.Fatal(err)
	}
	want := &quotav1.ReserveRequest{OrgId: e.org.ID.String(), Owner: "user:" + e.user.String(), Resources: []string{"vm/" + v.ID.String()},
		Amounts: &quotav1.Amounts{Vcpu: i32(2), RamMb: i32(512), DiskGb: i32(10), VmCount: i32(1)}}
	if r := lastReserve(t, e); !proto.Equal(r, want) {
		t.Fatalf("reserve = %v, want %v", r, want)
	}
	evs := e.lifecycle(t)
	if got := eventTypes(evs); !slices.Equal(got, []string{usage.InstanceCreated, usage.ReservationsCommitted}) {
		t.Fatalf("events = %v", got)
	}
	l, err := usage.Parse(evs[0])
	if err != nil || evs[0].GetResource() != "org/vms/"+v.ID.String() || evs[0].GetOrg() != e.org.ID.String() ||
		l != (usage.Lifecycle{Resource: "vm/" + v.ID.String(), Owner: "user:" + e.user.String(), Amounts: usage.Amounts{VCPU: 2, RAMMB: 512, DiskGB: 10, VMCount: 1}}) {
		t.Fatalf("created = %v %+v %v", evs[0], l, err)
	}
	if ids, err := usage.ParseCommitted(evs[1]); err != nil || len(ids) != 1 {
		t.Fatalf("committed = %v %v", ids, err)
	}
	if n := e.quota.Holds(); n != 1 {
		t.Fatalf("holds = %d, want the committed one", n)
	}
}

func TestRolledBackCreateFreesQuota(t *testing.T) {
	ctx := t.Context()
	e := newVMEnv(t)
	pending, _, err := e.s.CreateNode(ctx, store.Node{OrgID: e.org.ID, Name: "pending", CreatedBy: e.user})
	if err != nil {
		t.Fatal(err)
	}
	v := e.vm("a", nil, 1, 1, 1)
	v.NodeID = pending.ID
	if _, err := e.s.CreateVM(ctx, v); !errors.Is(err, store.ErrNotEnrolled) {
		t.Fatalf("not enrolled: %v", err)
	}
	if n := e.quota.Holds(); n != 0 {
		t.Fatalf("holds after not enrolled = %d", n)
	}
	if _, err := e.s.CreateVM(ctx, e.vm("a", nil, 1, 1, 1)); err != nil {
		t.Fatal(err)
	}
	if _, err := e.s.CreateVM(ctx, e.vm("a", nil, 1, 1, 1)); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("name conflict: %v", err)
	}
	if n := e.quota.Holds(); n != 1 {
		t.Fatalf("holds after name conflict = %d, want 1", n)
	}
	if got := eventTypes(e.lifecycle(t)); !slices.Equal(got, []string{usage.InstanceCreated, usage.ReservationsCommitted}) {
		t.Fatalf("events = %v: a rolled-back create emits nothing", got)
	}
}

func TestConcurrentCreatesAtLimit(t *testing.T) {
	for _, scope := range []string{"org", "owner"} {
		t.Run(scope, func(t *testing.T) {
			e := newVMEnv(t)
			if scope == "org" {
				e.setLimits("org", store.Limits{VMCount: ip(1)})
			} else {
				e.setLimits("user:"+e.user.String(), store.Limits{VCPU: ip(2)})
			}
			errs := make([]error, 4)
			var wg sync.WaitGroup
			for i := range errs {
				wg.Go(func() { _, errs[i] = e.s.CreateVM(t.Context(), e.vm("v"+string(rune('a'+i)), nil, 2, 1, 1)) })
			}
			wg.Wait()
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
		})
	}
}

// The fake never sees the committed events, so every create here is one whose committed event is still pending.
func TestDeleteThenCreateImmediately(t *testing.T) {
	ctx := t.Context()
	e := newDiskEnv(t)
	e.setLimits("org", store.Limits{VMCount: ip(1), SnapshotCount: ip(1), DiskGB: ip(5)})
	v := mustStoppedVM(t, e, "a", nil)
	mustSnapshot(t, e, v.ID, "s", false)

	errRollback := errors.New("rollback")
	if err := e.s.InTx(ctx, func(tx *store.Store) error {
		if err := tx.DeleteVM(ctx, e.org.ID, v.ID, &e.user, store.DeleteReasonUser); err != nil {
			return err
		}
		return errRollback
	}); !errors.Is(err, errRollback) {
		t.Fatal(err)
	}
	if _, err := e.s.CreateVM(ctx, e.vm("b", nil, 1, 1, 1)); !errors.Is(err, store.ErrLimitExceeded) {
		t.Fatalf("a rolled-back delete freed quota: %v", err)
	}
	if err := e.s.InTx(ctx, func(tx *store.Store) error {
		if err := tx.InTx(ctx, func(sp *store.Store) error {
			if err := sp.DeleteVM(ctx, e.org.ID, v.ID, &e.user, store.DeleteReasonUser); err != nil {
				return err
			}
			return errRollback
		}); !errors.Is(err, errRollback) {
			return err
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.s.CreateVM(ctx, e.vm("b", nil, 1, 1, 1)); !errors.Is(err, store.ErrLimitExceeded) {
		t.Fatalf("a rolled-back savepoint's delete freed quota: %v", err)
	}

	if err := e.s.DeleteVM(ctx, e.org.ID, v.ID, &e.user, store.DeleteReasonUser); err != nil {
		t.Fatal(err)
	}
	b := mustStoppedVM(t, e, "b", nil)
	mustSnapshot(t, e, b.ID, "s", false)

	d := mustCreateDisk(t, e, e.disk("d", nil, 4))
	if err := e.s.DeleteDisk(ctx, e.org.ID, d.ID); err != nil {
		t.Fatal(err)
	}
	mustCreateDisk(t, e, e.disk("d2", nil, 4))

	var deleted []string
	for _, ev := range e.lifecycle(t) {
		if ev.GetType() == usage.InstanceDeleted || ev.GetType() == usage.DiskDeleted {
			l, err := usage.Parse(ev)
			if err != nil {
				t.Fatal(err)
			}
			deleted = append(deleted, ev.GetResource()+" "+l.Resource)
		}
	}
	if want := []string{"org/vms/" + v.ID.String() + " vm/" + v.ID.String(), "org/disks/" + d.ID.String() + " disk/" + d.ID.String()}; !slices.Equal(deleted, want) {
		t.Fatalf("deleted events = %v, want %v", deleted, want)
	}
}

func TestForkReservesTotal(t *testing.T) {
	ctx := t.Context()
	e := newDiskEnv(t)
	vm, _ := vmWithDisk(t, e, "v")
	snap := readySnapshot(t, e, vm.ID, "s")
	before := len(e.quota.Reserves())
	fork, err := e.s.ForkVM(ctx, e.org.ID, snap, "f", false, e.user)
	if err != nil {
		t.Fatal(err)
	}
	if n := len(e.quota.Reserves()) - before; n != 1 {
		t.Fatalf("reserves = %d, want one for the total", n)
	}
	disks, err := e.s.VMDisks(ctx, fork.ID)
	if err != nil || len(disks) != 1 {
		t.Fatalf("fork disks %+v %v", disks, err)
	}
	copyRes := "disk/" + disks[0].DiskID.String()
	want := &quotav1.ReserveRequest{OrgId: e.org.ID.String(), Owner: "user:" + e.user.String(), Resources: []string{"vm/" + fork.ID.String(), copyRes},
		Amounts: &quotav1.Amounts{Vcpu: i32(int32(snap.CPUs)), RamMb: i32(int32(snap.MemoryMB)), DiskGb: i32(int32(snap.DiskGB + 1)), VmCount: i32(1)}}
	if r := lastReserve(t, e); !proto.Equal(r, want) {
		t.Fatalf("reserve = %v, want %v", r, want)
	}
	evs := e.lifecycle(t)
	tail := evs[len(evs)-3:]
	if got := eventTypes(tail); !slices.Equal(got, []string{usage.InstanceCreated, usage.DiskCreated, usage.ReservationsCommitted}) {
		t.Fatalf("fork events = %v", got)
	}
	if l, err := usage.Parse(tail[1]); err != nil || l.Resource != copyRes || l.Amounts.DiskGB != 1 {
		t.Fatalf("disk copy event = %+v %v", l, err)
	}
}

func TestGrowDiskReservesDelta(t *testing.T) {
	e := newVMEnv(t)
	d := mustCreateDisk(t, e, e.disk("d", nil, 2))
	if _, err := e.s.GrowDisk(t.Context(), e.org.ID, d.ID, 5, true); err != nil {
		t.Fatal(err)
	}
	want := &quotav1.ReserveRequest{OrgId: e.org.ID.String(), Owner: "user:" + e.user.String(), Resources: []string{"disk/" + d.ID.String()}, Amounts: &quotav1.Amounts{DiskGb: i32(3)}}
	if r := lastReserve(t, e); !proto.Equal(r, want) {
		t.Fatalf("reserve = %v, want %v", r, want)
	}
	evs := e.lifecycle(t)
	if got := eventTypes(evs); !slices.Equal(got, []string{usage.DiskCreated, usage.ReservationsCommitted, usage.DiskResized, usage.ReservationsCommitted}) {
		t.Fatalf("events = %v", got)
	}
	if l, err := usage.Parse(evs[2]); err != nil || l.Amounts.DiskGB != 5 || evs[2].GetResource() != "org/disks/"+d.ID.String() {
		t.Fatalf("resized = %+v %v, want the absolute size", l, err)
	}
}

func TestSnapshotCountLimit(t *testing.T) {
	ctx := t.Context()
	e := newDiskEnv(t)
	e.setLimits("org", store.Limits{SnapshotCount: ip(1)})
	v := mustStoppedVM(t, e, "v", nil)
	sn := mustSnapshot(t, e, v.ID, "a", false)
	if r := lastReserve(t, e); !slices.Equal(r.GetResources(), []string{"snapshot/" + sn.ID.String(), "vm/" + v.ID.String()}) || r.GetAmounts().GetSnapshotCount() != 1 {
		t.Fatalf("reserve = %v", r)
	}
	if _, err := e.s.CreateSnapshot(ctx, e.org.ID, v.ID, "b", false, e.user); !errors.Is(err, store.ErrLimitExceeded) {
		t.Fatalf("over: %v", err)
	}
	var created *eventsv1.Event
	for _, ev := range e.lifecycle(t) {
		if ev.GetType() == usage.SnapshotCreated {
			created = ev
		}
	}
	if l, err := usage.Parse(created); err != nil || created.GetResource() != "org/vms/"+v.ID.String() ||
		l != (usage.Lifecycle{Resource: "snapshot/" + sn.ID.String(), Parent: "vm/" + v.ID.String(), Owner: "user:" + e.user.String(), Amounts: usage.Amounts{SnapshotCount: 1}}) {
		t.Fatalf("snapshot created = %+v %v", l, err)
	}
	if err := e.s.DeleteSnapshot(ctx, e.org.ID, v.ID, sn.ID); err != nil {
		t.Fatal(err)
	}
	mustSnapshot(t, e, v.ID, "b", false)
}

// A canceled call the hub did not cancel (a reset connection while quota stops) is quota being down too.
func TestQuotaUnavailableCreatesNothing(t *testing.T) {
	e := newVMEnv(t)
	for _, code := range []connect.Code{connect.CodeUnavailable, connect.CodeDeadlineExceeded, connect.CodeInternal, connect.CodeUnknown, connect.CodeCanceled} {
		e.quota.FailReserves(connect.NewError(code, errors.New("down")))
		if _, err := e.s.CreateVM(t.Context(), e.vm("a", nil, 1, 1, 1)); !errors.Is(err, store.ErrQuotaUnavailable) {
			t.Fatalf("create while quota answers %v: %v", code, err)
		}
	}
	e.quota.FailReserves(connect.NewError(connect.CodeInvalidArgument, errors.New("bad")))
	if _, err := e.s.CreateVM(t.Context(), e.vm("a", nil, 1, 1, 1)); err == nil || errors.Is(err, store.ErrQuotaUnavailable) {
		t.Fatalf("invalid argument read as unavailable: %v", err)
	}
	var n int
	if err := e.pool.QueryRow(t.Context(), "SELECT count(*) FROM vms").Scan(&n); err != nil || n != 0 {
		t.Fatalf("vms = %d %v", n, err)
	}
}

func TestFailedSavepointCancelsItsHoldsAtOnce(t *testing.T) {
	ctx := t.Context()
	e := newVMEnv(t)
	errRollback := errors.New("rollback")
	if err := e.s.InTx(ctx, func(tx *store.Store) error {
		if err := tx.InTx(ctx, func(sp *store.Store) error {
			if _, err := sp.CreateVM(ctx, e.vm("a", nil, 1, 1, 1)); err != nil {
				return err
			}
			return errRollback
		}); !errors.Is(err, errRollback) {
			return err
		}
		if n := e.quota.Holds(); n != 0 {
			t.Errorf("holds after the failed savepoint = %d, want 0 before the outer commit", n)
		}
		_, err := tx.CreateVM(ctx, e.vm("b", nil, 1, 1, 1))
		return err
	}); err != nil {
		t.Fatal(err)
	}
	evs := e.lifecycle(t)
	if got := eventTypes(evs); !slices.Equal(got, []string{usage.InstanceCreated, usage.ReservationsCommitted}) {
		t.Fatalf("events = %v", got)
	}
	if ids, err := usage.ParseCommitted(evs[1]); err != nil || len(ids) != 1 {
		t.Fatalf("committed = %v %v, want only the kept hold", ids, err)
	}
}

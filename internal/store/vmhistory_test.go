package store_test

import (
	"context"
	"testing"
	"time"

	"github.com/asterism-dev/observatory/internal/store"
)

func TestDeleteVMWritesHistory(t *testing.T) {
	ctx := context.Background()
	e := newVMEnv(t)
	v := e.vm("a", nil, 2, 1024, 20)
	at := time.Now().Add(time.Hour)
	v.DeleteAt = &at
	vm, err := e.s.CreateVM(ctx, v)
	if err != nil {
		t.Fatal(err)
	}
	if err := e.s.DeleteVM(ctx, e.org.ID, vm.ID, &e.user, store.DeleteReasonUser); err != nil {
		t.Fatal(err)
	}
	h, err := e.s.VMHistory(ctx, e.org.ID)
	if err != nil || len(h) != 1 {
		t.Fatalf("history: %+v %v", h, err)
	}
	r := h[0]
	if r.ID != vm.ID || r.Name != "a" || r.CPUs != 2 || r.MemoryMB != 1024 || r.DiskGB != 20 || *r.UserID != e.user || r.TeamID != nil ||
		*r.CreatedBy != e.user || *r.DeletedBy != e.user || r.Reason != "user" || !r.DeletedAt.After(r.CreatedAt) || *r.NodeID != e.node.ID {
		t.Fatalf("row: %+v", r)
	}
	if exp, _ := e.s.ExpiredVMs(ctx, time.Now().Add(48*time.Hour)); len(exp) != 0 {
		t.Fatalf("deleted VM still expires: %+v", exp)
	}
}

func TestDeleteVMExpiredReason(t *testing.T) {
	ctx := context.Background()
	e := newVMEnv(t)
	vm, _ := e.s.CreateVM(ctx, e.vm("a", &e.team.ID, 1, 512, 10))
	if err := e.s.DeleteVM(ctx, e.org.ID, vm.ID, nil, store.DeleteReasonExpired); err != nil {
		t.Fatal(err)
	}
	h, _ := e.s.VMHistory(ctx, e.org.ID)
	if len(h) != 1 || h[0].DeletedBy != nil || h[0].Reason != "expired" || *h[0].TeamID != e.team.ID {
		t.Fatalf("history: %+v", h)
	}
}

func TestBuilderVMNoHistory(t *testing.T) {
	ctx := context.Background()
	e := newVMEnv(t)
	v := e.vm("b", nil, 1, 512, 10)
	v.Kind = store.VMKindBuilder
	vm, _ := e.s.CreateVM(ctx, v)
	if err := e.s.DeleteVM(ctx, e.org.ID, vm.ID, nil, store.DeleteReasonUser); err != nil {
		t.Fatal(err)
	}
	if h, _ := e.s.VMHistory(ctx, e.org.ID); len(h) != 0 {
		t.Fatalf("builder history: %+v", h)
	}
}

func TestExpiredVMs(t *testing.T) {
	ctx := context.Background()
	e := newVMEnv(t)
	now := time.Now()
	past, future := now.Add(-time.Minute), now.Add(time.Hour)
	mk := func(name string, at *time.Time, kind string) store.VM {
		v := e.vm(name, nil, 1, 512, 10)
		v.DeleteAt, v.Kind = at, kind
		vm, err := e.s.CreateVM(ctx, v)
		if err != nil {
			t.Fatal(err)
		}
		return vm
	}
	want := mk("past", &past, "")
	mk("future", &future, "")
	mk("never", nil, "")
	mk("builder", &past, store.VMKindBuilder)
	got, err := e.s.ExpiredVMs(ctx, now)
	if err != nil || len(got) != 1 || got[0].ID != want.ID {
		t.Fatalf("expired: %+v %v", got, err)
	}
}

func TestSetDeleteAt(t *testing.T) {
	ctx := context.Background()
	e := newVMEnv(t)
	vm, _ := e.s.CreateVM(ctx, e.vm("a", nil, 1, 512, 10))
	at := time.Now().Add(time.Hour).Truncate(time.Microsecond)
	got, err := e.s.SetDeleteAt(ctx, e.org.ID, vm.ID, &at)
	if err != nil || got.DeleteAt == nil || !got.DeleteAt.Equal(at) || got.Generation != vm.Generation {
		t.Fatalf("set: %+v %v", got, err)
	}
	got, err = e.s.SetDeleteAt(ctx, e.org.ID, vm.ID, nil)
	if err != nil || got.DeleteAt != nil {
		t.Fatalf("clear: %+v %v", got, err)
	}
}

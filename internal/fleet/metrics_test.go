package fleet_test

import (
	"context"
	"testing"
	"time"

	"github.com/asterism-dev/observatory/internal/proto"
	"github.com/asterism-dev/observatory/internal/store"
)

func TestNoteMetricsIgnoresForeignVM(t *testing.T) {
	e := newEnv(t)
	a, _ := e.node(e.org.ID, "a")
	b, _ := e.node(e.org.ID, "b")
	v := e.vm(e.org.ID, b.ID, "dev", &e.owner, nil)
	e.f.NoteMetrics(context.Background(), a.ID, []proto.VMMetrics{{ID: v.ID.String(), CPUPercent: 10}})
	if got := e.f.Metrics(v.ID); len(got) != 0 {
		t.Fatalf("samples = %+v", got)
	}
}

func TestNoteMetricsClamps(t *testing.T) {
	e := newEnv(t)
	n, _ := e.node(e.org.ID, "a")
	v, err := e.s.CreateVM(context.Background(), store.VM{OrgID: e.org.ID, NodeID: n.ID, UserID: &e.owner, Name: "big", CPUs: 2, MemoryMB: 4096, DiskGB: 5, CreatedBy: e.owner})
	if err != nil {
		t.Fatal(err)
	}
	id := v.ID.String()
	e.f.NoteMetrics(context.Background(), n.ID, []proto.VMMetrics{
		{ID: id, CPUPercent: 250, MemTotalMB: 9000, MemUsedMB: 9000, DiskUsedMB: 9000},
		{ID: id, CPUPercent: -5, MemTotalMB: 4096, MemUsedMB: -3, DiskUsedMB: -1},
		{ID: "not-a-uuid"},
	})
	got := e.f.Metrics(v.ID)
	if len(got) != 2 || got[0].CPUPercent != 100 || got[0].MemUsedMB != 4096 || got[0].MemTotalMB != 4096 || got[0].DiskUsedMB != 5120 || got[1].CPUPercent != 0 || got[1].MemUsedMB != 0 || got[1].DiskUsedMB != 0 {
		t.Fatalf("samples = %+v", got)
	}
	if got[0].At.IsZero() || time.Since(got[0].At) > time.Minute {
		t.Fatalf("at = %v", got[0].At)
	}
}

func TestMetricsRingKeeps60(t *testing.T) {
	e := newEnv(t)
	n, _ := e.node(e.org.ID, "a")
	v := e.vm(e.org.ID, n.ID, "dev", &e.owner, nil)
	for i := 1; i <= 70; i++ {
		e.f.NoteMetrics(context.Background(), n.ID, []proto.VMMetrics{{ID: v.ID.String(), CPUPercent: float64(i) / 2}})
	}
	got := e.f.Metrics(v.ID)
	if len(got) != 60 || got[0].CPUPercent != 5.5 || got[59].CPUPercent != 35 {
		t.Fatalf("len %d first %v last %v", len(got), got[0].CPUPercent, got[len(got)-1].CPUPercent)
	}
	e.f.DropMetrics(v.ID)
	if len(e.f.Metrics(v.ID)) != 0 {
		t.Fatal("ring kept after drop")
	}
}

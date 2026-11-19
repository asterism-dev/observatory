package fleet

import (
	"context"
	"log/slog"
	"time"

	"github.com/google/uuid"

	"github.com/asterism-dev/observatory/internal/proto"
	"github.com/asterism-dev/observatory/internal/store"
)

const metricsKept = 60

type MetricSample struct {
	At                    time.Time
	CPUPercent            float64
	MemTotalMB, MemUsedMB int
	DiskUsedMB            int
}

// Metrics returns the VM's samples, oldest first; the slice is a copy.
func (f *Fleet) Metrics(vm uuid.UUID) []MetricSample {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]MetricSample(nil), f.metrics[vm]...)
}

func (f *Fleet) DropMetrics(vm uuid.UUID) {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.metrics, vm)
}

// NoteMetrics keeps samples only for VMs placed on node and clamps them to the VM's size, as nodes are untrusted.
func (f *Fleet) NoteMetrics(ctx context.Context, node uuid.UUID, ms []proto.VMMetrics) {
	if len(ms) == 0 {
		return
	}
	vms, err := f.store.VMsOnNode(ctx, node)
	if err != nil {
		slog.Error("metrics: vms on node", "node", node, "err", err)
		return
	}
	sizes := make(map[uuid.UUID]store.VM, len(vms))
	for _, v := range vms {
		sizes[v.ID] = v
	}
	now := time.Now()
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, m := range ms {
		id, err := uuid.Parse(m.ID)
		v, ok := sizes[id]
		if err != nil || !ok {
			continue
		}
		limit := v.MemoryMB
		s := MetricSample{At: now, CPUPercent: min(max(m.CPUPercent, 0), 100),
			MemTotalMB: min(max(m.MemTotalMB, 0), limit), MemUsedMB: min(max(m.MemUsedMB, 0), limit),
			DiskUsedMB: min(max(m.DiskUsedMB, 0), v.DiskGB*1024)}
		f.metrics[id] = append(f.metrics[id], s)
		if n := len(f.metrics[id]) - metricsKept; n > 0 {
			f.metrics[id] = f.metrics[id][n:]
		}
	}
}

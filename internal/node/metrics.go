package node

import (
	"context"
	"errors"
	"log"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/asterism-dev/observatory/internal/proto"
)

var metricsEvery = 10 * time.Second

// USER_HZ is 100 on every Linux architecture we run on.
const clockTicks = 100

// parseProcStatTicks sums utime and stime; the command name may hold spaces, so fields count from the last ")".
func parseProcStatTicks(stat string) (uint64, error) {
	i := strings.LastIndexByte(stat, ')')
	if i < 0 {
		return 0, errors.New("malformed stat")
	}
	f := strings.Fields(stat[i+1:])
	// f[0] is field 3 (state), so utime and stime (fields 14 and 15) are f[11] and f[12].
	if len(f) < 13 {
		return 0, errors.New("short stat")
	}
	u, err := strconv.ParseUint(f[11], 10, 64)
	if err != nil {
		return 0, err
	}
	s, err := strconv.ParseUint(f[12], 10, 64)
	if err != nil {
		return 0, err
	}
	return u + s, nil
}

// ProcTicks returns the CPU ticks of a host process.
func ProcTicks(pid int) (uint64, error) {
	b, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		return 0, err
	}
	return parseProcStatTicks(string(b))
}

type cpuMark struct {
	inst  Instance
	ticks uint64
}

func (a *Agent) metricsLoop(ctx context.Context) {
	t := time.NewTicker(metricsEvery)
	defer t.Stop()
	marks := map[string]cpuMark{}
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			a.sendMetrics(marks)
		}
	}
}

// sendMetrics skips a VM's first sight, which only sets the CPU baseline.
func (a *Agent) sendMetrics(marks map[string]cpuMark) {
	type runner struct {
		id   string
		cpus int
		inst Instance
	}
	var running []runner
	a.mu.Lock()
	for id, e := range a.vms {
		if e.status.State == proto.StateRunning && e.inst != nil && !e.paused {
			running = append(running, runner{id, max(e.file.Spec.CPUs, 1), e.inst})
		}
	}
	send := a.send
	a.mu.Unlock()
	seen := map[string]bool{}
	var out []proto.VMMetrics
	for _, r := range running {
		seen[r.id] = true
		ticks, err := r.inst.CPUTicks()
		if err != nil {
			delete(marks, r.id)
			continue
		}
		prev, ok := marks[r.id]
		marks[r.id] = cpuMark{r.inst, ticks}
		if !ok || prev.inst != r.inst || ticks < prev.ticks {
			continue
		}
		cpu := float64(ticks-prev.ticks) / (clockTicks * metricsEvery.Seconds() * float64(r.cpus)) * 100
		m := proto.VMMetrics{ID: r.id, CPUPercent: cpu}
		if total, used, err := r.inst.Memory(); err == nil {
			m.MemTotalMB, m.MemUsedMB = total, used
		}
		out = append(out, m)
	}
	for id := range marks {
		if !seen[id] {
			delete(marks, id)
		}
	}
	if len(out) > 0 && send != nil {
		if err := send(proto.Message{Type: "metrics", Metrics: out}); err != nil {
			log.Printf("metrics: %v", err)
		}
	}
}

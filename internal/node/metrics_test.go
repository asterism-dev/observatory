package node

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/asterism-dev/observatory/internal/proto"
)

func TestParseProcStatTicks(t *testing.T) {
	got, err := parseProcStatTicks("42 (fire cracker) S 1 42 42 0 -1 4194560 100 0 0 0 100 50 0 0 20 0 5 0 1000 1 2")
	if err != nil || got != 150 {
		t.Fatalf("ticks = %d, %v", got, err)
	}
	if _, err := parseProcStatTicks("42 (x) S 1"); err == nil {
		t.Fatal("short stat accepted")
	}
}

func TestMetricsLoopSendsRunningOnly(t *testing.T) {
	env := startEnv(t, func(a *Agent, _ *fakeProvider) { a.metricsEvery = time.Second })
	env.hub.mu.Lock()
	env.hub.cpus = 2
	env.hub.mu.Unlock()
	running := env.hub.create("run")
	stopped := env.hub.create("stop")
	eventually(t, "both running", func() bool {
		a, _ := env.hub.vm(running.ID)
		b, _ := env.hub.vm(stopped.ID)
		return a.State == proto.StateRunning && b.State == proto.StateRunning
	})
	for _, n := range []string{"run", "stop"} {
		env.provider.instance(n).tickStep.Store(200)
	}
	env.hub.setPower(stopped.ID, proto.PowerStopped, proto.StopHard)
	eventually(t, "stopped", func() bool { v, _ := env.hub.vm(stopped.ID); return v.State == proto.StateStopped })

	eventually(t, "metrics", func() bool { env.hub.mu.Lock(); defer env.hub.mu.Unlock(); return len(env.hub.metrics) > 0 })
	env.hub.mu.Lock()
	defer env.hub.mu.Unlock()
	m := env.hub.metrics[0]
	if len(m.Metrics) != 1 || m.Metrics[0].ID != running.ID || m.Metrics[0].CPUPercent != 100 || m.Metrics[0].MemTotalMB != 1024 || m.Metrics[0].MemUsedMB != 256 {
		t.Fatalf("metrics = %+v", m.Metrics)
	}
}

func TestAllocatedMBCountsBlocksNotSize(t *testing.T) {
	p := filepath.Join(t.TempDir(), "root.img")
	f, err := os.Create(p)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.Write(make([]byte, 3<<20)); err != nil {
		t.Fatal(err)
	}
	if err := f.Truncate(1 << 30); err != nil {
		t.Fatal(err)
	}
	f.Close()
	if got := allocatedMB(p); got < 3 || got > 100 {
		t.Fatalf("allocated = %d MB", got)
	}
	if got := allocatedMB(p + ".missing"); got != 0 {
		t.Fatalf("missing = %d", got)
	}
}

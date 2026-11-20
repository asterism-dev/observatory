package guestinit

import (
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	metricspb "go.opentelemetry.io/proto/otlp/metrics/v1"
)

func writeSysProc(t *testing.T, dir, stat string) {
	t.Helper()
	files := map[string]string{
		"stat":      stat,
		"meminfo":   "MemTotal: 1000 kB\nMemFree: 100 kB\nBuffers: 50 kB\nCached: 200 kB\nSReclaimable: 50 kB\n",
		"loadavg":   "0.50 0.25 0.10 1/100 42\n",
		"1/net/dev": "Inter-|   Receive\n face |bytes\n    lo: 9 0 0 0 0 0 0 0 9 0 0 0 0 0 0 0\n  eth0: 100 1 0 0 0 0 0 0 200 2 0 0 0 0 0 0\n",
	}
	for name, body := range files {
		p := filepath.Join(dir, name)
		os.MkdirAll(filepath.Dir(p), 0o755)
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func attrString(attrs []*commonpb.KeyValue) string {
	var kv []string
	for _, a := range attrs {
		kv = append(kv, a.Key+"="+a.Value.GetStringValue())
	}
	slices.Sort(kv)
	return strings.Join(kv, ",")
}

func TestUsageMetrics(t *testing.T) {
	proc, mnt := t.TempDir(), t.TempDir()
	writeSysProc(t, proc, "cpu  1 2 3 4\ncpu0 100 0 50 1000 0 0 0 0\n")
	start := time.Unix(1000, 0)
	// A second directory on the same filesystem is reported once, as on the node.
	u := newUsage(proc, []string{mnt, t.TempDir()}, start)
	rm := u.collect(time.Unix(1010, 0))

	if got := attrString(rm.GetResource().GetAttributes()); got != "service.name=observatory-init" {
		t.Fatalf("resource = %s", got)
	}
	// Same names, units and attribute keys as the node's host metrics (internal/node/hostmetrics.go).
	want := map[string]struct {
		unit, kind string
		monotonic  bool
		attrs      []string
	}{
		"system.cpu.time":             {"s", "sum", true, []string{"cpu", "state"}},
		"system.memory.usage":         {"By", "sum", false, []string{"state"}},
		"system.filesystem.usage":     {"By", "sum", false, []string{"mountpoint"}},
		"system.filesystem.limit":     {"By", "sum", false, []string{"mountpoint"}},
		"system.network.io":           {"By", "sum", true, []string{"device", "direction"}},
		"system.cpu.load_average.1m":  {"{thread}", "gauge", false, nil},
		"system.cpu.load_average.5m":  {"{thread}", "gauge", false, nil},
		"system.cpu.load_average.15m": {"{thread}", "gauge", false, nil},
	}
	metrics := map[string]*metricspb.Metric{}
	for _, m := range rm.ScopeMetrics[0].Metrics {
		metrics[m.Name] = m
	}
	if len(metrics) != len(want) {
		t.Fatalf("metrics = %v", slices.Collect(maps.Keys(metrics)))
	}
	for name, w := range want {
		m := metrics[name]
		if m == nil || m.Unit != w.unit {
			t.Fatalf("%s = %v", name, m)
		}
		var attrs [][]*commonpb.KeyValue
		switch w.kind {
		case "sum":
			s := m.GetSum()
			if s == nil || s.IsMonotonic != w.monotonic || s.AggregationTemporality != metricspb.AggregationTemporality_AGGREGATION_TEMPORALITY_CUMULATIVE {
				t.Fatalf("%s sum = %v", name, s)
			}
			for _, p := range s.DataPoints {
				if p.StartTimeUnixNano != uint64(start.UnixNano()) {
					t.Fatalf("%s start = %d", name, p.StartTimeUnixNano)
				}
				attrs = append(attrs, p.Attributes)
			}
		case "gauge":
			if m.GetGauge() == nil || len(m.GetGauge().DataPoints) != 1 {
				t.Fatalf("%s gauge = %v", name, m)
			}
			attrs = append(attrs, m.GetGauge().DataPoints[0].Attributes)
		}
		for _, a := range attrs {
			var keys []string
			for _, kv := range a {
				keys = append(keys, kv.Key)
			}
			slices.Sort(keys)
			if !slices.Equal(keys, w.attrs) {
				t.Fatalf("%s attrs = %v, want %v", name, keys, w.attrs)
			}
		}
	}

	var net []string
	for _, p := range metrics["system.network.io"].GetSum().DataPoints {
		net = append(net, attrString(p.Attributes)+"="+strconv.FormatInt(p.GetAsInt(), 10))
	}
	slices.Sort(net)
	if want := []string{"device=eth0,direction=receive=100", "device=eth0,direction=transmit=200"}; !slices.Equal(net, want) {
		t.Fatalf("network = %v, want %v", net, want)
	}
	if fs := metrics["system.filesystem.limit"].GetSum().DataPoints; len(fs) != 1 || attrString(fs[0].Attributes) != "mountpoint="+mnt || fs[0].GetAsInt() <= 0 {
		t.Fatalf("filesystem limit = %v", fs)
	}
	var mem []string
	for _, p := range metrics["system.memory.usage"].GetSum().DataPoints {
		mem = append(mem, attrString(p.Attributes)+"="+strconv.FormatInt(p.GetAsInt(), 10))
	}
	slices.Sort(mem)
	if want := []string{"state=cached=256000", "state=free=102400", "state=used=614400"}; !slices.Equal(mem, want) {
		t.Fatalf("memory = %v", mem)
	}

	cpuUser := func(rm *metricspb.ResourceMetrics) float64 {
		for _, m := range rm.ScopeMetrics[0].Metrics {
			if m.Name != "system.cpu.time" {
				continue
			}
			for _, p := range m.GetSum().DataPoints {
				if attrString(p.Attributes) == "cpu=cpu0,state=user" {
					return p.GetAsDouble()
				}
			}
		}
		return -1
	}
	if v := cpuUser(rm); v != 1 {
		t.Fatalf("cpu0 user = %v, want 1", v)
	}
	writeSysProc(t, proc, "cpu0 300 0 50 1000 0 0 0 0\n")
	if v := cpuUser(u.collect(time.Unix(1020, 0))); v != 3 {
		t.Fatalf("cpu0 user after = %v, want the cumulative 3", v)
	}
}

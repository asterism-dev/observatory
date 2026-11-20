package node

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"go.opentelemetry.io/otel/attribute"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
)

func TestRegisterHostMetrics(t *testing.T) {
	dir := t.TempDir()
	write := func(name, body string) {
		if err := os.MkdirAll(filepath.Dir(filepath.Join(dir, name)), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("stat", statFixture)
	write("meminfo", meminfoFixture)
	write("loadavg", "0.52 0.58 0.59 2/467 12345\n")
	write("net/dev", netDevFixture)
	prevDir, prevEnabled := procDir, hostMetricsEnabled
	procDir, hostMetricsEnabled = dir, true
	t.Cleanup(func() { procDir, hostMetricsEnabled = prevDir, prevEnabled })

	reader := sdkmetric.NewManualReader()
	meter := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader)).Meter("test")
	a := &Agent{}
	mounts := []string{dir, filepath.Join(dir, "net")}
	if err := a.RegisterHostMetrics(meter, mounts); err != nil {
		t.Fatal(err)
	}
	var rm metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &rm); err != nil {
		t.Fatal(err)
	}

	points := map[string][]attribute.Set{}
	units := map[string]string{}
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			units[m.Name] = m.Unit
			switch d := m.Data.(type) {
			case metricdata.Sum[float64]:
				for _, p := range d.DataPoints {
					points[m.Name] = append(points[m.Name], p.Attributes)
				}
			case metricdata.Sum[int64]:
				for _, p := range d.DataPoints {
					points[m.Name] = append(points[m.Name], p.Attributes)
				}
			case metricdata.Gauge[float64]:
				for _, p := range d.DataPoints {
					points[m.Name] = append(points[m.Name], p.Attributes)
				}
			case metricdata.Gauge[int64]:
				for _, p := range d.DataPoints {
					points[m.Name] = append(points[m.Name], p.Attributes)
				}
			}
		}
	}

	has := func(name string, kv ...attribute.KeyValue) bool {
		for _, s := range points[name] {
			want := attribute.NewSet(kv...)
			if s.Equals(&want) {
				return true
			}
		}
		return false
	}
	str := attribute.String
	for name, want := range map[string]bool{
		"system.cpu.time": has("system.cpu.time", str("cpu", "cpu0"), str("state", "user")),
		"memory used":     has("system.memory.usage", str("state", "used")),
		"memory cached":   has("system.memory.usage", str("state", "cached")),
		"memory free":     has("system.memory.usage", str("state", "free")),
		"paging used":     has("system.paging.usage", str("state", "used")),
		"paging free":     has("system.paging.usage", str("state", "free")),
		"net rx":          has("system.network.io", str("device", "eth0"), str("direction", "receive")),
		"net tx":          has("system.network.io", str("device", "eth0"), str("direction", "transmit")),
		"fs usage":        has("system.filesystem.usage", str("mountpoint", dir)),
		"fs limit":        has("system.filesystem.limit", str("mountpoint", dir)),
		"load 1m":         len(points["system.cpu.load_average.1m"]) == 1,
		"load 5m":         len(points["system.cpu.load_average.5m"]) == 1,
		"load 15m":        len(points["system.cpu.load_average.15m"]) == 1,
		"vms running":     len(points["observatory.node.vms.running"]) == 1,
	} {
		if !want {
			t.Errorf("missing %s; points %v", name, points)
		}
	}
	if len(points["system.filesystem.usage"]) != 1 {
		t.Errorf("same-filesystem mounts must be deduplicated: %v", points["system.filesystem.usage"])
	}
	if has("system.network.io", str("device", "lo"), str("direction", "receive")) {
		t.Error("lo must be skipped")
	}
	if units["system.cpu.time"] != "s" || units["system.memory.usage"] != "By" {
		t.Errorf("units %v", units)
	}
}

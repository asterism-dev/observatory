package node

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"slices"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
	"golang.org/x/sys/unix"
)

var (
	procDir            = "/proc"
	hostMetricsEnabled = runtime.GOOS == "linux"
)

func readProcFile[T any](name string, parse func(*os.File) (T, error)) (T, bool) {
	f, err := os.Open(filepath.Join(procDir, name))
	if err != nil {
		var zero T
		return zero, false
	}
	defer f.Close()
	v, err := parse(f)
	return v, err == nil
}

// uniqueMounts drops mounts that share a device with an earlier one, so a shared filesystem is reported once.
func uniqueMounts(mounts []string) []string {
	var out []string
	var devs []uint64
	for _, m := range mounts {
		var st unix.Stat_t
		if unix.Stat(m, &st) != nil {
			continue
		}
		if dev := uint64(st.Dev); !slices.Contains(devs, dev) {
			devs = append(devs, dev)
			out = append(out, m)
		}
	}
	return out
}

func (a *Agent) RegisterHostMetrics(m metric.Meter, mounts []string) error {
	_, err := m.Int64ObservableGauge("observatory.node.vms.running", metric.WithUnit("{vm}"),
		metric.WithInt64Callback(func(_ context.Context, o metric.Int64Observer) error {
			o.Observe(int64(a.runningVMs()))
			return nil
		}))
	if err != nil || !hostMetricsEnabled {
		return err
	}

	cpu, err := m.Float64ObservableCounter("system.cpu.time", metric.WithUnit("s"))
	if err != nil {
		return err
	}
	load := [3]metric.Float64ObservableGauge{}
	for i, name := range []string{"1m", "5m", "15m"} {
		if load[i], err = m.Float64ObservableGauge("system.cpu.load_average."+name, metric.WithUnit("{thread}")); err != nil {
			return err
		}
	}
	mem, err := m.Int64ObservableUpDownCounter("system.memory.usage", metric.WithUnit("By"))
	if err != nil {
		return err
	}
	paging, err := m.Int64ObservableUpDownCounter("system.paging.usage", metric.WithUnit("By"))
	if err != nil {
		return err
	}
	fsUsage, err := m.Int64ObservableUpDownCounter("system.filesystem.usage", metric.WithUnit("By"))
	if err != nil {
		return err
	}
	fsLimit, err := m.Int64ObservableUpDownCounter("system.filesystem.limit", metric.WithUnit("By"))
	if err != nil {
		return err
	}
	netIO, err := m.Int64ObservableCounter("system.network.io", metric.WithUnit("By"))
	if err != nil {
		return err
	}

	_, err = m.RegisterCallback(func(_ context.Context, o metric.Observer) error {
		if cpus, ok := readProcFile("stat", func(f *os.File) (map[string]map[string]float64, error) { return parseCPUTimes(f) }); ok {
			for c, states := range cpus {
				for s, secs := range states {
					o.ObserveFloat64(cpu, secs, metric.WithAttributes(attribute.String("cpu", c), attribute.String("state", s)))
				}
			}
		}
		if l, ok := readProcFile("loadavg", func(f *os.File) ([3]float64, error) {
			l1, l5, l15, err := parseLoadAvg(f)
			return [3]float64{l1, l5, l15}, err
		}); ok {
			for i, g := range load {
				o.ObserveFloat64(g, l[i])
			}
		}
		if v, ok := readProcFile("meminfo", func(f *os.File) ([5]int64, error) {
			used, cached, free, swapUsed, swapFree, err := parseMemInfo(f)
			return [5]int64{used, cached, free, swapUsed, swapFree}, err
		}); ok {
			state := func(s string) metric.MeasurementOption { return metric.WithAttributes(attribute.String("state", s)) }
			o.ObserveInt64(mem, v[0], state("used"))
			o.ObserveInt64(mem, v[1], state("cached"))
			o.ObserveInt64(mem, v[2], state("free"))
			o.ObserveInt64(paging, v[3], state("used"))
			o.ObserveInt64(paging, v[4], state("free"))
		}
		if devs, ok := readProcFile("net/dev", func(f *os.File) (map[string][2]int64, error) { return parseNetDev(f) }); ok {
			for d, b := range devs {
				o.ObserveInt64(netIO, b[0], metric.WithAttributes(attribute.String("device", d), attribute.String("direction", "receive")))
				o.ObserveInt64(netIO, b[1], metric.WithAttributes(attribute.String("device", d), attribute.String("direction", "transmit")))
			}
		}
		for _, mp := range uniqueMounts(mounts) {
			used, total, err := filesystemUsage(mp)
			if err != nil {
				continue
			}
			at := metric.WithAttributes(attribute.String("mountpoint", mp))
			o.ObserveInt64(fsUsage, used, at)
			o.ObserveInt64(fsLimit, total, at)
		}
		return nil
	}, cpu, load[0], load[1], load[2], mem, paging, fsUsage, fsLimit, netIO)
	return err
}

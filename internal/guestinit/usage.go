package guestinit

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"time"

	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	metricspb "go.opentelemetry.io/proto/otlp/metrics/v1"
	"golang.org/x/sys/unix"

	"github.com/asterism-dev/observatory/internal/sysstat"
)

const usageInterval = 10 * time.Second

// usage measures the guest the way the node measures itself (internal/node/hostmetrics.go): same names, units and attributes, so queries serve both.
type usage struct {
	proc   string
	mounts []string
	start  uint64
}

func newUsage(proc string, mounts []string, start time.Time) *usage {
	return &usage{proc: proc, mounts: mounts, start: uint64(start.UnixNano())}
}

// runUsage sends the guest's usage on stream usage every usageInterval until ctx ends.
func runUsage(ctx context.Context, sink *Sink, u *usage) {
	tick := time.NewTicker(usageInterval)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-tick.C:
			sink.Metrics("usage", []*metricspb.ResourceMetrics{u.collect(now)})
		}
	}
}

func attr(k, v string) *commonpb.KeyValue { return &commonpb.KeyValue{Key: k, Value: stringValue(v)} }

func (u *usage) open(name string) (*os.File, bool) {
	f, err := os.Open(filepath.Join(u.proc, name))
	return f, err == nil
}

// collect reads what it can; a file missing (the image has not mounted its /proc yet) leaves its metrics out.
func (u *usage) collect(now time.Time) *metricspb.ResourceMetrics {
	ts := uint64(now.UnixNano())
	var ms []*metricspb.Metric
	sum := func(name, unit string, monotonic bool, pts []*metricspb.NumberDataPoint) {
		if len(pts) > 0 {
			ms = append(ms, &metricspb.Metric{Name: name, Unit: unit, Data: &metricspb.Metric_Sum{Sum: &metricspb.Sum{
				DataPoints: pts, IsMonotonic: monotonic, AggregationTemporality: metricspb.AggregationTemporality_AGGREGATION_TEMPORALITY_CUMULATIVE}}})
		}
	}
	intPt := func(v int64, attrs ...*commonpb.KeyValue) *metricspb.NumberDataPoint {
		return &metricspb.NumberDataPoint{StartTimeUnixNano: u.start, TimeUnixNano: ts, Value: &metricspb.NumberDataPoint_AsInt{AsInt: v}, Attributes: attrs}
	}

	if f, ok := u.open("stat"); ok {
		cpus, err := sysstat.CPUTimes(f)
		f.Close()
		if err == nil {
			var pts []*metricspb.NumberDataPoint
			for c, states := range cpus {
				for s, secs := range states {
					pts = append(pts, &metricspb.NumberDataPoint{StartTimeUnixNano: u.start, TimeUnixNano: ts, Value: &metricspb.NumberDataPoint_AsDouble{AsDouble: secs},
						Attributes: []*commonpb.KeyValue{attr("cpu", c), attr("state", s)}})
				}
			}
			sum("system.cpu.time", "s", true, pts)
		}
	}
	if f, ok := u.open("loadavg"); ok {
		l1, l5, l15, err := sysstat.LoadAvg(f)
		f.Close()
		if err == nil {
			for i, v := range []float64{l1, l5, l15} {
				ms = append(ms, &metricspb.Metric{Name: "system.cpu.load_average." + [...]string{"1m", "5m", "15m"}[i], Unit: "{thread}",
					Data: &metricspb.Metric_Gauge{Gauge: &metricspb.Gauge{DataPoints: []*metricspb.NumberDataPoint{{TimeUnixNano: ts, Value: &metricspb.NumberDataPoint_AsDouble{AsDouble: v}}}}}})
			}
		}
	}
	if f, ok := u.open("meminfo"); ok {
		m, err := sysstat.MemInfo(f)
		f.Close()
		if err == nil {
			sum("system.memory.usage", "By", false, []*metricspb.NumberDataPoint{
				intPt(m.Used, attr("state", "used")), intPt(m.Cached, attr("state", "cached")), intPt(m.Free, attr("state", "free"))})
		}
	}
	// /proc/net is self/net, absent for the init outside the image's PID namespace; the image's PID 1 shares the init's network.
	if f, ok := u.open("1/net/dev"); ok {
		devs, err := sysstat.NetDev(f, nil)
		f.Close()
		if err == nil {
			var pts []*metricspb.NumberDataPoint
			for d, b := range devs {
				pts = append(pts, intPt(b[0], attr("device", d), attr("direction", "receive")), intPt(b[1], attr("device", d), attr("direction", "transmit")))
			}
			sum("system.network.io", "By", true, pts)
		}
	}
	var used, limit []*metricspb.NumberDataPoint
	var devs []uint64
	for _, mp := range u.mounts {
		// A filesystem mounted twice is reported once, as the node does.
		var st unix.Stat_t
		if unix.Stat(mp, &st) != nil || slices.Contains(devs, uint64(st.Dev)) {
			continue
		}
		devs = append(devs, uint64(st.Dev))
		if fsUsed, total, err := sysstat.FilesystemUsage(mp); err == nil {
			used = append(used, intPt(fsUsed, attr("mountpoint", mp)))
			limit = append(limit, intPt(total, attr("mountpoint", mp)))
		}
	}
	sum("system.filesystem.usage", "By", false, used)
	sum("system.filesystem.limit", "By", false, limit)

	return &metricspb.ResourceMetrics{Resource: initResource, ScopeMetrics: []*metricspb.ScopeMetrics{{Metrics: ms}}}
}

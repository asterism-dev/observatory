package vmtelemetry

import (
	"compress/gzip"
	"errors"
	"hash/maphash"
	"io"
	"net"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	collogspb "go.opentelemetry.io/proto/otlp/collector/logs/v1"
	colmetricspb "go.opentelemetry.io/proto/otlp/collector/metrics/v1"
	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	logspb "go.opentelemetry.io/proto/otlp/logs/v1"
	metricspb "go.opentelemetry.io/proto/otlp/metrics/v1"
	"golang.org/x/net/netutil"
	gproto "google.golang.org/protobuf/proto"
)

const (
	maxBody      = 1 << 20
	logRate      = 100
	logBurst     = 500
	maxSeries    = 2000
	seriesWindow = time.Hour
	// maxConns bounds what one guest can make the node buffer at once.
	maxConns = 4
)

// guestStreams are the stream classes a guest may send per signal; console and egress only originate on the node.
var guestStreams = map[string][]string{"logs": {"service", "system", "app"}, "metrics": {"usage", "app"}}

var errTooLarge = errors.New("body too large")

// Sink enforces the per-VM limits on telemetry from guests and node-origin streams before handing it to the Forwarder.
type Sink struct {
	fw      *Forwarder
	dropped func(vmID uuid.UUID, kind string, n int64)
	seed    maphash.Seed

	mu  sync.Mutex
	vms map[uuid.UUID]*vmLimits
}

type vmLimits struct {
	tokens      float64
	filled      time.Time
	windowStart time.Time
	series      map[uint64]struct{} // app series seen in the current window
}

// NewSink forwards through fw and reports every drop to dropped, which may be nil.
func NewSink(fw *Forwarder, dropped func(vmID uuid.UUID, kind string, n int64)) *Sink {
	if dropped == nil {
		dropped = func(uuid.UUID, string, int64) {}
	}
	return &Sink{fw: fw, dropped: dropped, seed: maphash.MakeSeed(), vms: map[uuid.UUID]*vmLimits{}}
}

// Serve answers OTLP/HTTP requests on l, the listener of vmID only, until l is closed.
func (s *Sink) Serve(l net.Listener, vmID uuid.UUID) {
	srv := &http.Server{
		Handler:           http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { s.handle(w, r, vmID) }),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       time.Minute,
		IdleTimeout:       2 * time.Minute,
		MaxHeaderBytes:    16 << 10,
	}
	srv.Serve(netutil.LimitListener(l, maxConns))
}

// Emit forwards node-origin log records of vmID under the VM's log limit.
func (s *Sink) Emit(vmID uuid.UUID, stream string, records []*logspb.LogRecord) {
	req := &collogspb.ExportLogsServiceRequest{ResourceLogs: []*logspb.ResourceLogs{{ScopeLogs: []*logspb.ScopeLogs{{LogRecords: records}}}}}
	if out := s.admitLogs(vmID, req); out != nil {
		s.fw.Send(vmID, "logs", stream, out)
	}
}

// Forget drops vmID's limit state once the VM has exited.
func (s *Sink) Forget(vmID uuid.UUID) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.vms, vmID)
}

func (s *Sink) handle(w http.ResponseWriter, r *http.Request, vmID uuid.UUID) {
	signal, _ := strings.CutPrefix(r.URL.Path, "/v1/")
	streams, ok := guestStreams[signal]
	if !ok || r.Method != http.MethodPost {
		http.NotFound(w, r)
		return
	}
	stream := r.Header.Get("X-Observatory-Stream")
	if !slices.Contains(streams, stream) {
		http.Error(w, "invalid X-Observatory-Stream", http.StatusBadRequest)
		return
	}
	body, err := readBody(w, r)
	if errors.Is(err, errTooLarge) {
		s.dropped(vmID, "body", 1)
		http.Error(w, "body too large", http.StatusRequestEntityTooLarge)
		return
	}
	var out []byte
	if err == nil && signal == "logs" {
		var req collogspb.ExportLogsServiceRequest
		if err = gproto.Unmarshal(body, &req); err == nil {
			out = s.admitLogs(vmID, &req)
		}
	} else if err == nil {
		out, err = s.admitMetrics(vmID, stream, body)
	}
	if err != nil {
		http.Error(w, "invalid OTLP body", http.StatusBadRequest)
		return
	}
	if out != nil {
		s.fw.Send(vmID, signal, stream, out)
	}
	// An empty body is the protobuf encoding of an empty Export*ServiceResponse.
	w.Header().Set("Content-Type", "application/x-protobuf")
}

// readBody applies the body limit to the request and, for gzip, again to the decompressed body.
func readBody(w http.ResponseWriter, r *http.Request) ([]byte, error) {
	var src io.Reader = http.MaxBytesReader(w, r.Body, maxBody)
	switch r.Header.Get("Content-Encoding") {
	case "", "identity":
	case "gzip":
		zr, err := gzip.NewReader(src)
		if err != nil {
			return nil, err
		}
		defer zr.Close()
		src = zr
	default:
		return nil, errors.New("unsupported content encoding")
	}
	body, err := io.ReadAll(io.LimitReader(src, maxBody+1))
	var tooBig *http.MaxBytesError
	if len(body) > maxBody || errors.As(err, &tooBig) {
		return nil, errTooLarge
	}
	return body, err
}

func (s *Sink) limits(vmID uuid.UUID, now time.Time) *vmLimits {
	v := s.vms[vmID]
	if v == nil {
		v = &vmLimits{tokens: logBurst, filled: now, windowStart: now, series: map[uint64]struct{}{}}
		s.vms[vmID] = v
	}
	return v
}

// admitLogs keeps the records the VM's token bucket allows and returns them encoded, nil if none are left.
func (s *Sink) admitLogs(vmID uuid.UUID, req *collogspb.ExportLogsServiceRequest) []byte {
	n := 0
	for _, rl := range req.ResourceLogs {
		for _, sl := range rl.GetScopeLogs() {
			n += len(sl.GetLogRecords())
		}
	}
	if n == 0 {
		return nil
	}
	now := time.Now()
	s.mu.Lock()
	v := s.limits(vmID, now)
	v.tokens = min(logBurst, v.tokens+now.Sub(v.filled).Seconds()*logRate)
	v.filled = now
	allowed := min(n, int(v.tokens))
	v.tokens -= float64(allowed)
	s.mu.Unlock()
	if allowed < n {
		s.dropped(vmID, "logs", int64(n-allowed))
		left := allowed
		for _, rl := range req.ResourceLogs {
			for _, sl := range rl.GetScopeLogs() {
				k := min(left, len(sl.LogRecords))
				sl.LogRecords = sl.LogRecords[:k]
				left -= k
			}
		}
	}
	if allowed == 0 {
		return nil
	}
	out, _ := gproto.Marshal(req)
	return out
}

// admitMetrics drops new app series past the VM's hourly limit; usage series are not counted.
func (s *Sink) admitMetrics(vmID uuid.UUID, stream string, body []byte) ([]byte, error) {
	var req colmetricspb.ExportMetricsServiceRequest
	if err := gproto.Unmarshal(body, &req); err != nil {
		return nil, err
	}
	if stream != "app" {
		return body, nil
	}
	dropped, kept := 0, false
	now := time.Now()
	s.mu.Lock()
	v := s.limits(vmID, now)
	if now.Sub(v.windowStart) >= seriesWindow {
		clear(v.series)
		v.windowStart = now
	}
	for _, rm := range req.ResourceMetrics {
		res := rm.GetResource().GetAttributes()
		for _, sm := range rm.GetScopeMetrics() {
			sm.Metrics = slices.DeleteFunc(sm.Metrics, func(m *metricspb.Metric) bool {
				left := keepPoints(m, func(attrs []*commonpb.KeyValue) bool {
					key := s.seriesKey(m.GetName(), res, attrs)
					if _, ok := v.series[key]; ok {
						return true
					}
					if len(v.series) < maxSeries {
						v.series[key] = struct{}{}
						return true
					}
					dropped++
					return false
				})
				kept = kept || left
				return !left
			})
		}
	}
	s.mu.Unlock()
	if dropped > 0 {
		s.dropped(vmID, "series", int64(dropped))
	}
	if !kept {
		return nil, nil
	}
	return gproto.Marshal(&req)
}

// keepPoints filters m's data points by their attributes and reports whether any are left.
func keepPoints(m *metricspb.Metric, keep func([]*commonpb.KeyValue) bool) bool {
	switch d := m.Data.(type) {
	case *metricspb.Metric_Gauge:
		if d.Gauge != nil {
			d.Gauge.DataPoints = filterPoints(d.Gauge.DataPoints, keep)
			return len(d.Gauge.DataPoints) > 0
		}
	case *metricspb.Metric_Sum:
		if d.Sum != nil {
			d.Sum.DataPoints = filterPoints(d.Sum.DataPoints, keep)
			return len(d.Sum.DataPoints) > 0
		}
	case *metricspb.Metric_Histogram:
		if d.Histogram != nil {
			d.Histogram.DataPoints = filterPoints(d.Histogram.DataPoints, keep)
			return len(d.Histogram.DataPoints) > 0
		}
	case *metricspb.Metric_ExponentialHistogram:
		if d.ExponentialHistogram != nil {
			d.ExponentialHistogram.DataPoints = filterPoints(d.ExponentialHistogram.DataPoints, keep)
			return len(d.ExponentialHistogram.DataPoints) > 0
		}
	case *metricspb.Metric_Summary:
		if d.Summary != nil {
			d.Summary.DataPoints = filterPoints(d.Summary.DataPoints, keep)
			return len(d.Summary.DataPoints) > 0
		}
	}
	return false
}

func filterPoints[P interface{ GetAttributes() []*commonpb.KeyValue }](pts []P, keep func([]*commonpb.KeyValue) bool) []P {
	return slices.DeleteFunc(pts, func(p P) bool { return !keep(p.GetAttributes()) })
}

// seriesKey hashes the metric name with the resource and point attributes, which the store turns into labels.
func (s *Sink) seriesKey(name string, res, attrs []*commonpb.KeyValue) uint64 {
	var h maphash.Hash
	h.SetSeed(s.seed)
	h.WriteString(name)
	for _, kvs := range [][]*commonpb.KeyValue{res, attrs} {
		h.WriteByte(0)
		sorted := slices.SortedFunc(slices.Values(kvs), func(a, b *commonpb.KeyValue) int { return strings.Compare(a.GetKey(), b.GetKey()) })
		for _, kv := range sorted {
			h.WriteString(kv.GetKey())
			h.WriteByte(0)
			v, _ := gproto.MarshalOptions{Deterministic: true}.Marshal(kv.GetValue())
			h.Write(v)
			h.WriteByte(0)
		}
	}
	return h.Sum64()
}

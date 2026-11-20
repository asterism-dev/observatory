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
	"google.golang.org/protobuf/encoding/protowire"
	gproto "google.golang.org/protobuf/proto"
)

const (
	maxBody        = 1 << 20
	maxLine        = 16 << 10
	logRate        = 100
	logBurst       = 500
	maxSeries      = 2000
	maxUsageSeries = 2000 // cpu × state points alone pass 500 above ~60 vCPUs
	maxAttrs       = 128
	seriesWindow   = time.Hour
	byteRate       = 256 << 10
	byteBurst      = 2 << 20
	// maxItems bounds records or points per request: decoding inflates a body of empty items about a hundredfold.
	maxItems = 100_000
	// maxConns and maxDecodes bound the memory decoding takes, per VM and node-wide.
	maxConns   = 2
	maxDecodes = 4
)

var decodeWait = 5 * time.Second

// forgetDelay keeps an exited VM's forwarder queue long enough for its last lines to reach the hub.
var forgetDelay = time.Minute

var (
	logItems    = [][]protowire.Number{{1}, {2}, {2}}
	metricItems = [][]protowire.Number{{1}, {2}, {2}, {5, 7, 9, 10, 11}, {1}}
)

// guestStreams are the stream classes a guest may send per signal; console and egress only originate on the node.
var guestStreams = map[string][]string{"logs": {"service", "system", "app"}, "metrics": {"usage", "app"}}

var (
	errTooLarge     = errors.New("body too large")
	errTooManyAttrs = errors.New("too many attributes")
)

// usageMetrics are the metrics the guest collector sends on stream usage.
var usageMetrics = []string{"system.cpu.time", "system.memory.usage", "system.filesystem.usage", "system.filesystem.limit", "system.network.io",
	"system.cpu.load_average.1m", "system.cpu.load_average.5m", "system.cpu.load_average.15m"}

// Sink enforces the per-VM limits on telemetry from guests and node-origin streams before handing it to the Forwarder.
type Sink struct {
	fw      *Forwarder
	dropped func(vmID uuid.UUID, kind string, n int64)
	seed    maphash.Seed
	decodes chan struct{}

	mu      sync.Mutex
	vms     map[uuid.UUID]*vmLimits   // registered VMs
	forgets map[uuid.UUID]*time.Timer // pending forwarder Forgets of exited VMs
}

// vmLimits is one VM's limit state; its own lock keeps one VM's admission from holding up another's.
type vmLimits struct {
	mu          sync.Mutex
	logs, bytes bucket
	windowStart time.Time
	series      map[string]map[uint64]struct{} // per stream, the series seen in the current window
}

type bucket struct {
	tokens float64
	filled time.Time
}

// refill adds what accrued since the last refill at rate per second, up to burst.
func (b *bucket) refill(now time.Time, rate, burst float64) {
	b.tokens = min(burst, b.tokens+now.Sub(b.filled).Seconds()*rate)
	b.filled = now
}

// NewSink forwards through fw and reports every drop to dropped, which may be nil.
func NewSink(fw *Forwarder, dropped func(vmID uuid.UUID, kind string, n int64)) *Sink {
	if dropped == nil {
		dropped = func(uuid.UUID, string, int64) {}
	}
	return &Sink{fw: fw, dropped: dropped, seed: maphash.MakeSeed(), decodes: make(chan struct{}, maxDecodes), vms: map[uuid.UUID]*vmLimits{}, forgets: map[uuid.UUID]*time.Timer{}}
}

// Serve answers OTLP/HTTP requests on l, the listener of vmID only, until l is closed; it starts vmID with fresh limits.
func (s *Sink) Serve(l net.Listener, vmID uuid.UUID) {
	s.mu.Lock()
	v := s.vms[vmID]
	s.mu.Unlock()
	if v == nil {
		return
	}
	srv := &http.Server{
		Handler:           http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { s.handle(w, r, vmID, v) }),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       time.Minute,
		IdleTimeout:       2 * time.Minute,
		MaxHeaderBytes:    16 << 10,
	}
	srv.Serve(capListener{l, make(chan struct{}, maxConns)})
}

// capListener closes connections past the cap; unlike netutil.LimitListener it never waits for a slot, so closing l ends Serve.
type capListener struct {
	net.Listener
	slots chan struct{}
}

func (l capListener) Accept() (net.Conn, error) {
	for {
		c, err := l.Listener.Accept()
		if err != nil {
			return nil, err
		}
		select {
		case l.slots <- struct{}{}:
			return &slotConn{Conn: c, release: sync.OnceFunc(func() { <-l.slots })}, nil
		default:
			c.Close()
		}
	}
}

type slotConn struct {
	net.Conn
	release func()
}

func (c *slotConn) Close() error {
	c.release()
	return c.Conn.Close()
}

// Register starts vmID with fresh limits when it boots; Emit and Serve accept a VM only from here until Forget.
func (s *Sink) Register(vmID uuid.UUID) {
	now := time.Now()
	s.mu.Lock()
	defer s.mu.Unlock()
	s.vms[vmID] = &vmLimits{logs: bucket{logBurst, now}, bytes: bucket{byteBurst, now}, windowStart: now, series: map[string]map[uint64]struct{}{}}
	if t := s.forgets[vmID]; t != nil {
		t.Stop()
		delete(s.forgets, vmID)
	}
}

// LineRecord is an info-level record of line, truncated at maxLine bytes, stamped now.
func LineRecord(line string) *logspb.LogRecord {
	now := uint64(time.Now().UnixNano())
	return &logspb.LogRecord{
		TimeUnixNano: now, ObservedTimeUnixNano: now,
		SeverityNumber: logspb.SeverityNumber_SEVERITY_NUMBER_INFO, SeverityText: "INFO",
		Body: &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: strings.ToValidUTF8(line[:min(len(line), maxLine)], "")}},
	}
}

// Emit forwards node-origin log records of vmID under the VM's log limit; records of a VM not registered are dropped.
func (s *Sink) Emit(vmID uuid.UUID, stream string, records []*logspb.LogRecord) {
	s.mu.Lock()
	v := s.vms[vmID]
	s.mu.Unlock()
	if v == nil {
		return
	}
	req := &collogspb.ExportLogsServiceRequest{ResourceLogs: []*logspb.ResourceLogs{{ScopeLogs: []*logspb.ScopeLogs{{LogRecords: records}}}}}
	if out := s.admitLogs(vmID, v, req); out != nil {
		s.fw.Send(vmID, "logs", stream, out)
	}
}

// Forget drops an exited VM's limit state (in-flight requests keep theirs) and its forwarder queue forgetDelay later unless it registers again.
func (s *Sink) Forget(vmID uuid.UUID) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.vms, vmID)
	if t := s.forgets[vmID]; t != nil {
		t.Stop()
	}
	var t *time.Timer
	t = time.AfterFunc(forgetDelay, func() {
		s.mu.Lock()
		defer s.mu.Unlock()
		if s.forgets[vmID] == t {
			delete(s.forgets, vmID)
			s.fw.Forget(vmID)
		}
	})
	s.forgets[vmID] = t
}

func (s *Sink) handle(w http.ResponseWriter, r *http.Request, vmID uuid.UUID, v *vmLimits) {
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
	if err == nil && !v.takeBytes(len(body)) {
		s.dropped(vmID, "body", 1)
		http.Error(w, "rate limited", http.StatusTooManyRequests)
		return
	}
	items := logItems
	if signal == "metrics" {
		items = metricItems
	}
	var n int
	if err == nil {
		n, err = countFields(body, items)
	}
	if err == nil && n > maxItems {
		s.dropped(vmID, "body", 1)
		http.Error(w, "too many items", http.StatusRequestEntityTooLarge)
		return
	}
	if err == nil {
		t := time.NewTimer(decodeWait)
		select {
		case s.decodes <- struct{}{}:
			t.Stop()
			defer func() { <-s.decodes }()
		case <-t.C:
			http.Error(w, "busy", http.StatusServiceUnavailable)
			return
		}
	}
	var out []byte
	if err == nil && signal == "logs" {
		out, err = s.admitLogsBody(vmID, v, body)
	} else if err == nil {
		out, err = s.admitMetrics(vmID, v, stream, body)
	}
	if errors.Is(err, errTooManyAttrs) {
		s.dropped(vmID, "body", 1)
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

func (v *vmLimits) takeBytes(n int) bool {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.bytes.refill(time.Now(), byteRate, byteBurst)
	if float64(n) > v.bytes.tokens {
		return false
	}
	v.bytes.tokens -= float64(n)
	return true
}

// countFields counts the length-delimited fields reached by following path, one set of field numbers per nesting level, without decoding.
func countFields(b []byte, path [][]protowire.Number) (int, error) {
	n := 0
	for len(b) > 0 {
		num, typ, l := protowire.ConsumeTag(b)
		if l < 0 {
			return 0, protowire.ParseError(l)
		}
		b = b[l:]
		l = protowire.ConsumeFieldValue(num, typ, b)
		if l < 0 {
			return 0, protowire.ParseError(l)
		}
		if typ == protowire.BytesType && slices.Contains(path[0], num) {
			if len(path) == 1 {
				n++
			} else {
				v, _ := protowire.ConsumeBytes(b)
				k, err := countFields(v, path[1:])
				if err != nil {
					return 0, err
				}
				n += k
			}
		}
		b = b[l:]
	}
	return n, nil
}

func tooManyAttrs(attrs ...[]*commonpb.KeyValue) bool {
	return slices.ContainsFunc(attrs, func(a []*commonpb.KeyValue) bool { return len(a) > maxAttrs })
}

func (s *Sink) admitLogsBody(vmID uuid.UUID, v *vmLimits, body []byte) ([]byte, error) {
	var req collogspb.ExportLogsServiceRequest
	if err := gproto.Unmarshal(body, &req); err != nil {
		return nil, err
	}
	for _, rl := range req.ResourceLogs {
		if tooManyAttrs(rl.GetResource().GetAttributes()) {
			return nil, errTooManyAttrs
		}
		for _, sl := range rl.GetScopeLogs() {
			if tooManyAttrs(sl.GetScope().GetAttributes()) || slices.ContainsFunc(sl.GetLogRecords(), func(r *logspb.LogRecord) bool { return tooManyAttrs(r.GetAttributes()) }) {
				return nil, errTooManyAttrs
			}
		}
	}
	return s.admitLogs(vmID, v, &req), nil
}

// admitLogs keeps the records the VM's token bucket allows and returns them encoded, nil if none are left.
func (s *Sink) admitLogs(vmID uuid.UUID, v *vmLimits, req *collogspb.ExportLogsServiceRequest) []byte {
	n := 0
	for _, rl := range req.ResourceLogs {
		for _, sl := range rl.GetScopeLogs() {
			n += len(sl.GetLogRecords())
		}
	}
	if n == 0 {
		return nil
	}
	v.mu.Lock()
	v.logs.refill(time.Now(), logRate, logBurst)
	allowed := min(n, int(v.logs.tokens))
	v.logs.tokens -= float64(allowed)
	v.mu.Unlock()
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

type seriesPoint struct {
	key     uint64
	allowed bool // false for a metric the stream does not accept
}

// admitMetrics drops new series past the VM's hourly limit of the stream and, on usage, every metric the guest collector does not send.
func (s *Sink) admitMetrics(vmID uuid.UUID, v *vmLimits, stream string, body []byte) ([]byte, error) {
	var req colmetricspb.ExportMetricsServiceRequest
	if err := gproto.Unmarshal(body, &req); err != nil {
		return nil, err
	}
	// Keys are hashed before taking the lock, so a costly request only delays itself.
	var points []seriesPoint
	for _, rm := range req.ResourceMetrics {
		res := rm.GetResource().GetAttributes()
		if tooManyAttrs(res) {
			return nil, errTooManyAttrs
		}
		resKey := s.attrsKey(res)
		for _, sm := range rm.GetScopeMetrics() {
			if tooManyAttrs(sm.GetScope().GetAttributes()) {
				return nil, errTooManyAttrs
			}
			for _, m := range sm.GetMetrics() {
				allowed := stream != "usage" || slices.Contains(usageMetrics, m.GetName())
				tooMany := false
				keepPoints(m, func(attrs []*commonpb.KeyValue) bool {
					tooMany = tooMany || tooManyAttrs(attrs)
					points = append(points, seriesPoint{s.seriesKey(m.GetName(), resKey, attrs), allowed})
					return true
				})
				if tooMany {
					return nil, errTooManyAttrs
				}
			}
		}
	}
	limit := maxSeries
	if stream == "usage" {
		limit = maxUsageSeries
	}
	keep := make([]bool, len(points))
	dropped := 0
	now := time.Now()
	v.mu.Lock()
	if now.Sub(v.windowStart) >= seriesWindow {
		clear(v.series)
		v.windowStart = now
	}
	seen := v.series[stream]
	if seen == nil {
		seen = map[uint64]struct{}{}
		v.series[stream] = seen
	}
	for i, p := range points {
		if _, ok := seen[p.key]; ok && p.allowed {
			keep[i] = true
		} else if p.allowed && len(seen) < limit {
			seen[p.key] = struct{}{}
			keep[i] = true
		} else {
			dropped++
		}
	}
	v.mu.Unlock()
	if dropped > 0 {
		s.dropped(vmID, "series", int64(dropped))
	}
	if dropped == len(points) {
		return nil, nil
	}
	if dropped > 0 {
		i := 0
		for _, rm := range req.ResourceMetrics {
			for _, sm := range rm.GetScopeMetrics() {
				sm.Metrics = slices.DeleteFunc(sm.Metrics, func(m *metricspb.Metric) bool {
					return !keepPoints(m, func([]*commonpb.KeyValue) bool { i++; return keep[i-1] })
				})
			}
		}
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

// attrsKey hashes attributes independent of their order.
func (s *Sink) attrsKey(attrs []*commonpb.KeyValue) uint64 {
	var h maphash.Hash
	h.SetSeed(s.seed)
	sorted := slices.SortedFunc(slices.Values(attrs), func(a, b *commonpb.KeyValue) int { return strings.Compare(a.GetKey(), b.GetKey()) })
	for _, kv := range sorted {
		h.WriteString(kv.GetKey())
		h.WriteByte(0)
		v, _ := gproto.MarshalOptions{Deterministic: true}.Marshal(kv.GetValue())
		h.Write(v)
		h.WriteByte(0)
	}
	return h.Sum64()
}

// seriesKey hashes the metric name with the resource and point attributes, which the store turns into labels.
func (s *Sink) seriesKey(name string, resKey uint64, attrs []*commonpb.KeyValue) uint64 {
	var h maphash.Hash
	h.SetSeed(s.seed)
	h.WriteString(name)
	h.WriteByte(0)
	maphash.WriteComparable(&h, resKey)
	maphash.WriteComparable(&h, s.attrsKey(attrs))
	return h.Sum64()
}

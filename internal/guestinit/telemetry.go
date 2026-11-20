package guestinit

import (
	"bytes"
	"compress/gzip"
	"context"
	"io"
	"net"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	logspb "go.opentelemetry.io/proto/otlp/logs/v1"
	metricspb "go.opentelemetry.io/proto/otlp/metrics/v1"
	resourcepb "go.opentelemetry.io/proto/otlp/resource/v1"
	gproto "google.golang.org/protobuf/proto"
)

const (
	maxSinkQueue = 256
	// Metrics bodies are queued unsplit, so the count limit alone would allow hundreds of MiB.
	maxSinkQueueBytes = 16 << 20
	// 64 KiB per request keeps the full queue at 16 MiB and a request well inside the node's 1 MiB body and 256 KiB/s byte rate.
	maxRequestBytes   = 64 << 10
	maxRequestRecords = 500
	maxSinkBackoff    = 30 * time.Second
)

var sinkBackoff = time.Second

// initResource marks what the init itself sends, as the node's own telemetry carries its service.name.
var initResource = &resourcepb.Resource{Attributes: []*commonpb.KeyValue{{Key: "service.name", Value: stringValue("observatory-init")}}}

// Sink sends OTLP to the node's telemetry listener; a bounded queue that drops the oldest request keeps callers from ever blocking.
type Sink struct {
	client     *http.Client
	minBackoff time.Duration
	connected  atomic.Bool
	wake       chan struct{}

	mu         sync.Mutex
	queue      []sinkRequest
	queueBytes int
}

type sinkRequest struct {
	signal, stream string
	// LogsData and MetricsData encode like the collector's Export*ServiceRequest, whose packages would pull gRPC into the init.
	msg  gproto.Message
	size int
}

func newSink(dial func(context.Context) (net.Conn, error)) *Sink {
	s := &Sink{wake: make(chan struct{}, 1), minBackoff: sinkBackoff}
	s.client = &http.Client{
		Timeout: 30 * time.Second,
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				c, err := dial(ctx)
				s.connected.Store(err == nil)
				return c, err
			},
			MaxConnsPerHost:    1,
			IdleConnTimeout:    time.Minute,
			DisableCompression: true,
		},
	}
	go s.run()
	return s
}

// Connected reports whether the last connection attempt to the node succeeded.
func (s *Sink) Connected() bool { return s != nil && s.connected.Load() }

// Logs queues recs for stream, split into requests of at most maxRequestRecords and maxRequestBytes.
func (s *Sink) Logs(stream string, recs []*logspb.LogRecord) {
	var res *resourcepb.Resource
	if stream == "system" {
		res = initResource
	}
	for len(recs) > 0 {
		n, size := 0, 0
		for ; n < len(recs) && n < maxRequestRecords; n++ {
			size += gproto.Size(recs[n])
			if n > 0 && size > maxRequestBytes {
				break
			}
		}
		s.enqueue(sinkRequest{signal: "logs", stream: stream, msg: &logspb.LogsData{ResourceLogs: []*logspb.ResourceLogs{{Resource: res, ScopeLogs: []*logspb.ScopeLogs{{LogRecords: recs[:n]}}}}}})
		recs = recs[n:]
	}
}

func (s *Sink) Metrics(stream string, rms []*metricspb.ResourceMetrics) {
	s.enqueue(sinkRequest{signal: "metrics", stream: stream, msg: &metricspb.MetricsData{ResourceMetrics: rms}})
}

func (s *Sink) enqueue(r sinkRequest) {
	r.size = gproto.Size(r.msg)
	s.requeue(r, false)
	select {
	case s.wake <- struct{}{}:
	default:
	}
}

// requeue adds r at the back, or the front for a retry, dropping the oldest requests past the limit.
func (s *Sink) requeue(r sinkRequest, front bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if front {
		s.queue = append([]sinkRequest{r}, s.queue...)
	} else {
		s.queue = append(s.queue, r)
	}
	s.queueBytes += r.size
	over := 0
	for dropped := s.queueBytes; len(s.queue)-over > maxSinkQueue || (dropped > maxSinkQueueBytes && over < len(s.queue)-1); over++ {
		dropped -= s.queue[over].size
	}
	for _, d := range s.queue[:over] {
		s.queueBytes -= d.size
	}
	clear(s.queue[:over])
	s.queue = s.queue[over:]
}

func (s *Sink) pop() (sinkRequest, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.queue) == 0 {
		return sinkRequest{}, false
	}
	r := s.queue[0]
	s.queueBytes -= r.size
	s.queue[0] = sinkRequest{}
	s.queue = s.queue[1:]
	return r, true
}

// run sends one request at a time; a node it cannot reach is retried with backoff, any non-2xx answer drops the request.
func (s *Sink) run() {
	backoff := s.minBackoff
	for {
		r, ok := s.pop()
		if !ok {
			<-s.wake
			continue
		}
		status, err := s.post(r)
		if err != nil {
			s.requeue(r, true)
		}
		if err != nil || status == http.StatusTooManyRequests || status == http.StatusServiceUnavailable {
			time.Sleep(backoff)
			backoff = min(backoff*2, maxSinkBackoff)
			continue
		}
		backoff = s.minBackoff
	}
}

func (s *Sink) post(r sinkRequest) (int, error) {
	raw, err := gproto.Marshal(r.msg)
	if err != nil {
		return 0, nil
	}
	var body bytes.Buffer
	zw := gzip.NewWriter(&body)
	zw.Write(raw)
	zw.Close()
	req, err := http.NewRequest(http.MethodPost, "http://vsock/v1/"+r.signal, &body)
	if err != nil {
		return 0, nil
	}
	req.Header.Set("Content-Type", "application/x-protobuf")
	req.Header.Set("Content-Encoding", "gzip")
	req.Header.Set("X-Observatory-Stream", r.stream)
	resp, err := s.client.Do(req)
	if err != nil {
		return 0, err
	}
	io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
	resp.Body.Close()
	return resp.StatusCode, nil
}

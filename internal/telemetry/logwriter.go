package telemetry

import (
	"context"
	"log/slog"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	collogspb "go.opentelemetry.io/proto/otlp/collector/logs/v1"
	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	logspb "go.opentelemetry.io/proto/otlp/logs/v1"
	resourcepb "go.opentelemetry.io/proto/otlp/resource/v1"
	"google.golang.org/protobuf/proto"
)

const (
	logBatchLines   = 500
	logBatchEvery   = time.Second
	logFlushTimeout = 2 * time.Second
	logMaxBody      = 16 << 10
	logWarnEvery    = time.Minute
)

type queuedLine struct {
	resource []*commonpb.KeyValue
	body     string
	at       time.Time
}

// LogWriter queues log lines and ships them to the log store in the background; Write never blocks.
type LogWriter struct {
	client       *Client
	streamFields string
	maxQueue     int
	wake         chan struct{}

	mu       sync.Mutex
	queue    []queuedLine
	lastWarn time.Time
}

func NewLogWriter(c *Client, streamFields string, maxQueue int) *LogWriter {
	return &LogWriter{client: c, streamFields: streamFields, maxQueue: maxQueue, wake: make(chan struct{}, 1)}
}

// Write queues one record per line, dropping the oldest queued lines when full.
func (w *LogWriter) Write(resource []*commonpb.KeyValue, lines []string) {
	now := time.Now()
	w.mu.Lock()
	for _, l := range lines {
		w.queue = append(w.queue, queuedLine{resource, truncateUTF8(l, logMaxBody), now})
	}
	if drop := len(w.queue) - w.maxQueue; drop > 0 {
		w.queue = append(w.queue[:0], w.queue[drop:]...)
	}
	full := len(w.queue) >= logBatchLines
	w.mu.Unlock()
	if full {
		select {
		case w.wake <- struct{}{}:
		default:
		}
	}
}

func (w *LogWriter) Queued() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return len(w.queue)
}

// Run ships queued lines until ctx is done, then flushes the rest within a short deadline.
func (w *LogWriter) Run(ctx context.Context) {
	tick := time.NewTicker(logBatchEvery)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			fctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), logFlushTimeout)
			w.drain(fctx, true)
			cancel()
			return
		case <-w.wake:
			w.drain(ctx, false)
		case <-tick.C:
			w.drain(ctx, true)
		}
	}
}

// drain sends full batches, and also the remainder when all is set.
func (w *LogWriter) drain(ctx context.Context, all bool) {
	for ctx.Err() == nil {
		batch := w.take(all)
		if batch == nil {
			return
		}
		if err := w.client.WriteLogs(ctx, buildLogRequest(batch), w.streamFields); err != nil {
			w.warn(err, len(batch))
		}
	}
}

func (w *LogWriter) take(all bool) []queuedLine {
	w.mu.Lock()
	defer w.mu.Unlock()
	n := min(len(w.queue), logBatchLines)
	if n == 0 || (n < logBatchLines && !all) {
		return nil
	}
	batch := append([]queuedLine(nil), w.queue[:n]...)
	w.queue = append(w.queue[:0], w.queue[n:]...)
	return batch
}

func (w *LogWriter) warn(err error, lines int) {
	w.mu.Lock()
	ok := time.Since(w.lastWarn) >= logWarnEvery
	if ok {
		w.lastWarn = time.Now()
	}
	w.mu.Unlock()
	if ok {
		slog.Warn("telemetry log write failed; dropping batch", "lines", lines, "error", err)
	}
}

func buildLogRequest(batch []queuedLine) *collogspb.ExportLogsServiceRequest {
	req := &collogspb.ExportLogsServiceRequest{}
	groups := map[string]*logspb.ScopeLogs{}
	for _, q := range batch {
		key := resourceKey(q.resource)
		sl, ok := groups[key]
		if !ok {
			sl = &logspb.ScopeLogs{}
			groups[key] = sl
			req.ResourceLogs = append(req.ResourceLogs, &logspb.ResourceLogs{
				Resource:  &resourcepb.Resource{Attributes: q.resource},
				ScopeLogs: []*logspb.ScopeLogs{sl},
			})
		}
		sl.LogRecords = append(sl.LogRecords, &logspb.LogRecord{
			TimeUnixNano:         uint64(q.at.UnixNano()),
			ObservedTimeUnixNano: uint64(q.at.UnixNano()),
			SeverityNumber:       logspb.SeverityNumber_SEVERITY_NUMBER_INFO,
			SeverityText:         "INFO",
			Body:                 &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: q.body}},
		})
	}
	return req
}

func resourceKey(attrs []*commonpb.KeyValue) string {
	var sb strings.Builder
	for _, kv := range attrs {
		b, _ := proto.MarshalOptions{Deterministic: true}.Marshal(kv)
		sb.Write(b)
		sb.WriteByte(0)
	}
	return sb.String()
}

func truncateUTF8(s string, max int) string {
	if len(s) <= max {
		return s
	}
	for max > 0 && !utf8.RuneStart(s[max]) {
		max--
	}
	return s[:max]
}

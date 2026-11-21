package node

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
	"go.opentelemetry.io/contrib/bridges/otelslog"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlplog/otlploghttp"
	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetrichttp"
	"go.opentelemetry.io/otel/metric"
	sdklog "go.opentelemetry.io/otel/sdk/log"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/resource"
	semconv "go.opentelemetry.io/otel/semconv/v1.26.0"

	"github.com/asterism-dev/observatory/internal/node/vmtelemetry"
)

const (
	telemetryTimeout   = 10 * time.Second
	telemetryLogQueue  = 10000
	telemetryMeterName = "observatory-node"
	otelErrorInterval  = 5 * time.Minute
)

type Telemetry struct {
	Meter    metric.Meter
	shutdown func(context.Context) error
}

func StartTelemetry(ctx context.Context, connectURL, credential, version string) (*Telemetry, error) {
	return startTelemetry(ctx, connectURL, credential, version, nil)
}

func startTelemetry(ctx context.Context, connectURL, credential, version string, client *http.Client) (*Telemetry, error) {
	base := hubBase(connectURL) + "/otlp"
	headers := map[string]string{"Authorization": "Bearer " + credential}
	res := resource.NewSchemaless(semconv.ServiceName("observatory-node"), semconv.ServiceVersion(version))

	logOpts := []otlploghttp.Option{
		otlploghttp.WithEndpointURL(base + "/v1/logs"),
		otlploghttp.WithHeaders(headers),
		otlploghttp.WithCompression(otlploghttp.GzipCompression),
		otlploghttp.WithTimeout(telemetryTimeout),
		// retryingLogExporter retries with the next batch; the exporter's own backoff would block the processor.
		otlploghttp.WithRetry(otlploghttp.RetryConfig{Enabled: false}),
	}
	metricOpts := []otlpmetrichttp.Option{
		otlpmetrichttp.WithEndpointURL(base + "/v1/metrics"),
		otlpmetrichttp.WithHeaders(headers),
		otlpmetrichttp.WithCompression(otlpmetrichttp.GzipCompression),
		otlpmetrichttp.WithTimeout(telemetryTimeout),
	}
	logTransport := http.DefaultTransport
	if client != nil {
		metricOpts = append(metricOpts, otlpmetrichttp.WithHTTPClient(client))
		if client.Transport != nil {
			logTransport = client.Transport
		}
	}
	retrying := &retryingLogExporter{limit: telemetryLogQueue}
	logOpts = append(logOpts, otlploghttp.WithHTTPClient(&http.Client{
		Transport: statusRecorder{RoundTripper: logTransport, status: &retrying.status},
		Timeout:   telemetryTimeout,
	}))

	logExp, err := otlploghttp.New(ctx, logOpts...)
	if err != nil {
		return nil, err
	}
	retrying.Exporter = logExp
	metricExp, err := otlpmetrichttp.New(ctx, metricOpts...)
	if err != nil {
		return nil, err
	}

	logProvider := sdklog.NewLoggerProvider(
		sdklog.WithResource(res),
		sdklog.WithProcessor(sdklog.NewBatchProcessor(
			retrying,
			sdklog.WithMaxQueueSize(telemetryLogQueue),
			sdklog.WithExportTimeout(telemetryTimeout),
		)),
	)
	meterProvider := sdkmetric.NewMeterProvider(
		sdkmetric.WithResource(res),
		sdkmetric.WithReader(sdkmetric.NewPeriodicReader(metricExp, sdkmetric.WithInterval(10*time.Second), sdkmetric.WithTimeout(telemetryTimeout))),
	)

	otel.SetErrorHandler(newOTelErrorHandler(os.Stderr))
	otelHandler := defaultSource{Handler: otelslog.NewHandler(telemetryMeterName, otelslog.WithLoggerProvider(logProvider))}
	slog.SetDefault(slog.New(slog.NewMultiHandler(slog.NewTextHandler(os.Stderr, nil), otelHandler)))

	return &Telemetry{
		Meter: meterProvider.Meter(telemetryMeterName),
		shutdown: func(ctx context.Context) error {
			return errors.Join(logProvider.Shutdown(ctx), meterProvider.Shutdown(ctx))
		},
	}, nil
}

func (t *Telemetry) Shutdown(ctx context.Context) error { return t.shutdown(ctx) }

// NewVMTelemetry forwards VM telemetry to the hub of connectURL and counts its drops on tel's meter; tel may be nil.
func NewVMTelemetry(connectURL, credential string, tel *Telemetry) (*vmtelemetry.Sink, *vmtelemetry.Forwarder, error) {
	fw := vmtelemetry.NewForwarder(hubBase(connectURL), credential, &http.Client{})
	if tel == nil {
		return vmtelemetry.NewSink(fw, nil), fw, nil
	}
	dropped, err := tel.Meter.Int64Counter("observatory.vm.telemetry.dropped")
	if err != nil {
		return nil, nil, err
	}
	return vmtelemetry.NewSink(fw, func(vmID uuid.UUID, kind string, n int64) {
		dropped.Add(context.Background(), n, metric.WithAttributes(attribute.String("vm", vmID.String()), attribute.String("kind", kind)))
	}), fw, nil
}

// defaultSource tags records without a source attribute as coming from the node agent.
type defaultSource struct {
	slog.Handler
	hasSource bool
}

func (h defaultSource) Handle(ctx context.Context, r slog.Record) error {
	out := slog.NewRecord(r.Time, r.Level, r.Message, r.PC)
	hasSource := h.hasSource
	r.Attrs(func(a slog.Attr) bool {
		hasSource = hasSource || a.Key == "source"
		out.AddAttrs(durationString(a))
		return true
	})
	if !hasSource {
		out.AddAttrs(slog.String("source", "observatory-node"))
	}
	return h.Handler.Handle(ctx, out)
}

func (h defaultSource) WithAttrs(attrs []slog.Attr) slog.Handler {
	out := make([]slog.Attr, len(attrs))
	for i, a := range attrs {
		h.hasSource = h.hasSource || a.Key == "source"
		out[i] = durationString(a)
	}
	h.Handler = h.Handler.WithAttrs(out)
	return h
}

// otelslog exports durations as integer nanoseconds; the string form matches stderr.
func durationString(a slog.Attr) slog.Attr {
	a.Value = a.Value.Resolve()
	switch a.Value.Kind() {
	case slog.KindDuration:
		return slog.String(a.Key, a.Value.Duration().String())
	case slog.KindGroup:
		group := a.Value.Group()
		out := make([]slog.Attr, len(group))
		for i, g := range group {
			out[i] = durationString(g)
		}
		return slog.Attr{Key: a.Key, Value: slog.GroupValue(out...)}
	}
	return a
}

// Records under a group would get a prefixed source, so none is added.
func (h defaultSource) WithGroup(name string) slog.Handler {
	h.hasSource = true
	h.Handler = h.Handler.WithGroup(name)
	return h
}

// otelErrorHandler logs SDK errors to stderr only: the default handler's log.Print would feed them back into the log exporter.
type otelErrorHandler struct {
	log  *slog.Logger
	mu   sync.Mutex
	last map[string]time.Time
}

func newOTelErrorHandler(w io.Writer) *otelErrorHandler {
	return &otelErrorHandler{log: slog.New(slog.NewTextHandler(w, nil)), last: map[string]time.Time{}}
}

func (h *otelErrorHandler) Handle(err error) {
	msg := err.Error()
	h.mu.Lock()
	if t, ok := h.last[msg]; ok && time.Since(t) < otelErrorInterval {
		h.mu.Unlock()
		return
	}
	if len(h.last) >= 100 {
		clear(h.last)
	}
	h.last[msg] = time.Now()
	h.mu.Unlock()
	h.log.Warn("telemetry export error", "err", err)
}

// retryingLogExporter resends records that failed retryably with the next batch (the processor drops them), keeping at most limit.
type retryingLogExporter struct {
	sdklog.Exporter
	limit   int
	mu      sync.Mutex
	pending []sdklog.Record
	// status is the HTTP status of the last request, 0 if it got no response.
	status atomic.Int32
}

const telemetryLogChunk = 512

func (e *retryingLogExporter) Export(ctx context.Context, records []sdklog.Record) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	for i := range records {
		e.pending = append(e.pending, records[i].Clone())
	}
	if over := len(e.pending) - e.limit; over > 0 {
		clear(e.pending[:over])
		e.pending = e.pending[over:]
	}
	var errs []error
	for len(e.pending) > 0 {
		n := min(len(e.pending), telemetryLogChunk)
		e.status.Store(0)
		if err := e.Exporter.Export(ctx, e.pending[:n]); err != nil {
			errs = append(errs, err)
			// Mirrors the SDK's retryable set; anything else would be rejected again, so the chunk is dropped.
			if code := e.status.Load(); code == 0 || code == http.StatusTooManyRequests || code >= 500 {
				return errors.Join(errs...)
			}
		}
		clear(e.pending[:n])
		e.pending = e.pending[n:]
	}
	return errors.Join(errs...)
}

func (e *retryingLogExporter) ForceFlush(ctx context.Context) error {
	return errors.Join(e.Export(ctx, nil), e.Exporter.ForceFlush(ctx))
}

type statusRecorder struct {
	http.RoundTripper
	status *atomic.Int32
}

func (s statusRecorder) RoundTrip(r *http.Request) (*http.Response, error) {
	resp, err := s.RoundTripper.RoundTrip(r)
	if err == nil {
		s.status.Store(int32(resp.StatusCode))
	}
	return resp, err
}

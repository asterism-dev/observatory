package node

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"time"

	"go.opentelemetry.io/contrib/bridges/otelslog"
	"go.opentelemetry.io/otel/exporters/otlp/otlplog/otlploghttp"
	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetrichttp"
	"go.opentelemetry.io/otel/metric"
	sdklog "go.opentelemetry.io/otel/sdk/log"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/resource"
	semconv "go.opentelemetry.io/otel/semconv/v1.26.0"
)

const (
	telemetryTimeout   = 10 * time.Second
	telemetryLogQueue  = 10000
	telemetryMeterName = "observatory-node"
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
	}
	metricOpts := []otlpmetrichttp.Option{
		otlpmetrichttp.WithEndpointURL(base + "/v1/metrics"),
		otlpmetrichttp.WithHeaders(headers),
		otlpmetrichttp.WithCompression(otlpmetrichttp.GzipCompression),
		otlpmetrichttp.WithTimeout(telemetryTimeout),
	}
	if client != nil {
		logOpts = append(logOpts, otlploghttp.WithHTTPClient(client))
		metricOpts = append(metricOpts, otlpmetrichttp.WithHTTPClient(client))
	}

	logExp, err := otlploghttp.New(ctx, logOpts...)
	if err != nil {
		return nil, err
	}
	metricExp, err := otlpmetrichttp.New(ctx, metricOpts...)
	if err != nil {
		return nil, err
	}

	logProvider := sdklog.NewLoggerProvider(
		sdklog.WithResource(res),
		sdklog.WithProcessor(sdklog.NewBatchProcessor(logExp, sdklog.WithMaxQueueSize(telemetryLogQueue))),
	)
	meterProvider := sdkmetric.NewMeterProvider(
		sdkmetric.WithResource(res),
		sdkmetric.WithReader(sdkmetric.NewPeriodicReader(metricExp, sdkmetric.WithInterval(10*time.Second), sdkmetric.WithTimeout(telemetryTimeout))),
	)

	otelHandler := defaultSource{otelslog.NewHandler(telemetryMeterName, otelslog.WithLoggerProvider(logProvider))}
	slog.SetDefault(slog.New(slog.NewMultiHandler(slog.NewTextHandler(os.Stderr, nil), otelHandler)))

	return &Telemetry{
		Meter: meterProvider.Meter(telemetryMeterName),
		shutdown: func(ctx context.Context) error {
			return errors.Join(logProvider.Shutdown(ctx), meterProvider.Shutdown(ctx))
		},
	}, nil
}

func (t *Telemetry) Shutdown(ctx context.Context) error { return t.shutdown(ctx) }

// defaultSource tags records without a source attribute as coming from the node agent.
type defaultSource struct{ slog.Handler }

func (h defaultSource) Handle(ctx context.Context, r slog.Record) error {
	hasSource := false
	r.Attrs(func(a slog.Attr) bool {
		hasSource = hasSource || a.Key == "source"
		return !hasSource
	})
	if !hasSource {
		r.AddAttrs(slog.String("source", "observatory-node"))
	}
	return h.Handler.Handle(ctx, r)
}

func (h defaultSource) WithAttrs(attrs []slog.Attr) slog.Handler {
	return defaultSource{h.Handler.WithAttrs(attrs)}
}

func (h defaultSource) WithGroup(name string) slog.Handler {
	return defaultSource{h.Handler.WithGroup(name)}
}

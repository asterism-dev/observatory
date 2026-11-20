package guestinit

import (
	"compress/gzip"
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"strings"
	"time"

	logspb "go.opentelemetry.io/proto/otlp/logs/v1"
	metricspb "go.opentelemetry.io/proto/otlp/metrics/v1"
	"google.golang.org/protobuf/encoding/protojson"
	gproto "google.golang.org/protobuf/proto"
)

const maxReceiverBody = 1 << 20

// startReceiver serves OTLP/HTTP logs and metrics from apps on httpAddr into stream app; a busy port is logged and skipped.
func startReceiver(ctx context.Context, sink *Sink, httpAddr string) (stop func(), err error) {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/logs", func(w http.ResponseWriter, r *http.Request) {
		var data logspb.LogsData
		if !receive(w, r, &data) {
			return
		}
		for _, rl := range data.ResourceLogs {
			for _, sl := range rl.ScopeLogs {
				sink.Logs("app", sl.LogRecords)
			}
		}
	})
	mux.HandleFunc("POST /v1/metrics", func(w http.ResponseWriter, r *http.Request) {
		var data metricspb.MetricsData
		if !receive(w, r, &data) {
			return
		}
		sink.Metrics("app", data.ResourceMetrics)
	})
	srv := &http.Server{Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	l, err := net.Listen("tcp", httpAddr)
	if err != nil {
		logf("init: otlp receiver %s: %v", httpAddr, err)
		return func() {}, nil
	}
	go func() {
		if err := srv.Serve(l); !errors.Is(err, http.ErrServerClosed) {
			logf("init: otlp receiver: %v", err)
		}
	}()
	stop = func() { _ = srv.Close() }
	go func() { <-ctx.Done(); stop() }()
	return stop, nil
}

// receive decodes the request body into msg and answers errors itself; success is an empty Export*ServiceResponse.
func receive(w http.ResponseWriter, r *http.Request, msg gproto.Message) bool {
	var body io.Reader = http.MaxBytesReader(w, r.Body, maxReceiverBody)
	if r.Header.Get("Content-Encoding") == "gzip" {
		zr, err := gzip.NewReader(body)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return false
		}
		body = io.LimitReader(zr, maxReceiverBody+1)
	}
	raw, err := io.ReadAll(body)
	var tooBig *http.MaxBytesError
	if errors.As(err, &tooBig) || len(raw) > maxReceiverBody {
		http.Error(w, "request too large", http.StatusRequestEntityTooLarge)
		return false
	}
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return false
	}
	ct, _, _ := strings.Cut(r.Header.Get("Content-Type"), ";")
	switch ct = strings.TrimSpace(ct); ct {
	case "application/x-protobuf":
		err = gproto.Unmarshal(raw, msg)
	case "application/json":
		err = protojson.UnmarshalOptions{DiscardUnknown: true}.Unmarshal(raw, msg)
	default:
		http.Error(w, "unsupported content type", http.StatusUnsupportedMediaType)
		return false
	}
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return false
	}
	w.Header().Set("Content-Type", ct)
	if ct == "application/json" {
		_, _ = w.Write([]byte("{}"))
	}
	return true
}

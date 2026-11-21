package guestinit

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	logspb "go.opentelemetry.io/proto/otlp/logs/v1"
	metricspb "go.opentelemetry.io/proto/otlp/metrics/v1"
	"google.golang.org/protobuf/encoding/protojson"
	gproto "google.golang.org/protobuf/proto"
)

const (
	maxReceiverBody  = 1 << 20
	maxReceiverConns = 64
)

// cappedListener closes connections beyond the slot count so an app cannot exhaust PID 1's descriptors.
type cappedListener struct {
	net.Listener
	slots chan struct{}
}

func (l *cappedListener) Accept() (net.Conn, error) {
	for {
		c, err := l.Listener.Accept()
		if err != nil {
			return nil, err
		}
		select {
		case l.slots <- struct{}{}:
			return &cappedConn{Conn: c, release: sync.OnceFunc(func() { <-l.slots })}, nil
		default:
			_ = c.Close()
		}
	}
}

type cappedConn struct {
	net.Conn
	release func()
}

func (c *cappedConn) Close() error {
	c.release()
	return c.Conn.Close()
}

// startReceiver serves OTLP/HTTP logs and metrics from apps on httpAddr into stream app; a busy port is logged and skipped.
func startReceiver(ctx context.Context, sink *Sink, httpAddr string) (stop func(), err error) {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/logs", func(w http.ResponseWriter, r *http.Request) {
		var data logspb.LogsData
		if !receive(w, r, &data) {
			return
		}
		for _, rl := range data.ResourceLogs {
			sink.LogsData("app", rl)
		}
	})
	mux.HandleFunc("POST /v1/metrics", func(w http.ResponseWriter, r *http.Request) {
		var data metricspb.MetricsData
		if !receive(w, r, &data) {
			return
		}
		sink.Metrics("app", data.ResourceMetrics)
	})
	srv := &http.Server{
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       60 * time.Second,
		MaxHeaderBytes:    16 << 10,
	}
	l, err := net.Listen("tcp", httpAddr)
	if err != nil {
		logf("init: otlp receiver %s: %v", httpAddr, err)
		return func() {}, nil
	}
	l = &cappedListener{Listener: l, slots: make(chan struct{}, maxReceiverConns)}
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
		if raw, err = hexIDsToBase64(raw); err == nil {
			err = protojson.UnmarshalOptions{DiscardUnknown: true}.Unmarshal(raw, msg)
		}
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

// hexIDsToBase64 rewrites OTLP/JSON trace and span ids, which are hex there but base64 in protojson.
func hexIDsToBase64(raw []byte) ([]byte, error) {
	var doc any
	d := json.NewDecoder(bytes.NewReader(raw))
	// Numbers stay json.Number: as float64 an int64 like timeUnixNano would lose precision.
	d.UseNumber()
	if err := d.Decode(&doc); err != nil {
		return nil, err
	}
	if _, err := d.Token(); err != io.EOF {
		return nil, errors.New("invalid JSON: data after the top-level value")
	}
	var walk func(v any)
	walk = func(v any) {
		switch v := v.(type) {
		case map[string]any:
			for k, e := range v {
				if s, ok := e.(string); ok && (k == "traceId" || k == "spanId" || k == "parentSpanId") {
					if b, err := hex.DecodeString(s); err == nil {
						v[k] = base64.StdEncoding.EncodeToString(b)
					}
					continue
				}
				walk(e)
			}
		case []any:
			for _, e := range v {
				walk(e)
			}
		}
	}
	walk(doc)
	return json.Marshal(doc)
}

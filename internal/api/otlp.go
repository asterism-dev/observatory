package api

import (
	"compress/gzip"
	"errors"
	"io"
	"log/slog"
	"mime"
	"net/http"
	"slices"
	"strings"

	"github.com/google/uuid"
	collogspb "go.opentelemetry.io/proto/otlp/collector/logs/v1"
	colmetricspb "go.opentelemetry.io/proto/otlp/collector/metrics/v1"
	gproto "google.golang.org/protobuf/proto"

	"github.com/asterism-dev/observatory/internal/store"
	"github.com/asterism-dev/observatory/internal/telemetry"
)

const maxOTLPBody = 4 << 20

func OTLPEndpoints(mux *http.ServeMux, s *store.Store, c *telemetry.Client) {
	mux.HandleFunc("POST /otlp/v1/metrics", ingest(s, c, []string{"usage", "app"}, func(r *http.Request, body []byte, id telemetry.Identity, c *telemetry.Client) (gproto.Message, error) {
		var req colmetricspb.ExportMetricsServiceRequest
		if err := gproto.Unmarshal(body, &req); err != nil {
			return nil, errBadBody
		}
		telemetry.StampMetrics(&req, id)
		return &colmetricspb.ExportMetricsServiceResponse{}, c.WriteMetrics(r.Context(), &req)
	}))
	mux.HandleFunc("POST /otlp/v1/logs", ingest(s, c, []string{"service", "console", "app", "system", "egress"}, func(r *http.Request, body []byte, id telemetry.Identity, c *telemetry.Client) (gproto.Message, error) {
		var req collogspb.ExportLogsServiceRequest
		if err := gproto.Unmarshal(body, &req); err != nil {
			return nil, errBadBody
		}
		telemetry.StampLogs(&req, id)
		fields := telemetry.NodeLogStreamFields
		if id.VMID != uuid.Nil {
			fields = telemetry.VMLogStreamFields
		}
		return &collogspb.ExportLogsServiceResponse{}, c.WriteLogs(r.Context(), &req, fields)
	}))
}

var errBadBody = errors.New("invalid OTLP body")

func ingest(s *store.Store, c *telemetry.Client, streams []string, write func(*http.Request, []byte, telemetry.Identity, *telemetry.Client) (gproto.Message, error)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if c == nil {
			http.NotFound(w, r)
			return
		}
		cred, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		node, err := s.NodeByCredential(r.Context(), cred)
		if !ok || errors.Is(err, store.ErrNotFound) {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		if err != nil {
			serverError(w, "node auth", err)
			return
		}
		if node.Quarantined {
			http.Error(w, "node quarantined", http.StatusForbidden)
			return
		}
		id := telemetry.Identity{OrgID: node.OrgID, NodeID: node.ID, NodeName: node.Name}
		if h := r.Header.Get("X-Observatory-VM"); h != "" {
			vmID, err := uuid.Parse(h)
			if err != nil {
				http.Error(w, "invalid X-Observatory-VM", http.StatusBadRequest)
				return
			}
			id.Stream = r.Header.Get("X-Observatory-Stream")
			if !slices.Contains(streams, id.Stream) {
				http.Error(w, "invalid X-Observatory-Stream", http.StatusBadRequest)
				return
			}
			v, err := s.VM(r.Context(), node.OrgID, vmID)
			if errors.Is(err, store.ErrNotFound) || err == nil && v.NodeID != node.ID {
				http.Error(w, "vm not on this node", http.StatusForbidden)
				return
			}
			if err != nil {
				serverError(w, "otlp vm lookup", err)
				return
			}
			id.VMID, id.VMName = v.ID, v.Name
		}
		if mt, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type")); mt != "application/x-protobuf" {
			http.Error(w, "only application/x-protobuf is supported", http.StatusUnsupportedMediaType)
			return
		}
		var src io.Reader = http.MaxBytesReader(w, r.Body, maxOTLPBody)
		switch r.Header.Get("Content-Encoding") {
		case "", "identity":
		case "gzip":
			zr, err := gzip.NewReader(src)
			if err != nil {
				http.Error(w, "invalid gzip body", http.StatusBadRequest)
				return
			}
			defer zr.Close()
			src = zr
		default:
			http.Error(w, "unsupported content encoding", http.StatusUnsupportedMediaType)
			return
		}
		body, err := io.ReadAll(io.LimitReader(src, maxOTLPBody+1))
		var tooBig *http.MaxBytesError
		if len(body) > maxOTLPBody || errors.As(err, &tooBig) {
			http.Error(w, "body too large", http.StatusRequestEntityTooLarge)
			return
		}
		if err != nil {
			http.Error(w, "invalid body", http.StatusBadRequest)
			return
		}
		resp, err := write(r, body, id, c)
		switch {
		case errors.Is(err, telemetry.ErrStore):
			slog.Warn("telemetry store", "err", err)
			http.Error(w, "telemetry store unavailable", http.StatusServiceUnavailable)
			return
		case err != nil:
			slog.Warn("telemetry ingest rejected", "node", node.ID, "err", err)
			http.Error(w, "invalid telemetry", http.StatusBadRequest)
			return
		}
		out, err := gproto.Marshal(resp)
		if err != nil {
			serverError(w, "marshal otlp response", err)
			return
		}
		w.Header().Set("Content-Type", "application/x-protobuf")
		_, _ = w.Write(out)
	}
}

package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/asterism-dev/observatory/internal/store"
	"github.com/asterism-dev/observatory/internal/telemetry"
)

const (
	maxTelemetrySpan = 31 * 24 * time.Hour
	usageTimeout     = 15 * time.Second
	defaultLogLimit  = 500
	maxLogLimit      = 2000
)

func (h *api) usageNode(w http.ResponseWriter, r *http.Request) {
	if h.telemetry == nil {
		writeError(w, http.StatusNotFound, "not found")
		return
	}
	n, ok := h.nodeFromPath(w, r, store.ActionNodeMetrics)
	if !ok {
		return
	}
	from, to, ok := timeRange(w, r, 6*time.Hour)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), usageTimeout)
	defer cancel()
	u, err := h.telemetry.NodeUsage(ctx, n.OrgID, n.ID, from, to)
	if err != nil {
		telemetryError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, u)
}

func (h *api) nodeLogs(w http.ResponseWriter, r *http.Request) {
	if h.telemetry == nil {
		writeError(w, http.StatusNotFound, "not found")
		return
	}
	n, ok := h.nodeFromPath(w, r, store.ActionNodeMetrics)
	if !ok {
		return
	}
	q, ok := logQuery(w, r)
	if !ok {
		return
	}
	q.Org, q.Node = n.OrgID, n.ID
	h.writeLogs(w, r, q)
}

func (h *api) writeLogs(w http.ResponseWriter, r *http.Request, q telemetry.LogQuery) {
	var ok bool
	if q.From, q.To, ok = timeRange(w, r, time.Hour); !ok {
		return
	}
	q.Limit = defaultLogLimit
	if v := r.URL.Query().Get("limit"); v != "" {
		var err error
		if q.Limit, err = strconv.Atoi(v); err != nil || q.Limit < 1 || q.Limit > maxLogLimit {
			writeError(w, http.StatusBadRequest, "invalid limit")
			return
		}
	}
	lines, err := h.telemetry.Logs(r.Context(), q)
	if err != nil {
		telemetryError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"lines": lines})
}

func (h *api) tailNodeLogs(w http.ResponseWriter, r *http.Request) {
	if h.telemetry == nil {
		writeError(w, http.StatusNotFound, "not found")
		return
	}
	n, ok := h.nodeFromPath(w, r, store.ActionNodeMetrics)
	if !ok {
		return
	}
	q, ok := logQuery(w, r)
	if !ok {
		return
	}
	q.Org, q.Node = n.OrgID, n.ID
	h.tailLogs(w, r, q, "node", n.ID)
}

func (h *api) tailLogs(w http.ResponseWriter, r *http.Request, q telemetry.LogQuery, kind string, id uuid.UUID) {
	rc := http.NewResponseController(w)
	// A tail is open for as long as the page is, longer than the server's write timeout.
	_ = rc.SetWriteDeadline(time.Time{})
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(http.StatusOK)
	_ = rc.Flush()
	err := h.telemetry.TailLogs(r.Context(), q, func(l telemetry.LogLine) error {
		b, err := json.Marshal(l)
		if err != nil {
			return err
		}
		if _, err := fmt.Fprintf(w, "data: %s\n\n", b); err != nil {
			return err
		}
		return rc.Flush()
	})
	if err != nil && r.Context().Err() == nil {
		slog.Warn("tail "+kind+" logs", kind, id, "err", err)
	}
}

func timeRange(w http.ResponseWriter, r *http.Request, def time.Duration) (from, to time.Time, ok bool) {
	to = time.Now()
	var err error
	if v := r.URL.Query().Get("to"); v != "" {
		if to, err = time.Parse(time.RFC3339, v); err != nil {
			writeError(w, http.StatusBadRequest, "invalid to")
			return
		}
	}
	from = to.Add(-def)
	if v := r.URL.Query().Get("from"); v != "" {
		if from, err = time.Parse(time.RFC3339, v); err != nil {
			writeError(w, http.StatusBadRequest, "invalid from")
			return
		}
	}
	if !from.Before(to) || to.Sub(from) > maxTelemetrySpan {
		writeError(w, http.StatusBadRequest, "invalid range")
		return
	}
	return from, to, true
}

func logQuery(w http.ResponseWriter, r *http.Request) (telemetry.LogQuery, bool) {
	v := r.URL.Query()
	q := telemetry.LogQuery{Text: v.Get("q"), Level: v.Get("level"), Source: v.Get("source"), Fields: map[string]string{}}
	switch q.Level {
	case "", "debug", "info", "warn", "error":
	default:
		writeError(w, http.StatusBadRequest, "invalid level")
		return q, false
	}
	for _, f := range v["field"] {
		k, val, _ := strings.Cut(f, ":")
		if k == "" {
			writeError(w, http.StatusBadRequest, "invalid field")
			return q, false
		}
		q.Fields[k] = val
	}
	return q, true
}

func telemetryError(w http.ResponseWriter, err error) {
	if !errors.Is(err, telemetry.ErrStore) && !errors.Is(err, context.DeadlineExceeded) {
		slog.Warn("telemetry query", "err", err)
	}
	writeError(w, http.StatusBadGateway, "telemetry store unavailable")
}

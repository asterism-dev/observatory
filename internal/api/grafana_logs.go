package api

import (
	"context"
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/asterism-dev/observatory/internal/telemetry"
)

var grafanaLogsPath = regexp.MustCompile(`^/grafana/logs/select/logsql/(query|tail|hits|field_names|field_values|stream_field_names|stream_field_values|streams|stream_ids|stats_query|stats_query_range|facets)$`)

// logsProxy forwards an allowlisted LogsQL read to VictoriaLogs, confined to the token's org.
func (h *api) logsProxy(w http.ResponseWriter, r *http.Request) {
	tok, done, ok := h.grafanaRequest(w, r)
	if !ok {
		return
	}
	defer done()
	m := grafanaLogsPath.FindStringSubmatch(r.URL.Path)
	if m == nil || r.URL.EscapedPath() != r.URL.Path || (r.Method != http.MethodGet && r.Method != http.MethodPost) {
		http.NotFound(w, r)
		return
	}
	if m[1] == "tail" {
		release, ok := h.grafanaTails.acquire(tok.ID)
		if !ok {
			writeError(w, http.StatusTooManyRequests, "too many concurrent tails")
			return
		}
		defer release()
	}
	params, ok := grafanaParams(w, r)
	if !ok {
		return
	}
	stripParams(params, "extra_filters", "extra_stream_filters")
	if telemetry.DeniedLogsQL(params) {
		writeError(w, http.StatusBadRequest, "query uses a LogsQL construct the hub does not allow")
		return
	}
	params.Set("extra_filters", fmt.Sprintf("observatory.org:=%q", tok.OrgID.String()))

	timeout := grafanaUpstreamTimeout
	if m[1] == "tail" {
		timeout = h.grafanaTailMax
	}
	ctx, cancel := context.WithTimeout(r.Context(), timeout)
	defer cancel()
	if m[1] == "tail" {
		// Bounded (and cut on revoke) so a client that stops reading cannot hold the stream open.
		rc := http.NewResponseController(w)
		_ = rc.SetWriteDeadline(time.Now().Add(h.grafanaTailMax + 5*time.Second))
		w = flushWriter{w}
		go h.endTailOnRevoke(ctx, cancel, r, rc, h.grafanaTailRecheck)
	}
	// A fresh request carries no client headers, so AccountID/ProjectID never reach VictoriaLogs.
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, h.telemetry.LogsURL+"/select/logsql/"+m[1], strings.NewReader(params.Encode()))
	if err != nil {
		serverError(w, "logs proxy request", err)
		return
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	forwardUpstream(w, h.telemetry.StoreClient(0), req)
}

// endTailOnRevoke re-runs token auth; a creator losing access shows up only once the auth cache (≤ 60 s) expires.
func (h *api) endTailOnRevoke(ctx context.Context, cancel context.CancelFunc, r *http.Request, rc *http.ResponseController, every time.Duration) {
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if _, _, status := h.grafanaAuth(r); status == http.StatusUnauthorized || status == http.StatusServiceUnavailable {
				// Once the handler has returned, the connection may already serve another request.
				if ctx.Err() == nil {
					_ = rc.SetWriteDeadline(time.Now())
				}
				cancel()
				return
			}
		}
	}
}

// flushWriter pushes every chunk to the client so tailed rows are not held in the server's buffer.
type flushWriter struct{ http.ResponseWriter }

func (f flushWriter) Write(p []byte) (int, error) {
	n, err := f.ResponseWriter.Write(p)
	if err == nil {
		err = http.NewResponseController(f.ResponseWriter).Flush()
	}
	return n, err
}

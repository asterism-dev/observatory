package api

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/asterism-dev/observatory/internal/telemetry"
)

var (
	grafanaLogsPath = regexp.MustCompile(`^/grafana/logs/select/logsql/(query|tail|hits|field_names|field_values|stream_field_names|stream_field_values|streams|stream_ids|stats_query|stats_query_range|facets)$`)
	// Words and single ASCII punctuation; anything else separates tokens.
	logsqlToken = regexp.MustCompile("[a-z0-9_]+|[!-/:-@\\[-`{-~]")
)

// logsProxy forwards an allowlisted LogsQL read to VictoriaLogs, confined to the token's org.
func (h *api) logsProxy(w http.ResponseWriter, r *http.Request) {
	org, ok := h.grafanaRequest(w, r)
	if !ok {
		return
	}
	m := grafanaLogsPath.FindStringSubmatch(r.URL.Path)
	if m == nil || r.URL.EscapedPath() != r.URL.Path || (r.Method != http.MethodGet && r.Method != http.MethodPost) {
		http.NotFound(w, r)
		return
	}
	params, ok := grafanaParams(w, r)
	if !ok {
		return
	}
	stripParams(params, "extra_filters", "extra_stream_filters")
	if usesLeakingLogsQL(params) {
		writeError(w, http.StatusBadRequest, "query uses a LogsQL construct the hub does not allow")
		return
	}
	params.Set("extra_filters", fmt.Sprintf("observatory.org:=%q", org.String()))

	timeout := grafanaUpstreamTimeout
	if m[1] == "tail" {
		timeout = h.grafanaTailMax
	}
	ctx, cancel := context.WithTimeout(r.Context(), timeout)
	defer cancel()
	if m[1] == "tail" {
		_ = http.NewResponseController(w).SetWriteDeadline(time.Time{})
		w = flushWriter{w}
		go h.endTailOnRevoke(ctx, cancel, r, h.grafanaTailRecheck)
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
func (h *api) endTailOnRevoke(ctx context.Context, cancel context.CancelFunc, r *http.Request, every time.Duration) {
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if _, _, status := h.grafanaAuth(r); status == http.StatusUnauthorized {
				cancel()
				return
			}
		}
	}
}

// usesLeakingLogsQL is a conservative token scan of every parameter for telemetry.LeakingLogsQL constructs.
func usesLeakingLogsQL(params url.Values) bool {
	for _, c := range telemetry.LeakingLogsQL {
		construct := logsqlToken.FindAllString(strings.ToLower(c), -1)
		if len(construct) == 0 {
			continue
		}
		for _, vs := range params {
			for _, v := range vs {
				toks := logsqlToken.FindAllString(strings.ToLower(v), -1)
				for i := range toks {
					if slices.Equal(toks[i:min(i+len(construct), len(toks))], construct) {
						return true
					}
				}
			}
		}
	}
	return false
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

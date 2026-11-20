package api

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"mime"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
)

const (
	grafanaUpstreamTimeout = 60 * time.Second
	grafanaMaxBody         = 1 << 20
)

var promPath = regexp.MustCompile(`^/grafana/prometheus/api/v1/(query|query_range|series|labels|label/[a-zA-Z_][a-zA-Z0-9_.]*/values|status/buildinfo|metadata)$`)

// grafanaRequest authenticates a Grafana bearer token and applies its rate limit; telemetry off reads as 404.
func (h *api) grafanaRequest(w http.ResponseWriter, r *http.Request) (uuid.UUID, bool) {
	if h.telemetry == nil {
		http.NotFound(w, r)
		return uuid.Nil, false
	}
	org, tok, status := h.grafanaAuth(r)
	switch {
	case status == http.StatusUnauthorized:
		writeError(w, status, "unauthorized")
		return uuid.Nil, false
	case status != http.StatusOK:
		writeError(w, status, "internal error")
		return uuid.Nil, false
	case !h.grafanaLimit.allow(tok.ID, h.now()):
		writeError(w, http.StatusTooManyRequests, "rate limit exceeded")
		return uuid.Nil, false
	}
	return org, true
}

// grafanaParams merges the query string and, for POST, a url-encoded body of at most 1 MiB; it writes the error itself.
func grafanaParams(w http.ResponseWriter, r *http.Request) (url.Values, bool) {
	params, err := url.ParseQuery(r.URL.RawQuery)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid query string")
		return nil, false
	}
	if r.Method != http.MethodPost {
		return params, true
	}
	if ct := r.Header.Get("Content-Type"); ct != "" {
		if mt, _, err := mime.ParseMediaType(ct); err != nil || mt != "application/x-www-form-urlencoded" {
			writeError(w, http.StatusUnsupportedMediaType, "expected application/x-www-form-urlencoded")
			return nil, false
		}
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, grafanaMaxBody))
	if mbe := (*http.MaxBytesError)(nil); errors.As(err, &mbe) {
		writeError(w, http.StatusRequestEntityTooLarge, "request body too large")
		return nil, false
	} else if err != nil {
		writeError(w, http.StatusBadRequest, "invalid body")
		return nil, false
	}
	form, err := url.ParseQuery(string(body))
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid form")
		return nil, false
	}
	for k, v := range form {
		params[k] = append(params[k], v...)
	}
	return params, true
}

func stripParams(v url.Values, names ...string) {
	for k := range v {
		for _, n := range names {
			if strings.EqualFold(k, n) {
				delete(v, k)
			}
		}
	}
}

// promProxy forwards an allowlisted Prometheus API read to VictoriaMetrics, confined to the token's org.
func (h *api) promProxy(w http.ResponseWriter, r *http.Request) {
	org, ok := h.grafanaRequest(w, r)
	if !ok {
		return
	}
	// Any escape in the path (%2F, %2e, ...) is refused rather than decoded and re-matched.
	m := promPath.FindStringSubmatch(r.URL.Path)
	if m == nil || r.URL.EscapedPath() != r.URL.Path || (r.Method != http.MethodGet && r.Method != http.MethodPost) {
		http.NotFound(w, r)
		return
	}
	// VictoriaMetrics keeps metadata per metric family, not per series, so extra_label may not scope it.
	if m[1] == "metadata" {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"status":"success","data":{}}`)
		return
	}
	params, ok := grafanaParams(w, r)
	if !ok {
		return
	}
	stripParams(params, "extra_label", "extra_filters", "extra_filters[]")
	params.Set("extra_label", "observatory_org="+org.String())

	ctx, cancel := context.WithTimeout(r.Context(), grafanaUpstreamTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, h.telemetry.MetricsURL+"/api/v1/"+m[1], strings.NewReader(params.Encode()))
	if err != nil {
		serverError(w, "prometheus proxy request", err)
		return
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	forwardUpstream(w, req)
}

// forwardUpstream relays the store's answer; store failures become a bare 502 so its URL never reaches the client.
func forwardUpstream(w http.ResponseWriter, req *http.Request) {
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		slog.Warn("grafana proxy upstream", "err", err)
		writeError(w, http.StatusBadGateway, "telemetry store unavailable")
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 500 {
		slog.Warn("grafana proxy upstream", "status", resp.StatusCode)
		writeError(w, http.StatusBadGateway, "telemetry store unavailable")
		return
	}
	if ct := resp.Header.Get("Content-Type"); ct != "" {
		w.Header().Set("Content-Type", ct)
	}
	w.WriteHeader(resp.StatusCode)
	_, _ = io.Copy(w, resp.Body)
}

type tokenBucket struct {
	tokens float64
	at     time.Time
}

// rateLimiter is a token bucket per telemetry token, shared by the Grafana proxies.
type rateLimiter struct {
	mu          sync.Mutex
	rate, burst float64
	buckets     map[uuid.UUID]*tokenBucket
	swept       time.Time
}

func newRateLimiter(rate, burst float64) *rateLimiter {
	return &rateLimiter{rate: rate, burst: burst, buckets: map[uuid.UUID]*tokenBucket{}}
}

func (l *rateLimiter) allow(id uuid.UUID, now time.Time) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if now.Sub(l.swept) >= time.Minute {
		// A bucket idle long enough to refill is indistinguishable from a new one.
		full := time.Duration(l.burst / l.rate * float64(time.Second))
		for id, b := range l.buckets {
			if now.Sub(b.at) >= full {
				delete(l.buckets, id)
			}
		}
		l.swept = now
	}
	b, ok := l.buckets[id]
	if !ok {
		b = &tokenBucket{tokens: l.burst, at: now}
		l.buckets[id] = b
	}
	b.tokens = min(l.burst, b.tokens+now.Sub(b.at).Seconds()*l.rate)
	b.at = now
	if b.tokens < 1 {
		return false
	}
	b.tokens--
	return true
}

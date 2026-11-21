package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"slices"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/asterism-dev/observatory/internal/auth"
	"github.com/asterism-dev/observatory/internal/authz"
	"github.com/asterism-dev/observatory/internal/store"
	"github.com/asterism-dev/observatory/internal/telemetry"
)

const (
	logsTimeout  = 30 * time.Second
	logsMaxBody  = 64 << 10
	logsCacheTTL = 30 * time.Second
)

// logsHubWait is how long an explorer query waits for a hub-wide slot before a 429.
var logsHubWait = 5 * time.Second

var errLogsBusy = errors.New("too many concurrent log queries")

// logStreams are the known VM and node streams; image streams are build ids.
var logStreams = map[telemetry.GroupKind][]string{
	telemetry.KindVM:   {"service", "console", "app", "system", "egress"},
	telemetry.KindNode: {"observatory-node", "firecracker"},
}

type logGroupJSON struct {
	Kind    telemetry.GroupKind `json:"kind"`
	ID      uuid.UUID           `json:"id"`
	Name    string              `json:"name"`
	Deleted bool                `json:"deleted"`
	Streams int                 `json:"streams"`
	LastAt  time.Time           `json:"lastAt"`
}

type logStreamJSON struct {
	Name   string        `json:"name"`
	LastAt time.Time     `json:"lastAt"`
	Build  *logBuildJSON `json:"build,omitempty"`
}

type logBuildJSON struct {
	StartedAt *time.Time `json:"startedAt"`
	Status    string     `json:"status"`
}

type logGroupCache struct {
	mu   sync.Mutex
	orgs map[uuid.UUID]*logGroupEntry
}

type logGroupEntry struct {
	mu     sync.Mutex
	at     time.Time
	groups []telemetry.LogGroup
}

// get returns org's groups loaded less than logsCacheTTL ago, else loads them; callers of one org wait for a single load.
func (c *logGroupCache) get(org uuid.UUID, now time.Time, load func() ([]telemetry.LogGroup, error)) ([]telemetry.LogGroup, error) {
	c.mu.Lock()
	if c.orgs == nil {
		c.orgs = map[uuid.UUID]*logGroupEntry{}
	}
	e := c.orgs[org]
	if e == nil {
		e = &logGroupEntry{}
		c.orgs[org] = e
	}
	c.mu.Unlock()
	e.mu.Lock()
	defer e.mu.Unlock()
	if !e.at.IsZero() && now.Sub(e.at) < logsCacheTTL {
		return e.groups, nil
	}
	groups, err := load()
	if err != nil {
		return nil, err
	}
	e.groups, e.at = groups, now
	return groups, nil
}

// logsRequest checks telemetry, membership, telemetry.logEntries.list and the caller's rate limit; it writes errors itself.
func (h *api) logsRequest(w http.ResponseWriter, r *http.Request) (org, user uuid.UUID, ok bool) {
	if h.telemetry == nil {
		writeError(w, http.StatusNotFound, "not found")
		return org, user, false
	}
	m, ok := h.orgFromPath(w, r)
	if !ok || !h.authorize(w, r, m.OrgID, authz.ObjLogs, "telemetry.logEntries.list") {
		return org, user, false
	}
	p, _ := auth.PrincipalFrom(r.Context())
	if !h.logsLimit.allow(p.User.ID, h.now()) {
		writeError(w, http.StatusTooManyRequests, "rate limit exceeded")
		return org, user, false
	}
	return m.OrgID, p.User.ID, true
}

// logsSlot takes one of user's query slots and one of the hub's, waiting up to logsHubWait for the latter; the caller calls done.
func (h *api) logsSlot(ctx context.Context, user uuid.UUID) (done func(), err error) {
	release, ok := h.logsQueries.acquire(user)
	if !ok {
		return nil, errLogsBusy
	}
	t := time.NewTimer(logsHubWait)
	defer t.Stop()
	select {
	case h.logsHub <- struct{}{}:
		return func() { <-h.logsHub; release() }, nil
	case <-t.C:
	case <-ctx.Done():
	}
	release()
	return nil, errLogsBusy
}

func logsError(w http.ResponseWriter, err error) {
	if errors.Is(err, errLogsBusy) {
		writeError(w, http.StatusTooManyRequests, err.Error())
		return
	}
	dashboardQueryError(w, err)
}

// clampLogsRange bounds from by the store's retention; it writes a 400 for an empty range.
func (h *api) clampLogsRange(w http.ResponseWriter, from, to time.Time) (time.Time, time.Time, bool) {
	if oldest := h.now().Add(-h.logsRetention); from.Before(oldest) {
		from = oldest
	}
	if !from.Before(to) {
		writeError(w, http.StatusBadRequest, "invalid range")
		return from, to, false
	}
	return from, to, true
}

func groupFromPath(w http.ResponseWriter, r *http.Request) (telemetry.GroupRef, bool) {
	kind, ok := telemetry.ParseGroupKind(r.PathValue("kind"))
	raw := r.PathValue("id")
	id, err := uuid.Parse(raw)
	if !ok || err != nil || id.String() != raw {
		writeError(w, http.StatusBadRequest, "invalid log group")
		return telemetry.GroupRef{}, false
	}
	return telemetry.GroupRef{Kind: kind, ID: id}, true
}

func (h *api) listLogGroups(w http.ResponseWriter, r *http.Request) {
	org, user, ok := h.logsRequest(w, r)
	if !ok {
		return
	}
	var kind telemetry.GroupKind
	if v := r.URL.Query().Get("kind"); v != "" {
		if kind, ok = telemetry.ParseGroupKind(v); !ok {
			writeError(w, http.StatusBadRequest, "invalid kind")
			return
		}
	}
	groups, err := h.logGroups.get(org, h.now(), func() ([]telemetry.LogGroup, error) {
		done, err := h.logsSlot(r.Context(), user)
		if err != nil {
			return nil, err
		}
		defer done()
		ctx, cancel := context.WithTimeout(r.Context(), logsTimeout)
		defer cancel()
		return h.telemetry.LogGroups(ctx, org, h.now().Add(-h.logsRetention))
	})
	if err != nil {
		logsError(w, err)
		return
	}
	names, err := h.liveLogGroupNames(r.Context(), org)
	if err != nil {
		serverError(w, "log group names", err)
		return
	}
	out := []logGroupJSON{}
	for _, g := range groups {
		if kind != "" && g.Kind != kind {
			continue
		}
		name, live := names[telemetry.GroupRef{Kind: g.Kind, ID: g.ID}]
		if !live {
			name = g.Name
		}
		out = append(out, logGroupJSON{Kind: g.Kind, ID: g.ID, Name: name, Deleted: !live, Streams: g.Streams, LastAt: g.LastAt})
	}
	writeJSON(w, http.StatusOK, map[string]any{"retentionSeconds": int64(h.logsRetention.Seconds()), "groups": out})
}

// liveLogGroupNames maps the org's VMs (builders included), nodes and images in Postgres to their current names.
func (h *api) liveLogGroupNames(ctx context.Context, org uuid.UUID) (map[telemetry.GroupRef]string, error) {
	names := map[telemetry.GroupRef]string{}
	vms, err := h.store.VMs(ctx, org)
	if err != nil {
		return nil, err
	}
	builders, err := h.store.BuilderVMs(ctx, org)
	if err != nil {
		return nil, err
	}
	for _, v := range append(vms, builders...) {
		names[telemetry.GroupRef{Kind: telemetry.KindVM, ID: v.ID}] = v.Name
	}
	nodes, err := h.store.Nodes(ctx, org)
	if err != nil {
		return nil, err
	}
	for _, n := range nodes {
		names[telemetry.GroupRef{Kind: telemetry.KindNode, ID: n.ID}] = n.Name
	}
	images, err := h.store.ListImages(ctx, org)
	if err != nil {
		return nil, err
	}
	for _, img := range images {
		names[telemetry.GroupRef{Kind: telemetry.KindImage, ID: img.ID}] = img.Name
	}
	return names, nil
}

func (h *api) listLogStreams(w http.ResponseWriter, r *http.Request) {
	org, user, ok := h.logsRequest(w, r)
	if !ok {
		return
	}
	g, ok := groupFromPath(w, r)
	if !ok {
		return
	}
	done, err := h.logsSlot(r.Context(), user)
	if err != nil {
		logsError(w, err)
		return
	}
	defer done()
	ctx, cancel := context.WithTimeout(r.Context(), logsTimeout)
	defer cancel()
	streams, err := h.telemetry.LogGroupStreams(ctx, org, g, h.now().Add(-h.logsRetention))
	if err != nil {
		logsError(w, err)
		return
	}
	builds := map[string]store.ImageBuild{}
	if g.Kind == telemetry.KindImage {
		if _, err := h.store.Image(r.Context(), org, g.ID); err == nil {
			bs, err := h.store.ImageBuilds(r.Context(), g.ID)
			if err != nil {
				serverError(w, "image builds", err)
				return
			}
			for _, b := range bs {
				builds[b.ID.String()] = b
			}
		} else if !errors.Is(err, store.ErrNotFound) {
			serverError(w, "image", err)
			return
		}
	}
	out := make([]logStreamJSON, 0, len(streams))
	for _, s := range streams {
		j := logStreamJSON{Name: s.Name, LastAt: s.LastAt}
		if b, ok := builds[s.Name]; ok {
			j.Build = &logBuildJSON{StartedAt: b.StartedAt, Status: b.State}
		}
		out = append(out, j)
	}
	writeJSON(w, http.StatusOK, out)
}

// logEntriesQuery parses the detail-page log filters plus the group's stream; it writes errors itself.
func (h *api) logEntriesQuery(w http.ResponseWriter, r *http.Request) (q telemetry.LogQuery, user uuid.UUID, ok bool) {
	org, user, ok := h.logsRequest(w, r)
	if !ok {
		return q, user, false
	}
	g, ok := groupFromPath(w, r)
	if !ok {
		return q, user, false
	}
	f, ok := logQuery(w, r)
	if !ok {
		return q, user, false
	}
	stream := r.URL.Query().Get("stream")
	// An image stream that is no build id would scope GroupLogQuery to all of the image's builds.
	valid := stream == "" || slices.Contains(logStreams[g.Kind], stream)
	if id, err := uuid.Parse(stream); g.Kind == telemetry.KindImage && err == nil && id.String() == stream {
		valid = true
	}
	if !valid {
		writeError(w, http.StatusBadRequest, "invalid stream")
		return q, user, false
	}
	q = h.telemetry.GroupLogQuery(org, g, stream)
	q.Text, q.Level, q.Fields = f.Text, f.Level, f.Fields
	return q, user, true
}

func (h *api) listLogEntries(w http.ResponseWriter, r *http.Request) {
	q, user, ok := h.logEntriesQuery(w, r)
	if !ok {
		return
	}
	v := r.URL.Query()
	to := h.now()
	var err error
	if s := v.Get("to"); s != "" {
		if to, err = time.Parse(time.RFC3339, s); err != nil {
			writeError(w, http.StatusBadRequest, "invalid to")
			return
		}
	}
	from := to.Add(-time.Hour)
	if s := v.Get("from"); s != "" {
		if from, err = time.Parse(time.RFC3339, s); err != nil {
			writeError(w, http.StatusBadRequest, "invalid from")
			return
		}
	}
	if q.From, q.To, ok = h.clampLogsRange(w, from, to); !ok {
		return
	}
	done, err := h.logsSlot(r.Context(), user)
	if err != nil {
		logsError(w, err)
		return
	}
	defer done()
	ctx, cancel := context.WithTimeout(r.Context(), logsTimeout)
	defer cancel()
	h.writeLogLines(w, r.WithContext(ctx), q, streamFixer(q))
}

// streamFixer fills the stream the explorer shows: node lines carry it as source, image lines as the build id.
func streamFixer(q telemetry.LogQuery) func(*telemetry.LogLine) {
	switch {
	case q.Node != uuid.Nil:
		return func(l *telemetry.LogLine) { l.Stream = l.Source }
	case q.Image != uuid.Nil:
		return func(l *telemetry.LogLine) { l.Stream, _ = l.Fields["observatory.build"].(string) }
	}
	return nil
}

func (h *api) tailLogEntries(w http.ResponseWriter, r *http.Request) {
	q, user, ok := h.logEntriesQuery(w, r)
	if !ok {
		return
	}
	release, ok := h.logsTails.acquire(user)
	if !ok {
		writeError(w, http.StatusTooManyRequests, "too many live tails")
		return
	}
	defer release()
	h.tailLogs(w, r, q, "explorer", q.Org, streamFixer(q))
}

func (h *api) queryLogs(w http.ResponseWriter, r *http.Request) {
	org, user, ok := h.logsRequest(w, r)
	if !ok {
		return
	}
	var body struct {
		Query  string
		Groups []struct {
			Kind string
			ID   uuid.UUID
		}
		From, To *time.Time
		Limit    int
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, logsMaxBody)).Decode(&body); err != nil {
		if mbe := (*http.MaxBytesError)(nil); errors.As(err, &mbe) {
			writeError(w, http.StatusRequestEntityTooLarge, "request body too large")
		} else {
			writeError(w, http.StatusBadRequest, "invalid body")
		}
		return
	}
	refs := make([]telemetry.GroupRef, 0, len(body.Groups))
	for _, g := range body.Groups {
		kind, ok := telemetry.ParseGroupKind(g.Kind)
		if !ok {
			writeError(w, http.StatusBadRequest, "invalid group kind")
			return
		}
		refs = append(refs, telemetry.GroupRef{Kind: kind, ID: g.ID})
	}
	to := h.now()
	if body.To != nil {
		to = *body.To
	}
	from := to.Add(-time.Hour)
	if body.From != nil {
		from = *body.From
	}
	if from, to, ok = h.clampLogsRange(w, from, to); !ok {
		return
	}
	if body.Query == "" {
		body.Query = "*"
	}
	done, err := h.logsSlot(r.Context(), user)
	if err != nil {
		logsError(w, err)
		return
	}
	defer done()
	ctx, cancel := context.WithTimeout(r.Context(), logsTimeout)
	defer cancel()
	res, err := h.telemetry.ExplorerQuery(ctx, org, refs, body.Query, from, to, body.Limit)
	if err != nil {
		logsError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"columns": res.Columns, "rows": res.Rows, "stats": res.Stats})
}

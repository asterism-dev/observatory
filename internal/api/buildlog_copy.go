package api

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	commonpb "go.opentelemetry.io/proto/otlp/common/v1"

	"github.com/asterism-dev/observatory/internal/store"
	"github.com/asterism-dev/observatory/internal/telemetry"
)

const (
	// Past this many builds the least recently seen state is dropped; builds ended by the scheduler (timeout, cancel) never reach finish.
	maxCopiedBuilds = 1024
	copyMaxLines    = 10000
	// The writer truncates lines at 16 KiB on a rune boundary; the extra bytes let it find one.
	maxCarriedPartial = 16<<10 + utf8.UTFMax
)

var copyScopeRefresh = 30 * time.Second

type copiedBuild struct {
	checked, seen time.Time
	resource      []*commonpb.KeyValue // nil when the build is not copied
	partial       string
}

// buildLogCopier copies the log chunks of org and team image builds to the log store; Postgres stays the record.
type buildLogCopier struct {
	w     *telemetry.LogWriter
	store *store.Store

	mu     sync.Mutex
	builds map[uuid.UUID]*copiedBuild
}

func newBuildLogCopier(w *telemetry.LogWriter, s *store.Store) *buildLogCopier {
	return &buildLogCopier{w: w, store: s, builds: map[uuid.UUID]*copiedBuild{}}
}

func (c *buildLogCopier) chunk(ctx context.Context, b store.ImageBuild, data []byte) {
	if len(data) == 0 {
		return
	}
	st := c.state(ctx, b)
	if st == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if st.resource == nil {
		return
	}
	all := st.partial + string(data)
	end := strings.LastIndexByte(all, '\n')
	if end < 0 {
		st.partial = strings.Clone(all[:min(len(all), maxCarriedPartial)])
		return
	}
	rest := all[end+1:]
	st.partial = strings.Clone(rest[:min(len(rest), maxCarriedPartial)])
	complete := lastLines(all[:end], copyMaxLines)
	lines := strings.Split(complete, "\n")
	for i := range lines {
		lines[i] = strings.TrimSuffix(lines[i], "\r")
	}
	c.w.Write(st.resource, lines)
}

// lastLines returns the last n lines of s, which has no trailing newline.
func lastLines(s string, n int) string {
	i := len(s)
	for range n {
		if i = strings.LastIndexByte(s[:i], '\n'); i < 0 {
			return s
		}
	}
	return s[i+1:]
}

func (c *buildLogCopier) finish(b store.ImageBuild) {
	c.mu.Lock()
	defer c.mu.Unlock()
	st := c.builds[b.ID]
	delete(c.builds, b.ID)
	if st != nil && st.resource != nil && st.partial != "" {
		c.w.Write(st.resource, []string{strings.TrimSuffix(st.partial, "\r")})
	}
}

func (c *buildLogCopier) state(ctx context.Context, b store.ImageBuild) *copiedBuild {
	now := time.Now()
	c.mu.Lock()
	st := c.builds[b.ID]
	if st != nil && now.Sub(st.checked) < copyScopeRefresh {
		st.seen = now
		c.mu.Unlock()
		return st
	}
	c.mu.Unlock()
	var resource []*commonpb.KeyValue
	img, err := c.store.ImageByID(ctx, b.ImageID)
	switch {
	case errors.Is(err, store.ErrNotFound):
	case err != nil:
		slog.Warn("build log copy: image lookup", "build", b.ID, "err", err)
		return nil
	case img.Scope == store.EnvScopeOrg || img.Scope == store.EnvScopeTeam:
		resource = []*commonpb.KeyValue{
			attr("observatory.org", img.OrgID.String()),
			attr("observatory.image", img.ID.String()),
			attr("observatory.image_name", img.Name),
			attr("observatory.image_scope", string(img.Scope)),
			attr("observatory.build", b.ID.String()),
			attr("observatory.stream", "build"),
			attr("service.name", "observatory-builder"),
		}
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if st = c.builds[b.ID]; st == nil {
		st = &copiedBuild{}
		c.evictOldest()
		c.builds[b.ID] = st
	}
	st.resource, st.checked, st.seen = resource, now, now
	if resource == nil {
		st.partial = ""
	}
	return st
}

func (c *buildLogCopier) evictOldest() {
	if len(c.builds) < maxCopiedBuilds {
		return
	}
	var oldest uuid.UUID
	var at time.Time
	for id, st := range c.builds {
		if at.IsZero() || st.seen.Before(at) {
			oldest, at = id, st.seen
		}
	}
	delete(c.builds, oldest)
}

func attr(k, v string) *commonpb.KeyValue {
	return &commonpb.KeyValue{Key: k, Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: v}}}
}

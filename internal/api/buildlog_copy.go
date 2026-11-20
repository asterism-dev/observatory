package api

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"unicode/utf8"

	"github.com/google/uuid"
	commonpb "go.opentelemetry.io/proto/otlp/common/v1"

	"github.com/asterism-dev/observatory/internal/store"
	"github.com/asterism-dev/observatory/internal/telemetry"
)

const (
	// Past this many builds in flight the oldest-seen state is dropped; builds that never finish via the agent would leak otherwise.
	maxCopiedBuilds = 1024
	// The writer truncates lines at 16 KiB on a rune boundary; the extra bytes let it find one.
	maxCarriedPartial = 16<<10 + utf8.UTFMax
)

type copiedBuild struct {
	resource []*commonpb.KeyValue // nil when the build is not copied
	partial  string
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
	st := c.state(ctx, b)
	if st == nil || st.resource == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	parts := strings.Split(st.partial+string(data), "\n")
	last := len(parts) - 1
	for i := range parts[:last] {
		parts[i] = strings.TrimSuffix(parts[i], "\r")
	}
	st.partial = strings.Clone(parts[last][:min(len(parts[last]), maxCarriedPartial)])
	if last > 0 {
		c.w.Write(st.resource, parts[:last])
	}
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
	c.mu.Lock()
	st := c.builds[b.ID]
	c.mu.Unlock()
	if st != nil {
		return st
	}
	st = &copiedBuild{}
	img, err := c.store.ImageByID(ctx, b.ImageID)
	switch {
	case errors.Is(err, store.ErrNotFound):
	case err != nil:
		slog.Warn("build log copy: image lookup", "build", b.ID, "err", err)
		return nil
	case img.Scope == store.EnvScopeOrg || img.Scope == store.EnvScopeTeam:
		st.resource = []*commonpb.KeyValue{
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
	if cur := c.builds[b.ID]; cur != nil {
		return cur
	}
	if len(c.builds) >= maxCopiedBuilds {
		for k := range c.builds {
			delete(c.builds, k)
			break
		}
	}
	c.builds[b.ID] = st
	return st
}

func attr(k, v string) *commonpb.KeyValue {
	return &commonpb.KeyValue{Key: k, Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: v}}}
}

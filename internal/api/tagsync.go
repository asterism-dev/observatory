package api

import (
	"context"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/asterism-dev/observatory/internal/store"
)

// TagMirror points a registry tag at a digest, so pulls by tag (FROM <hub>/i/<id>:latest) see the hub's tags.
type TagMirror interface {
	PutTag(ctx context.Context, repo, tag, digest string) error
}

const (
	tagSyncTimeout = 2 * time.Minute
	tagSyncRounds  = 3
)

// TagSync copies the hub's image tags into the registry in the background, one run per image at a time; deleted tags stay (no delete by tag).
type TagSync struct {
	store  *store.Store
	mirror TagMirror

	// Held defers syncs until Resume while the registry is garbage-collected.
	Held func() bool

	mu      sync.Mutex
	dirty   map[uuid.UUID]bool // images with a run in flight; true when another change arrived meanwhile
	pending map[uuid.UUID]bool // images whose sync waits for Resume
	wg      sync.WaitGroup
}

// NewTagSync returns nil without a registry, which makes Kick a no-op.
func NewTagSync(s *store.Store, m TagMirror) *TagSync {
	if m == nil {
		return nil
	}
	return &TagSync{store: s, mirror: m, dirty: map[uuid.UUID]bool{}, pending: map[uuid.UUID]bool{}}
}

// Kick schedules a sync of image after its tags changed; a nil TagSync (hub without registry) does nothing.
func (t *TagSync) Kick(image uuid.UUID) {
	if t == nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if _, running := t.dirty[image]; running {
		t.dirty[image] = true
		return
	}
	t.dirty[image] = false
	t.wg.Add(1)
	go t.run(image)
}

// Resume syncs the images deferred while Held.
func (t *TagSync) Resume() {
	if t == nil {
		return
	}
	t.mu.Lock()
	images := t.pending
	t.pending = map[uuid.UUID]bool{}
	t.mu.Unlock()
	for image := range images {
		t.Kick(image)
	}
}

func (t *TagSync) run(image uuid.UUID) {
	defer t.wg.Done()
	for {
		// Checked under the lock so a reopen's Resume cannot miss the image.
		t.mu.Lock()
		if t.Held != nil && t.Held() {
			t.pending[image] = true
			delete(t.dirty, image)
			t.mu.Unlock()
			return
		}
		t.mu.Unlock()
		t.sync(image)
		t.mu.Lock()
		if t.dirty[image] {
			t.dirty[image] = false
			t.mu.Unlock()
			continue
		}
		delete(t.dirty, image)
		t.mu.Unlock()
		return
	}
}

// sync puts every changed tag and re-reads the tags, so a change committed during the puts is not left behind.
func (t *TagSync) sync(image uuid.UUID) {
	ctx, cancel := context.WithTimeout(context.Background(), tagSyncTimeout)
	defer cancel()
	put := map[string]string{}
	for range tagSyncRounds {
		tags, err := t.store.ImageTags(ctx, image)
		if err != nil {
			slog.Error("mirror image tags", "image", image, "err", err)
			return
		}
		changed := false
		for tag, digest := range tags {
			// The builder's own push created its build tag in the registry.
			if strings.HasPrefix(tag, "build-") || put[tag] == digest {
				continue
			}
			changed = true
			if err := t.mirror.PutTag(ctx, imageRepo(image), tag, digest); err != nil {
				slog.Error("mirror image tag", "image", image, "tag", tag, "err", err)
				return
			}
			put[tag] = digest
		}
		if !changed {
			return
		}
	}
}

// wait blocks until no sync runs; for tests.
func (t *TagSync) wait() { t.wg.Wait() }

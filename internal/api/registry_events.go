package api

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strings"

	"github.com/google/uuid"

	"github.com/asterism-dev/observatory/internal/registry"
	"github.com/asterism-dev/observatory/internal/store"
)

// RegistryManifests inspects and removes manifests in the registry.
type RegistryManifests interface {
	ManifestDeleter
	Manifest(ctx context.Context, repo, digest string) (registry.Manifest, error)
}

const maxEventsBody = 4 << 20

var manifestMediaTypes = map[string]bool{
	"application/vnd.oci.image.manifest.v1+json":                true,
	"application/vnd.oci.image.index.v1+json":                   true,
	"application/vnd.docker.distribution.manifest.v2+json":      true,
	"application/vnd.docker.distribution.manifest.list.v2+json": true,
}

type registryEvent struct {
	Action string
	Target struct {
		MediaType, Digest, Repository, Tag string
	}
	Actor struct{ Name string }
}

type registryEvents struct {
	store     *store.Store
	manifests RegistryManifests
	tags      *TagSync
	secret    string
}

func (h *registryEvents) serve(w http.ResponseWriter, r *http.Request) {
	tok, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if !ok || tok == "" || subtle.ConstantTimeCompare([]byte(tok), []byte(h.secret)) != 1 {
		writeError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	var env struct{ Events []registryEvent }
	if err := json.NewDecoder(io.LimitReader(r.Body, maxEventsBody)).Decode(&env); err != nil {
		writeError(w, http.StatusBadRequest, "invalid events")
		return
	}
	for _, ev := range env.Events {
		// The hub's own pushes are tag mirrors of versions it already has.
		if ev.Actor.Name == registry.HubSubject {
			continue
		}
		// ponytail: untagged pushes (index children, digest-only pushes) are skipped; digest-only blobs stay uncounted until a usage reconciliation, and they need the push right.
		if ev.Action != "push" || !manifestMediaTypes[ev.Target.MediaType] || ev.Target.Tag == "" {
			continue
		}
		// The registry redelivers the whole batch on failure, so every step is idempotent.
		if err := h.push(r.Context(), ev); err != nil {
			serverError(w, "registry push event", err)
			return
		}
	}
	w.WriteHeader(http.StatusOK)
}

func (h *registryEvents) push(ctx context.Context, ev registryEvent) error {
	repo, digest := ev.Target.Repository, ev.Target.Digest
	raw, ok := strings.CutPrefix(repo, "i/")
	id, err := uuid.Parse(raw)
	if !ok || err != nil || id.String() != raw {
		return nil
	}
	img, err := h.store.ImageByID(ctx, id)
	if errors.Is(err, store.ErrNotFound) {
		return h.manifests.DeleteManifest(ctx, repo, digest)
	} else if err != nil {
		return err
	}
	m, err := h.manifests.Manifest(ctx, repo, digest)
	if errors.Is(err, registry.ErrNotFound) {
		return nil
	} else if err != nil {
		return err
	}
	if m.Platform != "linux/amd64" {
		return h.reject(ctx, img, repo, digest, fmt.Sprintf("image platform %s is not linux/amd64", m.Platform))
	}
	v := store.ImageVersion{ImageID: img.ID, Digest: digest, Source: store.VersionFromPush}
	for _, l := range m.Layers {
		v.SizeBytes += l.Size
	}
	for _, l := range m.Blobs {
		v.Layers = append(v.Layers, store.ImageLayer{Digest: l.Digest, Size: l.Size})
	}
	_, err = h.store.ImageVersion(ctx, img.ID, digest)
	if errors.Is(err, store.ErrNotFound) {
		if msg, err := h.overLimit(ctx, img.OrgID, v.Layers); err != nil {
			return err
		} else if msg != "" {
			return h.reject(ctx, img, repo, digest, msg)
		}
	} else if err != nil {
		return err
	}
	if err := h.attribute(ctx, &v, ev.Actor.Name); err != nil {
		return err
	}
	tags := []string{ev.Target.Tag, latestTag}
	if v.BuildID != nil {
		// latest moves when the build reports success, not when its push lands.
		tags = slices.DeleteFunc(tags, func(t string) bool { return t == latestTag })
	}
	err = h.store.InTx(ctx, func(tx *store.Store) error {
		if err := tx.AddImageVersion(ctx, v); err != nil {
			return err
		}
		for _, tag := range tags {
			if err := tx.SetImageTag(ctx, img.ID, tag, digest); err != nil {
				return err
			}
		}
		return tx.SetImagePushError(ctx, img.ID, "")
	})
	if err == nil {
		h.tags.Kick(img.ID)
	}
	return err
}

// overLimit returns the error message when the new layers would push the org past its image storage limit.
func (h *registryEvents) overLimit(ctx context.Context, org uuid.UUID, layers []store.ImageLayer) (string, error) {
	ls, err := h.store.Limits(ctx, org)
	if err != nil {
		return "", err
	}
	limit := ls[store.ScopeOrg].ImageStorageGB
	if limit == nil {
		return "", nil
	}
	used, err := h.store.OrgImageUsage(ctx, org, layers...)
	if err != nil || used <= int64(*limit)<<30 {
		return "", err
	}
	return fmt.Sprintf("image storage limit exceeded (%d GB)", *limit), nil
}

// attribute links the version to the build whose token pushed it, else to the pushing user.
func (h *registryEvents) attribute(ctx context.Context, v *store.ImageVersion, actor string) error {
	if raw, ok := strings.CutPrefix(actor, "build:"); ok {
		id, err := uuid.Parse(raw)
		if err != nil {
			return nil
		}
		b, err := h.store.ImageBuild(ctx, id)
		if err != nil && !errors.Is(err, store.ErrNotFound) {
			return err
		}
		if err != nil || b.ImageID != v.ImageID {
			return nil
		}
		v.Source, v.BuildID, v.Revision, v.CreatedBy = store.VersionFromBuild, &b.ID, &b.Revision, &b.StartedBy
		return nil
	}
	if raw, ok := strings.CutPrefix(actor, "user:"); ok {
		if id, err := uuid.Parse(raw); err == nil {
			v.CreatedBy = &id
		}
	}
	return nil
}

func (h *registryEvents) reject(ctx context.Context, img store.Image, repo, digest, msg string) error {
	if err := h.store.SetImagePushError(ctx, img.ID, msg); err != nil {
		return err
	}
	if err := h.manifests.DeleteManifest(ctx, repo, digest); err != nil {
		return err
	}
	// The rejected push may have moved one of the hub's tags in the registry.
	h.tags.Kick(img.ID)
	return nil
}

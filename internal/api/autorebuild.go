package api

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"github.com/google/uuid"

	"github.com/asterism-dev/observatory/internal/authz"
	"github.com/asterism-dev/observatory/internal/builds"
	"github.com/asterism-dev/observatory/internal/registry"
	"github.com/asterism-dev/observatory/internal/store"
)

const (
	autoCheckEvery  = time.Hour
	autoBuildEvery  = 6 * time.Hour
	autoResolveWait = 30 * time.Second
	autoErrorMax    = 200
	// autoCheckPrefix marks errors of the base check, so the next good check clears them but not a failed build's.
	autoCheckPrefix = "base image check failed: "
)

var autoPeriods = map[string]time.Duration{store.AutoDaily: 24 * time.Hour, store.AutoWeekly: 7 * 24 * time.Hour}

// AutoRebuilds starts builds of images on their schedule and when a base image of their latest version changed.
type AutoRebuilds struct {
	store    *store.Store
	z        *authz.Service
	builds   *builds.Scheduler
	hubHosts []string
	resolve  func(ctx context.Context, ref name.Reference) (string, error)
	resolved map[string]resolution // per pass, so a base shared by many images is looked up once

	Now func() time.Time
}

type resolution struct {
	digest string
	err    error
}

// NewAutoRebuilds takes the hub URLs that FROM lines may name observatory images by; those are looked up in the database.
func NewAutoRebuilds(s *store.Store, z *authz.Service, b *builds.Scheduler, hubURLs ...string) *AutoRebuilds {
	a := &AutoRebuilds{store: s, z: z, builds: b, Now: time.Now,
		resolve: func(ctx context.Context, ref name.Reference) (string, error) {
			return registry.PlatformDigest(ctx, ref, remote.WithTransport(registry.PublicTransport()))
		}}
	for _, u := range hubURLs {
		if p, err := url.Parse(u); err == nil && p.Host != "" {
			a.hubHosts = append(a.hubHosts, p.Host)
		}
	}
	return a
}

func (a *AutoRebuilds) Run(ctx context.Context, every time.Duration) {
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		if err := a.Tick(ctx); err != nil && ctx.Err() == nil {
			slog.Error("automatic rebuilds", "err", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// Tick looks at every image with automatic rebuilds on, one org after the other.
func (a *AutoRebuilds) Tick(ctx context.Context) error {
	imgs, err := a.store.AutoImages(ctx)
	if err != nil {
		return err
	}
	a.resolved = map[string]resolution{}
	var errs []error
	for _, img := range imgs {
		if err := a.image(ctx, img); err != nil {
			errs = append(errs, fmt.Errorf("image %s: %w", img.ID, err))
		}
	}
	return errors.Join(errs...)
}

func (a *AutoRebuilds) image(ctx context.Context, img store.Image) error {
	now := a.Now()
	active, lastAutomatic, lastSuccess, err := a.store.AutoBuildState(ctx, img.ID)
	// An approval answers the last failure, so builds before it do not count against the limit.
	sinceApproval := lastAutomatic != nil && (img.AutoApprovedAt == nil || lastAutomatic.After(*img.AutoApprovedAt))
	if err != nil || active || sinceApproval && now.Sub(*lastAutomatic) < autoBuildEvery {
		return err
	}
	due := false
	if period, ok := autoPeriods[img.AutoSchedule]; ok {
		due = lastSuccess == nil || now.Sub(*lastSuccess) >= period
	}
	if !due && img.AutoOnBaseChange && (img.AutoCheckedAt == nil || now.Sub(*img.AutoCheckedAt) >= autoCheckEvery) {
		changed, err := a.basesChanged(ctx, img)
		if err != nil {
			// The registry may be down; nothing is built and the next pass checks again.
			return a.store.SetImageAutoCheckError(ctx, img.ID, autoCheckPrefix+trimError(err), autoCheckPrefix)
		}
		if err := a.store.SetImageAutoChecked(ctx, img.ID, now, autoCheckPrefix); err != nil {
			return err
		}
		due = changed
	}
	if !due {
		return nil
	}
	return a.start(ctx, img)
}

// basesChanged compares the bases latest was built from with what they resolve to now; a pushed latest has none.
func (a *AutoRebuilds) basesChanged(ctx context.Context, img store.Image) (bool, error) {
	digest, err := a.store.ResolveImageTag(ctx, img.ID, latestTag)
	if errors.Is(err, store.ErrNotFound) {
		return false, nil
	} else if err != nil {
		return false, err
	}
	v, err := a.store.ImageVersion(ctx, img.ID, digest)
	if err != nil {
		return false, err
	}
	for _, b := range v.BaseDigests {
		d, err := a.resolveBase(ctx, b.Ref)
		if err != nil {
			return false, err
		}
		if d != b.Digest {
			return true, nil
		}
	}
	return false, nil
}

// resolveBase follows the build agent's rules: the linux/amd64 manifest digest, observatory images from the database.
func (a *AutoRebuilds) resolveBase(ctx context.Context, s string) (string, error) {
	r, ok := a.resolved[s]
	if !ok {
		r.digest, r.err = a.lookup(ctx, s)
		a.resolved[s] = r
	}
	return r.digest, r.err
}

func (a *AutoRebuilds) lookup(ctx context.Context, s string) (string, error) {
	ref, err := name.ParseReference(s)
	if err != nil {
		return "", err
	}
	if slices.Contains(a.hubHosts, ref.Context().RegistryStr()) {
		return a.hubImageDigest(ctx, ref)
	}
	ctx, cancel := context.WithTimeout(ctx, autoResolveWait)
	defer cancel()
	return a.resolve(ctx, ref)
}

// trimError keeps registry response bodies out of what members see on the image.
func trimError(err error) string {
	msg, _, _ := strings.Cut(err.Error(), "\n")
	if r := []rune(msg); len(r) > autoErrorMax {
		return string(r[:autoErrorMax]) + "…"
	}
	return msg
}

func (a *AutoRebuilds) hubImageDigest(ctx context.Context, ref name.Reference) (string, error) {
	repo, ok := strings.CutPrefix(ref.Context().RepositoryStr(), "i/")
	id, err := uuid.Parse(repo)
	if !ok || err != nil {
		return "", fmt.Errorf("%s is not an observatory image", ref)
	}
	digest, err := a.store.ResolveImageTag(ctx, id, ref.Identifier())
	if errors.Is(err, store.ErrNotFound) {
		return "", fmt.Errorf("%s does not exist", ref)
	} else if err != nil {
		return "", err
	}
	v, err := a.store.ImageVersion(ctx, id, digest)
	if err != nil {
		return "", err
	}
	if v.PlatformDigest != "" {
		return v.PlatformDigest, nil
	}
	return v.Digest, nil
}

// start queues an automatic build as the user who set it up; when that user may not build, the build is recorded as failed.
func (a *AutoRebuilds) start(ctx context.Context, img store.Image) error {
	if img.AutoBy == nil {
		return a.store.SetImageAutoResult(ctx, img.ID, "automatic rebuilds have no user to run as, save the settings again", a.Now())
	}
	rev, err := a.store.RecipeRevision(ctx, img.ID, *img.RecipeRevision)
	if err != nil {
		return err
	}
	msg, err := a.problem(ctx, img, rev, *img.AutoBy)
	if err != nil {
		return err
	}
	b := store.ImageBuild{ImageID: img.ID, Revision: rev.Number, StartedBy: *img.AutoBy, Automatic: true}
	if msg != "" {
		_, err = a.store.CreateFailedImageBuild(ctx, b, msg, a.Now())
	} else {
		_, err = a.store.CreateImageBuild(ctx, b)
	}
	if errors.Is(err, store.ErrBuildRunning) {
		return nil
	} else if err != nil {
		return err
	}
	if msg == "" {
		a.builds.Kick()
	}
	return nil
}

// problem applies the checks of a manual build to user and returns why the build may not run.
func (a *AutoRebuilds) problem(ctx context.Context, img store.Image, rev store.RecipeRevision, user uuid.UUID) (string, error) {
	msg, err := a.mayBuild(ctx, img, rev, user)
	if err != nil {
		return "", err
	}
	if msg != "" {
		return fmt.Sprintf("automatic rebuilds run as %s: %s", a.userName(ctx, user), msg), nil
	}
	// Otherwise someone with only image:edit could change the Dockerfile and have it run with user's references.
	if needsApproval(img, rev.CreatedBy) {
		if msg, err := a.mayBuild(ctx, img, rev, rev.CreatedBy); err != nil {
			return "", err
		} else if msg != "" {
			return fmt.Sprintf("recipe revision %d by %s: %s — approve the revision for automatic builds after review", rev.Number, a.userName(ctx, rev.CreatedBy), msg), nil
		}
	}
	if over, err := overImageStorageLimit(ctx, a.store, img.OrgID); err != nil || over {
		return "the org is over its image storage limit", err
	}
	return "", nil
}

func (a *AutoRebuilds) mayBuild(ctx context.Context, img store.Image, rev store.RecipeRevision, user uuid.UUID) (string, error) {
	sub := authz.UserSubject(user)
	if !a.z.Allowed(sub, img.OrgID, imageObject(img.Scope, img.OwnerID), "image:build") {
		return "no right to build the image", nil
	}
	return refsUsable(ctx, a.store, img.OrgID, rev.Refs, func(obj, act string) bool { return a.z.Allowed(sub, img.OrgID, obj, act) })
}

func (a *AutoRebuilds) userName(ctx context.Context, id uuid.UUID) string {
	if u, err := a.store.User(ctx, id); err == nil {
		return u.Name
	}
	return id.String()
}

// needsApproval reports whether the current revision, by author, is newer than what was approved and not by the user builds run as.
func needsApproval(img store.Image, author uuid.UUID) bool {
	return img.RecipeRevision != nil && (img.AutoBy == nil || author != *img.AutoBy) && (img.AutoRevision == nil || *img.RecipeRevision > *img.AutoRevision)
}

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
	resolved map[string]string // per pass, so a base shared by many images is looked up once

	Now func() time.Time
}

// NewAutoRebuilds takes the hub URLs that FROM lines may name observatory images by; those are looked up in the database.
func NewAutoRebuilds(s *store.Store, z *authz.Service, b *builds.Scheduler, hubURLs ...string) *AutoRebuilds {
	a := &AutoRebuilds{store: s, z: z, builds: b, Now: time.Now,
		resolve: func(ctx context.Context, ref name.Reference) (string, error) {
			return registry.PlatformDigest(ctx, ref)
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
	a.resolved = map[string]string{}
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
	bs, err := a.store.ImageBuilds(ctx, img.ID)
	if err != nil {
		return err
	}
	var lastSuccess *time.Time
	for _, b := range bs {
		if b.State == store.BuildQueued || b.State == store.BuildRunning || b.Automatic && now.Sub(b.QueuedAt) < autoBuildEvery {
			return nil
		}
		if b.State == store.BuildSucceeded && lastSuccess == nil {
			lastSuccess = b.FinishedAt
		}
	}
	due := false
	if period, ok := autoPeriods[img.AutoSchedule]; ok {
		due = lastSuccess == nil || now.Sub(*lastSuccess) >= period
	}
	if !due && img.AutoOnBaseChange && (img.AutoCheckedAt == nil || now.Sub(*img.AutoCheckedAt) >= autoCheckEvery) {
		changed, err := a.basesChanged(ctx, img)
		if err != nil {
			// The registry may be down; nothing is built and the next pass checks again.
			return a.store.SetImageAutoResult(ctx, img.ID, autoCheckPrefix+err.Error(), now)
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
	if d, ok := a.resolved[s]; ok {
		return d, nil
	}
	ref, err := name.ParseReference(s)
	if err != nil {
		return "", err
	}
	var d string
	if slices.Contains(a.hubHosts, ref.Context().RegistryStr()) {
		d, err = a.hubImageDigest(ctx, ref)
	} else {
		rctx, cancel := context.WithTimeout(ctx, autoResolveWait)
		d, err = a.resolve(rctx, ref)
		cancel()
	}
	if err != nil {
		return "", err
	}
	a.resolved[s] = d
	return d, nil
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
	b, err := a.store.CreateImageBuild(ctx, store.ImageBuild{ImageID: img.ID, Revision: rev.Number, StartedBy: *img.AutoBy, Automatic: true})
	if errors.Is(err, store.ErrBuildRunning) {
		return nil
	} else if err != nil {
		return err
	}
	if msg != "" {
		_, err := a.builds.Finish(ctx, b.ID, store.BuildFailed, "", msg)
		return err
	}
	a.builds.Kick()
	return nil
}

// problem applies the checks of a manual build to user and returns why the build may not run.
func (a *AutoRebuilds) problem(ctx context.Context, img store.Image, rev store.RecipeRevision, user uuid.UUID) (string, error) {
	u, err := a.store.User(ctx, user)
	if err != nil {
		return "", err
	}
	sub := authz.UserSubject(user)
	msg := ""
	if !a.z.Allowed(sub, img.OrgID, imageObject(img.Scope, img.OwnerID), "image:build") {
		msg = "no right to build the image"
	} else if msg, err = refsUsable(ctx, a.store, img.OrgID, rev.Refs, func(obj, act string) bool { return a.z.Allowed(sub, img.OrgID, obj, act) }); err != nil {
		return "", err
	}
	if msg != "" {
		return fmt.Sprintf("automatic rebuilds run as %s: %s", u.Name, msg), nil
	}
	if over, err := overImageStorageLimit(ctx, a.store, img.OrgID); err != nil || over {
		return "the org is over its image storage limit", err
	}
	return "", nil
}

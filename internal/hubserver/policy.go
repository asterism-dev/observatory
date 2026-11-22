package hubserver

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"slices"
	"sync/atomic"
	"time"

	"github.com/google/uuid"

	"github.com/asterism-dev/observatory/gen/go/observatory/iam/v1/iamv1connect"
	"github.com/asterism-dev/observatory/internal/authz"
	platformauthz "github.com/asterism-dev/observatory/internal/platform/authz"
	"github.com/asterism-dev/observatory/internal/platform/catalog"
	"github.com/asterism-dev/observatory/internal/store"
)

// firstPolicyWait bounds how long the hub starts without iam's first policy snapshot.
var firstPolicyWait = 60 * time.Second

// hubPolicy is the hub's enforcer: iam's streamed policy plus the hub's owner refs.
type hubPolicy struct {
	*authz.Service
	w *platformauthz.Watcher
}

// WaitRevision returns once the enforcer includes iam's revision rev; the reload makes that hold without waiting for OnChange.
func (h hubPolicy) WaitRevision(ctx context.Context, rev uint64) error {
	if err := h.w.WaitRevision(ctx, rev); err != nil {
		return err
	}
	return h.Reload(ctx)
}

// watchPolicy registers the hub's catalog and returns the hub's enforcer over w, reloaded on every stream message; it fails unless a snapshot with that catalog arrives within firstPolicyWait.
func watchPolicy(ctx context.Context, w *platformauthz.Watcher, s *store.Store, cat authz.Catalog, catalogs iamv1connect.CatalogInternalServiceClient) (hubPolicy, error) {
	go w.Run(ctx)
	wctx, cancel := context.WithTimeout(ctx, firstPolicyWait)
	defer cancel()
	digest, err := catalog.Register(wctx, catalogs, cat)
	if err != nil {
		if wctx.Err() != nil {
			return hubPolicy{}, fmt.Errorf("no policy from iam within %v: %w", firstPolicyWait, err)
		}
		return hubPolicy{}, fmt.Errorf("register hub catalog: %w", err)
	}
	w.Require(cat.Service, digest)
	for {
		// Taken before the check so a snapshot in between is not missed; revisions can go backwards after an iam restart.
		changed := w.Changed()
		if _, ok := w.Current(); ok {
			break
		}
		select {
		case <-wctx.Done():
			return hubPolicy{}, fmt.Errorf("no policy with the hub catalog from iam within %v: %w", firstPolicyWait, wctx.Err())
		case <-changed:
		}
	}
	z, err := authz.NewService(ctx, func(ctx context.Context) (authz.Policy, error) {
		p, _, ok := w.Policy()
		if !ok {
			return p, errors.New("no policy from iam")
		}
		var err error
		p.Owned, err = s.OwnerRefs(ctx)
		return p, err
	})
	if err != nil {
		return hubPolicy{}, err
	}
	// A failed reload would leave a revoked grant in place, so org routes refuse until one succeeds.
	var behind atomic.Bool
	z.WithFreshness(func() bool {
		_, ok := w.Current()
		return ok && !behind.Load() && !z.Behind()
	})
	reload := func(ctx context.Context) error {
		for wait := time.Second; ; wait = min(2*wait, 30*time.Second) {
			err := z.Reload(ctx)
			if err == nil {
				behind.Store(false)
				return nil
			}
			behind.Store(true)
			if ctx.Err() != nil {
				return err
			}
			slog.Error("hub policy reload failed, org routes refuse until it succeeds", "err", err, "retry_in", wait)
			select {
			case <-ctx.Done():
				return err
			case <-time.After(wait):
			}
		}
	}
	w.OnChange(func() { _ = reload(ctx) })
	// A snapshot that arrived before OnChange was set must not be missed; this reload shares the start budget.
	if err := reload(wctx); err != nil {
		return hubPolicy{}, fmt.Errorf("hub policy: %w", err)
	}
	return hubPolicy{z, w}, nil
}

// changedOrgs lists the orgs whose memberships, teams, team members, roles or bindings differ between old and new.
func changedOrgs(old, new authz.Policy) []uuid.UUID {
	a, b := orgFacts(old), orgFacts(new)
	var out []uuid.UUID
	for org := range maps.Keys(a) {
		if !slices.Equal(a[org], b[org]) {
			out = append(out, org)
		}
	}
	for org := range maps.Keys(b) {
		if _, ok := a[org]; !ok {
			out = append(out, org)
		}
	}
	slices.SortFunc(out, func(x, y uuid.UUID) int { return slices.Compare(x[:], y[:]) })
	return out
}

// orgFacts renders each org's IAM entries as sorted strings, so equal sets compare equal whatever their order.
func orgFacts(p authz.Policy) map[uuid.UUID][]string {
	m := map[uuid.UUID][]string{}
	add := func(org uuid.UUID, v any) { m[org] = append(m[org], fmt.Sprintf("%T%+v", v, v)) }
	for _, r := range p.Memberships {
		add(r.OrgID, r)
	}
	for _, t := range p.TeamMembers {
		add(t.OrgID, t)
	}
	for _, t := range p.Teams {
		add(t.OrgID, t)
	}
	for _, r := range p.Roles {
		add(r.OrgID, r)
	}
	for _, b := range p.Bindings {
		add(b.OrgID, b)
	}
	for _, facts := range m {
		slices.Sort(facts)
	}
	return m
}

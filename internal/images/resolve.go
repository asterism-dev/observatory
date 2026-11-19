package images

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/google/uuid"

	"github.com/asterism-dev/observatory/internal/authz"
	"github.com/asterism-dev/observatory/internal/store"
)

const (
	SourceExplicit    = "explicit"
	SourceUserDefault = "user-default"
	SourceTeamDefault = "team-default"
	SourceOrgDefault  = "org-default"
	SourcePlatform    = "platform"

	platformRef = "observatory/standard"
	latestTag   = "latest"
)

var ErrAmbiguous = errors.New("image name is ambiguous")

type Checker interface {
	Allowed(sub string, org uuid.UUID, obj, act string) bool
}

type Owner struct{ UserID, TeamID *uuid.UUID }

type Resolved struct {
	Image  *store.Image
	Digest string
	Tag    string
	Source string
}

type Resolver struct {
	Store *store.Store
	Authz Checker
}

func (r Resolver) Resolve(ctx context.Context, org, caller uuid.UUID, owner Owner, ref string) (Resolved, error) {
	if ref == "" {
		return r.cascade(ctx, org, owner)
	}
	if ref == platformRef || strings.HasPrefix(ref, platformRef+":") {
		return Resolved{Tag: strings.TrimPrefix(strings.TrimPrefix(ref, platformRef), ":"), Source: SourcePlatform}, nil
	}
	p, err := ParseRef(ref)
	if err != nil {
		return Resolved{}, err
	}
	img, err := r.find(ctx, org, caller, owner, p)
	if err != nil {
		return Resolved{}, err
	}
	res := Resolved{Image: &img, Tag: p.Tag, Digest: p.Digest, Source: SourceExplicit}
	if p.Digest != "" {
		return res, nil
	}
	if res.Tag == "" {
		res.Tag = latestTag
	}
	if res.Digest, err = r.Store.ResolveImageTag(ctx, img.ID, res.Tag); errors.Is(err, store.ErrNotFound) {
		return Resolved{}, fmt.Errorf("tag %q not found", res.Tag)
	} else if err != nil {
		return Resolved{}, err
	}
	return res, nil
}

func (r Resolver) find(ctx context.Context, org, caller uuid.UUID, owner Owner, p Ref) (store.Image, error) {
	switch p.Qualifier {
	case "me":
		return r.lookup(ctx, org, caller, store.EnvScopeUser, &caller, p.Name)
	case "org":
		return r.lookup(ctx, org, caller, store.EnvScopeOrg, nil, p.Name)
	case "":
		return r.findBare(ctx, org, caller, owner, p.Name)
	}
	teams, err := r.Store.Teams(ctx, org)
	if err != nil {
		return store.Image{}, err
	}
	for _, t := range teams {
		if t.Name == p.Qualifier {
			return r.lookup(ctx, org, caller, store.EnvScopeTeam, &t.ID, p.Name)
		}
	}
	return store.Image{}, fmt.Errorf("image %q: %w", p.Qualifier+"/"+p.Name, store.ErrNotFound)
}

func (r Resolver) findBare(ctx context.Context, org, caller uuid.UUID, owner Owner, name string) (store.Image, error) {
	var found []store.Image
	var forms []string
	add := func(scope store.EnvScope, id *uuid.UUID, form string) error {
		img, err := r.lookup(ctx, org, caller, scope, id, name)
		if errors.Is(err, store.ErrNotFound) {
			return nil
		}
		if err != nil {
			return err
		}
		found, forms = append(found, img), append(forms, form+"/"+name)
		return nil
	}
	if err := add(store.EnvScopeUser, &caller, "me"); err != nil {
		return store.Image{}, err
	}
	if owner.TeamID != nil {
		t, err := r.Store.Team(ctx, org, *owner.TeamID)
		if err != nil {
			return store.Image{}, err
		}
		if err := add(store.EnvScopeTeam, owner.TeamID, t.Name); err != nil {
			return store.Image{}, err
		}
	}
	if err := add(store.EnvScopeOrg, nil, "org"); err != nil {
		return store.Image{}, err
	}
	switch len(found) {
	case 0:
		return store.Image{}, fmt.Errorf("image %q: %w", name, store.ErrNotFound)
	case 1:
		return found[0], nil
	}
	return store.Image{}, fmt.Errorf("%w: use one of %s", ErrAmbiguous, strings.Join(forms, ", "))
}

// An image the caller may not use is reported as not found so private names do not leak.
func (r Resolver) lookup(ctx context.Context, org, caller uuid.UUID, scope store.EnvScope, id *uuid.UUID, name string) (store.Image, error) {
	img, err := r.Store.ImageByName(ctx, org, scope, id, name)
	if err != nil {
		return store.Image{}, err
	}
	obj := authz.ObjImages
	switch scope {
	case store.EnvScopeTeam:
		obj = authz.TeamImagesObject(*id)
	case store.EnvScopeUser:
		obj = authz.UserImagesObject(*id)
	}
	if !r.Authz.Allowed(authz.UserSubject(caller), org, obj, "image:use") {
		return store.Image{}, store.ErrNotFound
	}
	return img, nil
}

func (r Resolver) cascade(ctx context.Context, org uuid.UUID, owner Owner) (Resolved, error) {
	defaults, err := r.Store.ImageDefaults(ctx, org)
	if err != nil {
		return Resolved{}, err
	}
	steps := []struct {
		scope  store.EnvScope
		id     *uuid.UUID
		source string
	}{{store.EnvScopeUser, owner.UserID, SourceUserDefault}, {store.EnvScopeTeam, owner.TeamID, SourceTeamDefault}, {store.EnvScopeOrg, nil, SourceOrgDefault}}
	for _, st := range steps {
		if st.scope != store.EnvScopeOrg && st.id == nil {
			continue
		}
		for _, d := range defaults {
			if d.Scope != st.scope || !sameID(d.OwnerID, st.id) {
				continue
			}
			img, err := r.Store.Image(ctx, org, d.ImageID)
			if err != nil {
				return Resolved{}, err
			}
			res := Resolved{Image: &img, Tag: d.Tag, Digest: d.Digest, Source: st.source}
			if res.Digest == "" && res.Tag != "" {
				if res.Digest, err = r.Store.ResolveImageTag(ctx, img.ID, res.Tag); err != nil {
					return Resolved{}, fmt.Errorf("default tag %q: %w", res.Tag, err)
				}
			}
			return res, nil
		}
	}
	return Resolved{Source: SourcePlatform}, nil
}

func sameID(a, b *uuid.UUID) bool { return (a == nil) == (b == nil) && (a == nil || *a == *b) }

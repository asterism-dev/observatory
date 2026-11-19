package images_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/asterism-dev/observatory/internal/authz"
	"github.com/asterism-dev/observatory/internal/db/dbtest"
	"github.com/asterism-dev/observatory/internal/images"
	"github.com/asterism-dev/observatory/internal/store"
)

const digest = "sha256:0000000000000000000000000000000000000000000000000000000000000001"

func TestParseRef(t *testing.T) {
	ok := map[string]images.Ref{
		"tools":                {Name: "tools"},
		"tools:v1":             {Name: "tools", Tag: "v1"},
		"tools@" + digest:      {Name: "tools", Digest: digest},
		"me/tools":             {Qualifier: "me", Name: "tools"},
		"core/tools:v1":        {Qualifier: "core", Name: "tools", Tag: "v1"},
		"org/tools@" + digest:  {Qualifier: "org", Name: "tools", Digest: digest},
		"observatory/standard": {Qualifier: "observatory", Name: "standard"},
	}
	for in, want := range ok {
		if got, err := images.ParseRef(in); err != nil || got != want {
			t.Errorf("%q: %+v %v", in, got, err)
		}
	}
	for _, in := range []string{"", ":v1", "tools@sha256:abc", "tools:", "/tools", "a/b/c", "tools:v1@" + digest} {
		if _, err := images.ParseRef(in); err == nil {
			t.Errorf("%q: expected error", in)
		}
	}
}

type grants map[string]bool

func (g grants) Allowed(_ string, _ uuid.UUID, obj, act string) bool { return g[obj+" "+act] }

type fixture struct {
	s          *store.Store
	org        store.Org
	user, peer uuid.UUID
	team       store.Team
	r          images.Resolver
}

func newFixture(t *testing.T, g grants) fixture {
	t.Helper()
	ctx := context.Background()
	s := store.New(dbtest.New(t))
	login := func(sub string) uuid.UUID {
		res, err := s.Login(ctx, store.Identity{Subject: sub, Email: sub + "@x.io", Name: sub})
		if err != nil {
			t.Fatal(err)
		}
		return res.User.ID
	}
	f := fixture{s: s, user: login("u"), peer: login("p")}
	var err error
	if f.org, err = s.CreateOrg(ctx, "O", f.user); err != nil {
		t.Fatal(err)
	}
	tok, _, err := s.CreateInvite(ctx, f.org.ID, store.RoleMember, f.user, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err = s.AcceptInvite(ctx, tok, f.peer); err != nil {
		t.Fatal(err)
	}
	if f.team, err = s.CreateTeam(ctx, f.org.ID, "core"); err != nil {
		t.Fatal(err)
	}
	g[authz.ObjImages+" image:use"] = true
	g[authz.UserImagesObject(f.user)+" image:use"] = true
	g[authz.TeamImagesObject(f.team.ID)+" image:use"] = true
	f.r = images.Resolver{Store: s, Authz: g}
	return f
}

func (f fixture) image(t *testing.T, scope store.EnvScope, owner *uuid.UUID, name string) store.Image {
	t.Helper()
	img, err := f.s.CreateImage(context.Background(), store.Image{OrgID: f.org.ID, Scope: scope, OwnerID: owner, Name: name, CreatedBy: f.user})
	if err != nil {
		t.Fatal(err)
	}
	return img
}

func (f fixture) setDefault(t *testing.T, scope store.EnvScope, owner *uuid.UUID, img store.Image, tag string) {
	t.Helper()
	if err := f.s.SetImageDefault(context.Background(), store.ImageDefault{OrgID: f.org.ID, Scope: scope, OwnerID: owner, ImageID: img.ID, Tag: tag}); err != nil {
		t.Fatal(err)
	}
}

func (f fixture) tag(t *testing.T, img store.Image, tag string) {
	t.Helper()
	if err := f.s.AddImageVersion(context.Background(), store.ImageVersion{ImageID: img.ID, Digest: digest, Source: store.VersionFromPush}); err != nil {
		t.Fatal(err)
	}
	if err := f.s.SetImageTag(context.Background(), img.ID, tag, digest); err != nil {
		t.Fatal(err)
	}
}

func TestResolveBareNameAmbiguous(t *testing.T) {
	f := newFixture(t, grants{})
	priv := f.image(t, store.EnvScopeUser, &f.user, "tools")
	org := f.image(t, store.EnvScopeOrg, nil, "tools")
	f.tag(t, priv, "latest")
	f.tag(t, org, "latest")
	_, err := f.r.Resolve(context.Background(), f.org.ID, f.user, images.Owner{UserID: &f.user}, "tools")
	if !errors.Is(err, images.ErrAmbiguous) || !strings.Contains(err.Error(), "me/tools") || !strings.Contains(err.Error(), "org/tools") {
		t.Fatalf("got %v", err)
	}
	got, err := f.r.Resolve(context.Background(), f.org.ID, f.user, images.Owner{UserID: &f.user}, "me/tools:latest")
	if err != nil || got.Image.ID != priv.ID || got.Digest != digest || got.Source != images.SourceExplicit {
		t.Fatalf("qualified: %+v %v", got, err)
	}
}

func TestResolveTags(t *testing.T) {
	f := newFixture(t, grants{})
	img := f.image(t, store.EnvScopeOrg, nil, "tools")
	owner := images.Owner{UserID: &f.user}
	if _, err := f.r.Resolve(context.Background(), f.org.ID, f.user, owner, "tools:v9"); err == nil || !strings.Contains(err.Error(), `tag "v9" not found`) {
		t.Fatalf("missing tag: %v", err)
	}
	got, err := f.r.Resolve(context.Background(), f.org.ID, f.user, owner, "tools@"+digest)
	if err != nil || got.Digest != digest || got.Image.ID != img.ID {
		t.Fatalf("digest: %+v %v", got, err)
	}
}

func TestResolveCascadePersonalVM(t *testing.T) {
	f := newFixture(t, grants{})
	ud := f.image(t, store.EnvScopeUser, &f.user, "mine")
	td := f.image(t, store.EnvScopeTeam, &f.team.ID, "teams")
	od := f.image(t, store.EnvScopeOrg, nil, "all")
	for _, i := range []store.Image{ud, td, od} {
		f.tag(t, i, "latest")
	}
	f.setDefault(t, store.EnvScopeOrg, nil, od, "latest")
	f.setDefault(t, store.EnvScopeTeam, &f.team.ID, td, "latest")
	got, err := f.r.Resolve(context.Background(), f.org.ID, f.user, images.Owner{UserID: &f.user}, "")
	if err != nil || got.Source != images.SourceOrgDefault || got.Image.ID != od.ID {
		t.Fatalf("org: %+v %v", got, err)
	}
	f.setDefault(t, store.EnvScopeUser, &f.user, ud, "latest")
	got, err = f.r.Resolve(context.Background(), f.org.ID, f.user, images.Owner{UserID: &f.user}, "")
	if err != nil || got.Source != images.SourceUserDefault || got.Image.ID != ud.ID || got.Digest != digest {
		t.Fatalf("user: %+v %v", got, err)
	}
}

func TestResolveCascadeTeamVM(t *testing.T) {
	f := newFixture(t, grants{})
	ud := f.image(t, store.EnvScopeUser, &f.user, "mine")
	td := f.image(t, store.EnvScopeTeam, &f.team.ID, "teams")
	od := f.image(t, store.EnvScopeOrg, nil, "all")
	for _, i := range []store.Image{ud, td, od} {
		f.tag(t, i, "latest")
	}
	f.setDefault(t, store.EnvScopeOrg, nil, od, "latest")
	f.setDefault(t, store.EnvScopeUser, &f.user, ud, "latest")
	owner := images.Owner{TeamID: &f.team.ID}
	got, err := f.r.Resolve(context.Background(), f.org.ID, f.user, owner, "")
	if err != nil || got.Source != images.SourceOrgDefault {
		t.Fatalf("org: %+v %v", got, err)
	}
	f.setDefault(t, store.EnvScopeTeam, &f.team.ID, td, "latest")
	got, err = f.r.Resolve(context.Background(), f.org.ID, f.user, owner, "")
	if err != nil || got.Source != images.SourceTeamDefault || got.Image.ID != td.ID {
		t.Fatalf("team: %+v %v", got, err)
	}
}

func TestResolvePlatformFallback(t *testing.T) {
	f := newFixture(t, grants{})
	owner := images.Owner{UserID: &f.user}
	for _, ref := range []string{"", "observatory/standard"} {
		got, err := f.r.Resolve(context.Background(), f.org.ID, f.user, owner, ref)
		if err != nil || got.Image != nil || got.Digest != "" || got.Source != images.SourcePlatform {
			t.Fatalf("%q: %+v %v", ref, got, err)
		}
	}
}

func TestResolveNoUseRight(t *testing.T) {
	f := newFixture(t, grants{})
	f.image(t, store.EnvScopeUser, &f.peer, "secret")
	for _, ref := range []string{"secret", "me/secret", "secret:latest"} {
		if _, err := f.r.Resolve(context.Background(), f.org.ID, f.user, images.Owner{UserID: &f.user}, ref); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("%q: %v", ref, err)
		}
	}
	team := f.image(t, store.EnvScopeTeam, &f.team.ID, "internal")
	f.tag(t, team, "latest")
	f.r.Authz = grants{authz.ObjImages + " image:use": true}
	if _, err := f.r.Resolve(context.Background(), f.org.ID, f.user, images.Owner{TeamID: &f.team.ID}, "core/internal"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("team without use: %v", err)
	}
}

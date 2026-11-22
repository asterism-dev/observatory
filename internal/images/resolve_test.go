package images_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/asterism-dev/observatory/internal/authz"
	"github.com/asterism-dev/observatory/internal/db/dbtest"
	"github.com/asterism-dev/observatory/internal/images"
	"github.com/asterism-dev/observatory/internal/platform/iamtest"
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
	org        uuid.UUID
	user, peer uuid.UUID
	team       uuid.UUID
	r          images.Resolver
}

func newFixture(t *testing.T, g grants) fixture {
	t.Helper()
	s := store.New(dbtest.New(t))
	iam := iamtest.New(t)
	f := fixture{s: s, user: iam.AddUser("u").ID, peer: iam.AddUser("p").ID}
	f.org = iam.CreateOrg(f.user, "O")
	iam.AddMember(f.org, f.peer, "member")
	f.team = iam.AddTeam(f.org, "core")
	g[authz.ObjImages+" artifacts.images.use"] = true
	g[authz.UserImagesObject(f.user)+" artifacts.images.use"] = true
	g[authz.TeamImagesObject(f.team)+" artifacts.images.use"] = true
	f.r = images.Resolver{Store: s, IAM: iam.Client(), Authz: g}
	return f
}

func (f fixture) image(t *testing.T, scope store.EnvScope, owner *uuid.UUID, name string) store.Image {
	t.Helper()
	img, err := f.s.CreateImage(context.Background(), store.Image{OrgID: f.org, Scope: scope, OwnerID: owner, Name: name, CreatedBy: f.user})
	if err != nil {
		t.Fatal(err)
	}
	return img
}

func (f fixture) setDefault(t *testing.T, scope store.EnvScope, owner *uuid.UUID, img store.Image, tag string) {
	t.Helper()
	if err := f.s.SetImageDefault(context.Background(), store.ImageDefault{OrgID: f.org, Scope: scope, OwnerID: owner, ImageID: img.ID, Tag: tag}); err != nil {
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
	_, err := f.r.Resolve(context.Background(), f.org, f.user, images.Owner{UserID: &f.user}, "tools")
	if !errors.Is(err, images.ErrAmbiguous) || !strings.Contains(err.Error(), "me/tools") || !strings.Contains(err.Error(), "org/tools") {
		t.Fatalf("got %v", err)
	}
	got, err := f.r.Resolve(context.Background(), f.org, f.user, images.Owner{UserID: &f.user}, "me/tools:latest")
	if err != nil || got.Image.ID != priv.ID || got.Digest != digest || got.Source != images.SourceExplicit {
		t.Fatalf("qualified: %+v %v", got, err)
	}
}

func TestResolveTags(t *testing.T) {
	f := newFixture(t, grants{})
	img := f.image(t, store.EnvScopeOrg, nil, "tools")
	owner := images.Owner{UserID: &f.user}
	if _, err := f.r.Resolve(context.Background(), f.org, f.user, owner, "tools:v9"); err == nil || !strings.Contains(err.Error(), `tag "v9": not found`) {
		t.Fatalf("missing tag: %v", err)
	}
	if _, err := f.r.Resolve(context.Background(), f.org, f.user, owner, "tools@"+digest); err == nil || !strings.Contains(err.Error(), "not found") {
		t.Fatalf("unknown digest: %v", err)
	}
	f.tag(t, img, "latest")
	got, err := f.r.Resolve(context.Background(), f.org, f.user, owner, "tools@"+digest)
	if err != nil || got.Digest != digest || got.Image.ID != img.ID {
		t.Fatalf("digest: %+v %v", got, err)
	}
}

func TestResolveCascadePersonalVM(t *testing.T) {
	f := newFixture(t, grants{})
	ud := f.image(t, store.EnvScopeUser, &f.user, "mine")
	td := f.image(t, store.EnvScopeTeam, &f.team, "teams")
	od := f.image(t, store.EnvScopeOrg, nil, "all")
	for _, i := range []store.Image{ud, td, od} {
		f.tag(t, i, "latest")
	}
	f.setDefault(t, store.EnvScopeOrg, nil, od, "latest")
	f.setDefault(t, store.EnvScopeTeam, &f.team, td, "latest")
	got, err := f.r.Resolve(context.Background(), f.org, f.user, images.Owner{UserID: &f.user}, "")
	if err != nil || got.Source != images.SourceOrgDefault || got.Image.ID != od.ID {
		t.Fatalf("org: %+v %v", got, err)
	}
	f.setDefault(t, store.EnvScopeUser, &f.user, ud, "latest")
	got, err = f.r.Resolve(context.Background(), f.org, f.user, images.Owner{UserID: &f.user}, "")
	if err != nil || got.Source != images.SourceUserDefault || got.Image.ID != ud.ID || got.Digest != digest {
		t.Fatalf("user: %+v %v", got, err)
	}
}

func TestResolveCascadeTeamVM(t *testing.T) {
	f := newFixture(t, grants{})
	ud := f.image(t, store.EnvScopeUser, &f.user, "mine")
	td := f.image(t, store.EnvScopeTeam, &f.team, "teams")
	od := f.image(t, store.EnvScopeOrg, nil, "all")
	for _, i := range []store.Image{ud, td, od} {
		f.tag(t, i, "latest")
	}
	f.setDefault(t, store.EnvScopeOrg, nil, od, "latest")
	f.setDefault(t, store.EnvScopeUser, &f.user, ud, "latest")
	owner := images.Owner{TeamID: &f.team}
	got, err := f.r.Resolve(context.Background(), f.org, f.user, owner, "")
	if err != nil || got.Source != images.SourceOrgDefault {
		t.Fatalf("org: %+v %v", got, err)
	}
	f.setDefault(t, store.EnvScopeTeam, &f.team, td, "latest")
	got, err = f.r.Resolve(context.Background(), f.org, f.user, owner, "")
	if err != nil || got.Source != images.SourceTeamDefault || got.Image.ID != td.ID {
		t.Fatalf("team: %+v %v", got, err)
	}
}

func TestResolvePlatformFallback(t *testing.T) {
	f := newFixture(t, grants{})
	owner := images.Owner{UserID: &f.user}
	for _, ref := range []string{"", "observatory/standard"} {
		got, err := f.r.Resolve(context.Background(), f.org, f.user, owner, ref)
		if err != nil || got.Image != nil || got.Digest != "" || got.Source != images.SourcePlatform {
			t.Fatalf("%q: %+v %v", ref, got, err)
		}
	}
}

func TestResolveBrokenDefaultsFallBack(t *testing.T) {
	f := newFixture(t, grants{})
	ud := f.image(t, store.EnvScopeUser, &f.user, "mine")
	od := f.image(t, store.EnvScopeOrg, nil, "all")
	f.tag(t, ud, "latest")
	f.tag(t, od, "latest")
	f.setDefault(t, store.EnvScopeOrg, nil, od, "")
	f.setDefault(t, store.EnvScopeUser, &f.user, ud, "v1")
	owner := images.Owner{UserID: &f.user}
	got, err := f.r.Resolve(context.Background(), f.org, f.user, owner, "")
	if err != nil || got.Source != images.SourceOrgDefault || got.Digest != digest || got.Tag != "latest" {
		t.Fatalf("missing tag / empty tag: %+v %v", got, err)
	}
	if err := f.s.DeleteImage(context.Background(), f.org, od.ID); err != nil {
		t.Fatal(err)
	}
	got, err = f.r.Resolve(context.Background(), f.org, f.user, owner, "")
	if err != nil || got.Source != images.SourcePlatform {
		t.Fatalf("gone image: %+v %v", got, err)
	}
}

func TestResolvePlatformNotPinnable(t *testing.T) {
	f := newFixture(t, grants{})
	for _, ref := range []string{"observatory/standard:v2", "observatory/standard@" + digest} {
		if _, err := f.r.Resolve(context.Background(), f.org, f.user, images.Owner{UserID: &f.user}, ref); !errors.Is(err, images.ErrPlatformPinned) {
			t.Fatalf("%q: %v", ref, err)
		}
	}
}

func TestResolveNoUseRight(t *testing.T) {
	f := newFixture(t, grants{})
	f.image(t, store.EnvScopeUser, &f.peer, "secret")
	for _, ref := range []string{"secret", "me/secret", "secret:latest"} {
		if _, err := f.r.Resolve(context.Background(), f.org, f.user, images.Owner{UserID: &f.user}, ref); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("%q: %v", ref, err)
		}
	}
	team := f.image(t, store.EnvScopeTeam, &f.team, "internal")
	f.tag(t, team, "latest")
	f.r.Authz = grants{authz.ObjImages + " artifacts.images.use": true}
	if _, err := f.r.Resolve(context.Background(), f.org, f.user, images.Owner{TeamID: &f.team}, "core/internal"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("team without use: %v", err)
	}
}

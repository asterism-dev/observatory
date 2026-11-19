package store_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/asterism-dev/observatory/internal/db/dbtest"
	"github.com/asterism-dev/observatory/internal/store"
)

func newImageEnv(t *testing.T) (*store.Store, store.Org, uuid.UUID, store.Team) {
	t.Helper()
	s, u, org, team := newEnvStore(t)
	return s, org, u.User.ID, team
}

func newImage(t *testing.T, s *store.Store, org store.Org, user uuid.UUID, name string) store.Image {
	t.Helper()
	img, err := s.CreateImage(context.Background(), store.Image{OrgID: org.ID, Scope: store.EnvScopeOrg, Name: name, CreatedBy: user})
	if err != nil {
		t.Fatal(err)
	}
	return img
}

func TestImageNameUniquePerScopeOwner(t *testing.T) {
	ctx := context.Background()
	pool := dbtest.New(t)
	s := store.New(pool).WithMasterKey(testKey(1))
	a := login(t, s, "a").User.ID
	b := login(t, s, "b").User.ID
	org := mustCreateOrg(t, s, "I", a)
	execSQL(t, pool, `INSERT INTO memberships (org_id, user_id, role) VALUES ($1, $2, 'member')`, org.ID, b)
	team, err := s.CreateTeam(ctx, org.ID, "core")
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		scope store.EnvScope
		owner *uuid.UUID
	}{{store.EnvScopeUser, &a}, {store.EnvScopeUser, &b}, {store.EnvScopeTeam, &team.ID}, {store.EnvScopeOrg, nil}} {
		if _, err := s.CreateImage(ctx, store.Image{OrgID: org.ID, Scope: c.scope, OwnerID: c.owner, Name: "tools", CreatedBy: a}); err != nil {
			t.Fatalf("%s: %v", c.scope, err)
		}
	}
	_, err = s.CreateImage(ctx, store.Image{OrgID: org.ID, Scope: store.EnvScopeUser, OwnerID: &a, Name: "tools", CreatedBy: a})
	if !errors.Is(err, store.ErrImageExists) {
		t.Fatalf("duplicate: %v", err)
	}
	got, err := s.ImageByName(ctx, org.ID, store.EnvScopeTeam, &team.ID, "tools")
	if err != nil || got.OwnerID == nil || *got.OwnerID != team.ID {
		t.Fatalf("by name: %+v %v", got, err)
	}
	if _, err := s.ImageByName(ctx, org.ID, store.EnvScopeOrg, nil, "tools"); err != nil {
		t.Fatalf("org by name: %v", err)
	}
	if imgs, _ := s.ListImages(ctx, org.ID); len(imgs) != 4 {
		t.Fatalf("list: %d", len(imgs))
	}
	up, err := s.UpdateImage(ctx, store.Image{ID: got.ID, OrgID: org.ID, Scope: store.EnvScopeUser, OwnerID: &a, Name: "tools"})
	if !errors.Is(err, store.ErrImageExists) {
		t.Fatalf("move onto taken name: %v %+v", err, up)
	}
}

func TestRecipeRevisionsIncrement(t *testing.T) {
	ctx := context.Background()
	s, org, user, _ := newImageEnv(t)
	img := newImage(t, s, org, user, "r")
	if img.RecipeRevision != nil {
		t.Fatalf("fresh image has revision %v", *img.RecipeRevision)
	}
	refs := []store.RecipeRef{{Kind: "secret", Scope: store.EnvScopeOrg, Name: "TOKEN"}}
	for want := 1; want <= 2; want++ {
		n, err := s.AddRecipeRevision(ctx, store.RecipeRevision{ImageID: img.ID, CreatedBy: user, Refs: refs,
			Files: []store.RecipeFile{{Path: "Dockerfile", Mode: 0o644, Content: []byte(fmt.Sprintf("FROM x # %d", want))}, {Path: "bin/run.sh", Mode: 0o755, Content: []byte{0, 1}}}})
		if err != nil || n != want {
			t.Fatalf("revision %d: got %d, %v", want, n, err)
		}
	}
	cur, _ := s.Image(ctx, org.ID, img.ID)
	if cur.RecipeRevision == nil || *cur.RecipeRevision != 2 {
		t.Fatalf("current: %v", cur.RecipeRevision)
	}
	rev, err := s.RecipeRevision(ctx, img.ID, 1)
	if err != nil || len(rev.Files) != 2 || rev.Files[0].Path != "Dockerfile" || string(rev.Files[0].Content) != "FROM x # 1" ||
		rev.Files[1].Mode != 0o755 || len(rev.Refs) != 1 || rev.Refs[0].Name != "TOKEN" {
		t.Fatalf("revision 1: %+v %v", rev, err)
	}
	if _, err := s.RecipeRevision(ctx, img.ID, 3); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("missing revision: %v", err)
	}
	if _, err := s.AddRecipeRevision(ctx, store.RecipeRevision{ImageID: uuid.New(), CreatedBy: user}); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("unknown image: %v", err)
	}
}

func TestOneBuildPerImage(t *testing.T) {
	ctx := context.Background()
	s, org, user, _ := newImageEnv(t)
	img := newImage(t, s, org, user, "b")
	other := newImage(t, s, org, user, "b2")
	b, err := s.CreateImageBuild(ctx, store.ImageBuild{ImageID: img.ID, Revision: 1, StartedBy: user})
	if err != nil || b.State != store.BuildQueued {
		t.Fatalf("create: %+v %v", b, err)
	}
	if _, err := s.CreateImageBuild(ctx, store.ImageBuild{ImageID: img.ID, Revision: 1, StartedBy: user}); !errors.Is(err, store.ErrBuildRunning) {
		t.Fatalf("second: %v", err)
	}
	if _, err := s.CreateImageBuild(ctx, store.ImageBuild{ImageID: other.ID, Revision: 1, StartedBy: user}); err != nil {
		t.Fatalf("other image: %v", err)
	}
	b.State = store.BuildFailed
	b.Error = "boom"
	if err := s.UpdateImageBuild(ctx, b); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateImageBuild(ctx, store.ImageBuild{ImageID: img.ID, Revision: 2, StartedBy: user}); err != nil {
		t.Fatalf("after finish: %v", err)
	}
	bs, err := s.ImageBuilds(ctx, img.ID)
	if err != nil || len(bs) != 2 || bs[1].Error != "boom" {
		t.Fatalf("builds: %+v %v", bs, err)
	}
}

func TestDeleteVersionDropsTags(t *testing.T) {
	ctx := context.Background()
	s, org, user, _ := newImageEnv(t)
	img := newImage(t, s, org, user, "v")
	for _, d := range []string{"sha256:a", "sha256:b"} {
		for range 2 {
			if err := s.AddImageVersion(ctx, store.ImageVersion{ImageID: img.ID, Digest: d, Source: store.VersionFromPush, SizeBytes: 5, CreatedBy: &user}); err != nil {
				t.Fatal(err)
			}
		}
	}
	if vs, _ := s.ImageVersions(ctx, img.ID); len(vs) != 2 {
		t.Fatalf("idempotent add: %d versions", len(vs))
	}
	for tag, d := range map[string]string{"latest": "sha256:a", "stable": "sha256:a", "next": "sha256:b"} {
		if err := s.SetImageTag(ctx, img.ID, tag, d); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.SetImageTag(ctx, img.ID, "ghost", "sha256:zzz"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("tag unknown digest: %v", err)
	}
	if err := s.SetImageTag(ctx, img.ID, "latest", "sha256:b"); err != nil {
		t.Fatal(err)
	}
	if d, err := s.ResolveImageTag(ctx, img.ID, "latest"); err != nil || d != "sha256:b" {
		t.Fatalf("moved tag: %q %v", d, err)
	}
	if err := s.DeleteImageVersion(ctx, img.ID, "sha256:b"); err != nil {
		t.Fatal(err)
	}
	tags, _ := s.ImageTags(ctx, img.ID)
	if len(tags) != 1 || tags["stable"] != "sha256:a" {
		t.Fatalf("tags after delete: %v", tags)
	}
	if _, err := s.ResolveImageTag(ctx, img.ID, "next"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("resolve dropped tag: %v", err)
	}
	if err := s.DeleteImageVersion(ctx, img.ID, "sha256:b"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("delete twice: %v", err)
	}
	if err := s.DeleteImageVersions(ctx, img.ID); err != nil {
		t.Fatal(err)
	}
	if tags, _ := s.ImageTags(ctx, img.ID); len(tags) != 0 {
		t.Fatalf("tags after delete all: %v", tags)
	}
}

func TestBuildLogTruncation(t *testing.T) {
	ctx := context.Background()
	s, org, user, _ := newImageEnv(t)
	img := newImage(t, s, org, user, "l")
	b, err := s.CreateImageBuild(ctx, store.ImageBuild{ImageID: img.ID, Revision: 1, StartedBy: user})
	if err != nil {
		t.Fatal(err)
	}
	var all bytes.Buffer
	for i := 0; all.Len() < 12<<20; i++ {
		fmt.Fprintf(&all, "line %07d %s\n", i, strings.Repeat("x", 100))
	}
	raw := all.Bytes()
	for off := 0; off < len(raw); off += 1 << 20 {
		if err := s.AppendBuildLog(ctx, b.ID, raw[off:min(off+1<<20, len(raw))]); err != nil {
			t.Fatal(err)
		}
	}
	live, err := s.BuildLog(ctx, b.ID)
	if err != nil || !bytes.Equal(live, raw) {
		t.Fatalf("live log differs: %d vs %d bytes, %v", len(live), len(raw), err)
	}
	if err := s.FinishBuildLog(ctx, b.ID); err != nil {
		t.Fatal(err)
	}
	got, err := s.BuildLog(ctx, b.ID)
	if err != nil {
		t.Fatal(err)
	}
	const keep = 5 << 20
	if !bytes.HasPrefix(got, raw[:keep]) || !bytes.HasSuffix(got, raw[len(raw)-keep:]) || !bytes.Contains(got, []byte("\n--- log truncated ---\n")) {
		t.Fatalf("truncated log malformed: %d bytes", len(got))
	}
	if len(got) > 2*keep+64 {
		t.Fatalf("truncated log too long: %d", len(got))
	}

	small, _ := s.CreateImageBuild(ctx, store.ImageBuild{ImageID: newImage(t, s, org, user, "l2").ID, Revision: 1, StartedBy: user})
	_ = s.AppendBuildLog(ctx, small.ID, []byte("hello\n"))
	if err := s.FinishBuildLog(ctx, small.ID); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.BuildLog(ctx, small.ID); string(got) != "hello\n" {
		t.Fatalf("small log: %q", got)
	}
}

func TestImageDefaultFallsBackWhenImageDeleted(t *testing.T) {
	ctx := context.Background()
	s, org, user, team := newImageEnv(t)
	img := newImage(t, s, org, user, "d")
	keep := newImage(t, s, org, user, "keep")
	for _, d := range []store.ImageDefault{
		{OrgID: org.ID, Scope: store.EnvScopeOrg, ImageID: img.ID, Tag: "latest"},
		{OrgID: org.ID, Scope: store.EnvScopeTeam, OwnerID: &team.ID, ImageID: img.ID, Digest: "sha256:a"},
		{OrgID: org.ID, Scope: store.EnvScopeUser, OwnerID: &user, ImageID: keep.ID, Tag: "latest"},
	} {
		if err := s.SetImageDefault(ctx, d); err != nil {
			t.Fatalf("%s: %v", d.Scope, err)
		}
	}
	if err := s.SetImageDefault(ctx, store.ImageDefault{OrgID: org.ID, Scope: store.EnvScopeOrg, ImageID: keep.ID, Tag: "v1"}); err != nil {
		t.Fatal(err)
	}
	ds, _ := s.ImageDefaults(ctx, org.ID)
	if len(ds) != 3 {
		t.Fatalf("defaults after upsert: %+v", ds)
	}
	if err := s.DeleteImage(ctx, org.ID, img.ID); err != nil {
		t.Fatal(err)
	}
	ds, _ = s.ImageDefaults(ctx, org.ID)
	if len(ds) != 2 || ds[0].Scope == store.EnvScopeTeam || ds[1].Scope == store.EnvScopeTeam {
		t.Fatalf("defaults after delete: %+v", ds)
	}
	if err := s.ClearImageDefault(ctx, org.ID, store.EnvScopeUser, &user); err != nil {
		t.Fatal(err)
	}
	if ds, _ = s.ImageDefaults(ctx, org.ID); len(ds) != 1 || ds[0].Scope != store.EnvScopeOrg {
		t.Fatalf("defaults after clear: %+v", ds)
	}
	if err := s.DeleteImage(ctx, org.ID, img.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("delete twice: %v", err)
	}
}

func TestOrgImageUsageCountsSharedLayersOnce(t *testing.T) {
	ctx := context.Background()
	pool := dbtest.New(t)
	s := store.New(pool).WithMasterKey(testKey(1))
	u := login(t, s, "usage").User.ID
	org := mustCreateOrg(t, s, "U", u)
	other := mustCreateOrg(t, s, "U2", u)
	a := newImage(t, s, org, u, "a")
	b := newImage(t, s, org, u, "b")
	foreign := newImage(t, s, other, u, "f")
	for _, v := range []struct {
		img    store.Image
		digest string
	}{{a, "sha256:v1"}, {a, "sha256:v2"}, {b, "sha256:v3"}, {foreign, "sha256:v4"}} {
		if err := s.AddImageVersion(ctx, store.ImageVersion{ImageID: v.img.ID, Digest: v.digest, Source: store.VersionFromPush}); err != nil {
			t.Fatal(err)
		}
		execSQL(t, pool, `INSERT INTO image_version_layers (image_id, version_digest, digest, size) VALUES ($1, $2, 'sha256:base', 100), ($1, $2, $3, 10)`,
			v.img.ID, v.digest, "sha256:own-"+v.digest)
	}
	got, err := s.OrgImageUsage(ctx, org.ID)
	if err != nil || got != 100+3*10 {
		t.Fatalf("usage: %d %v", got, err)
	}
	if got, _ := s.OrgImageUsage(ctx, uuid.New()); got != 0 {
		t.Fatalf("empty org: %d", got)
	}
}

func TestLimitsImageStorage(t *testing.T) {
	ctx := context.Background()
	s, org, user, team := newImageEnv(t)
	teamScope := "team:" + team.ID.String()
	if err := s.SetLimits(ctx, org.ID, "org", store.Limits{ImageStorageGB: ip(10)}); err != nil {
		t.Fatal(err)
	}
	if err := s.SetLimits(ctx, org.ID, teamScope, store.Limits{ImageStorageGB: ip(11)}); !errors.Is(err, store.ErrLimitExceedsOrg) {
		t.Fatalf("team above org: %v", err)
	}
	if err := s.SetLimits(ctx, org.ID, teamScope, store.Limits{ImageStorageGB: ip(4)}); err != nil {
		t.Fatal(err)
	}
	if err := s.SetLimits(ctx, org.ID, "org", store.Limits{ImageStorageGB: ip(3)}); !errors.Is(err, store.ErrLimitExceedsOrg) {
		t.Fatalf("org below team: %v", err)
	}
	ls, err := s.Limits(ctx, org.ID)
	if err != nil || *ls["org"].ImageStorageGB != 10 || *ls[teamScope].ImageStorageGB != 4 || ls["org"].VCPU != nil {
		t.Fatalf("limits: %+v %v", ls, err)
	}
	_ = user
}

func TestBuildLogFinishOnce(t *testing.T) {
	ctx := context.Background()
	s, org, user, _ := newImageEnv(t)
	b, err := s.CreateImageBuild(ctx, store.ImageBuild{ImageID: newImage(t, s, org, user, "f").ID, Revision: 1, StartedBy: user})
	if err != nil {
		t.Fatal(err)
	}
	_ = s.AppendBuildLog(ctx, b.ID, []byte("hello\n"))
	for range 2 {
		if err := s.FinishBuildLog(ctx, b.ID); err != nil {
			t.Fatal(err)
		}
	}
	if got, _ := s.BuildLog(ctx, b.ID); string(got) != "hello\n" {
		t.Fatalf("log after double finish: %q", got)
	}
	if err := s.AppendBuildLog(ctx, b.ID, []byte("late\n")); !errors.Is(err, store.ErrBuildLogFinished) {
		t.Fatalf("append after finish: %v", err)
	}
	if got, _ := s.BuildLog(ctx, b.ID); string(got) != "hello\n" {
		t.Fatalf("log after late append: %q", got)
	}
	if err := s.FinishBuildLog(ctx, uuid.New()); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("finish unknown: %v", err)
	}
	if err := s.AppendBuildLog(ctx, uuid.New(), []byte("x")); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("append unknown: %v", err)
	}
}

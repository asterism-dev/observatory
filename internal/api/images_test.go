package api

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/base64"
	"fmt"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"

	"github.com/asterism-dev/observatory/internal/store"
)

type fakeManifests struct {
	mu      sync.Mutex
	deleted []string
}

func (f *fakeManifests) DeleteManifest(_ context.Context, repo, digest string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.deleted = append(f.deleted, repo+"@"+digest)
	return nil
}

func (e *env) withManifests() *fakeManifests {
	f := &fakeManifests{}
	e.api.manifests = f
	return f
}

type imageListJSON []struct {
	ID           uuid.UUID
	Name, Scope  string
	Ref          string
	Platform     bool
	OwnerID      *uuid.UUID
	Tags         map[string]string
	LatestDigest string
	SizeBytes    int64
}

func imagesPath(org uuid.UUID) string { return "/api/orgs/" + org.String() + "/images" }

func imagePath(org, id uuid.UUID) string { return imagesPath(org) + "/" + id.String() }

func (e *env) createImage(tok string, org uuid.UUID, body string) uuid.UUID {
	e.t.Helper()
	rec := e.send("POST", imagesPath(org), tok, body)
	if rec.Code != 201 {
		e.t.Fatalf("create image %s: %d %s", body, rec.Code, rec.Body.String())
	}
	var got struct{ ID uuid.UUID }
	decode(e.t, rec, &got)
	return got.ID
}

func (e *env) listImages(tok string, org uuid.UUID) imageListJSON {
	e.t.Helper()
	rec := e.do("GET", imagesPath(org), tok)
	if rec.Code != 200 {
		e.t.Fatalf("list images: %d %s", rec.Code, rec.Body.String())
	}
	var got imageListJSON
	decode(e.t, rec, &got)
	return got
}

func digestN(n int) string { return fmt.Sprintf("sha256:%064x", n) }

func (e *env) addVersion(image uuid.UUID, n int, tags ...string) string {
	e.t.Helper()
	ctx := context.Background()
	d := digestN(n)
	if err := e.store.AddImageVersion(ctx, store.ImageVersion{ImageID: image, Digest: d, Source: store.VersionFromPush, SizeBytes: int64(n)}); err != nil {
		e.t.Fatal(err)
	}
	for _, tag := range tags {
		if err := e.store.SetImageTag(ctx, image, tag, d); err != nil {
			e.t.Fatal(err)
		}
	}
	return d
}

type tarEntry struct {
	name    string
	content []byte
}

func recipeTar(t *testing.T, entries ...tarEntry) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(zw)
	for _, en := range entries {
		if err := tw.WriteHeader(&tar.Header{Name: en.name, Mode: 0o644, Size: int64(len(en.content)), Typeflag: tar.TypeReg}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write(en.content); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func (e *env) putRecipe(tok string, org, id uuid.UUID, body []byte, refs string) *httptest.ResponseRecorder {
	req := httptest.NewRequest("PUT", imagePath(org, id)+"/recipe", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+tok)
	req.Header.Set("Content-Type", "application/gzip")
	if refs != "" {
		req.Header.Set("X-Observatory-Refs", refs)
	}
	rec := httptest.NewRecorder()
	e.h.ServeHTTP(rec, req)
	return rec
}

var dockerfile = tarEntry{"Dockerfile", []byte("FROM scratch\n")}

func TestCreateImageScopes(t *testing.T) {
	e := newEnv(t)
	_, aliceTok := e.user("alice")
	bob, bobTok := e.user("bob")
	carol, carolTok := e.user("carol")
	org := e.createOrg(aliceTok, "Acme")
	e.addMember(org, bob, store.RoleMember)
	e.addMember(org, carol, store.RoleMember)
	team := e.createTeam(aliceTok, org, "core")
	e.makeTeamAdmin(aliceTok, org, team, carol)

	e.createImage(aliceTok, org, `{"name":"base","description":"shared","scope":"org"}`)
	if rec := e.send("POST", imagesPath(org), bobTok, `{"name":"mine","scope":"org"}`); rec.Code != 403 {
		t.Fatalf("member creates org image: %d", rec.Code)
	}
	private := e.createImage(bobTok, org, `{"name":"base","scope":"user"}`)
	if rec := e.send("POST", imagesPath(org), bobTok, `{"name":"t","scope":"team","teamId":"`+team.String()+`"}`); rec.Code != 403 {
		t.Fatalf("non-maintainer creates team image: %d", rec.Code)
	}
	e.createImage(carolTok, org, `{"name":"base","scope":"team","teamId":"`+team.String()+`"}`)
	if rec := e.send("POST", imagesPath(org), aliceTok, `{"name":"base","scope":"org"}`); rec.Code != 409 {
		t.Fatalf("duplicate: %d", rec.Code)
	}
	for _, body := range []string{`{"name":"Bad Name","scope":"org"}`, `{"name":"x","scope":"galaxy"}`, `{"name":"x","scope":"team","teamId":"` + uuid.NewString() + `"}`} {
		if rec := e.send("POST", imagesPath(org), aliceTok, body); rec.Code != 400 {
			t.Fatalf("invalid %s: %d", body, rec.Code)
		}
	}
	if es := e.audit(store.ActionImageCreate); len(es) != 3 {
		t.Fatalf("audit: %d", len(es))
	}

	scopes := func(l imageListJSON) string {
		var s []string
		for _, i := range l {
			s = append(s, i.Ref)
		}
		slices.Sort(s[1:])
		return strings.Join(s, ",")
	}
	if got := scopes(e.listImages(aliceTok, org)); got != "observatory/standard,core/base,org/base" {
		t.Fatalf("alice sees %s", got)
	}
	if got := scopes(e.listImages(bobTok, org)); got != "observatory/standard,me/base,org/base" {
		t.Fatalf("bob sees %s", got)
	}
	if rec := e.do("GET", imagePath(org, private), carolTok); rec.Code != 404 {
		t.Fatalf("other member reads private image: %d", rec.Code)
	}
	if rec := e.do("GET", imagePath(org, private), bobTok); rec.Code != 200 {
		t.Fatalf("owner reads private image: %d", rec.Code)
	}
}

func TestRecipeUploadTarLimits(t *testing.T) {
	e := newEnv(t)
	_, tok := e.user("alice")
	org := e.createOrg(tok, "Acme")
	id := e.createImage(tok, org, `{"name":"base","scope":"org"}`)

	if rec := e.putRecipe(tok, org, id, recipeTar(t, dockerfile, tarEntry{"big", make([]byte, 51<<20)}), ""); rec.Code != 413 {
		t.Fatalf("51 MB: %d %s", rec.Code, rec.Body.String())
	}
	if rec := e.putRecipe(tok, org, id, recipeTar(t, dockerfile, tarEntry{"../x", []byte("x")}), ""); rec.Code != 422 {
		t.Fatalf("../x: %d %s", rec.Code, rec.Body.String())
	}
	if rec := e.putRecipe(tok, org, id, recipeTar(t, tarEntry{"sub/Dockerfile", []byte("FROM scratch\n")}), ""); rec.Code != 422 {
		t.Fatalf("missing Dockerfile: %d %s", rec.Code, rec.Body.String())
	}
	if rec := e.putRecipe(tok, org, id, recipeTar(t, dockerfile, tarEntry{"./bin/tool", []byte{0xff, 0xfe}}), ""); rec.Code != 200 || !strings.Contains(rec.Body.String(), `"revision":1`) {
		t.Fatalf("valid: %d %s", rec.Code, rec.Body.String())
	}
	rec := e.do("GET", imagePath(org, id)+"/recipe", tok)
	var got struct {
		Revision int
		Files    []struct {
			Path    string
			Size    int
			Content *string
		}
	}
	decode(t, rec, &got)
	if got.Revision != 1 || len(got.Files) != 2 || got.Files[0].Path != "Dockerfile" || got.Files[0].Content == nil ||
		*got.Files[0].Content != "FROM scratch\n" || got.Files[1].Path != "bin/tool" || got.Files[1].Content != nil || got.Files[1].Size != 2 {
		t.Fatalf("recipe: %s", rec.Body.String())
	}
	if rec := e.do("GET", imagePath(org, id)+"/recipe/files/bin/tool", tok); rec.Code != 200 || rec.Body.String() != "\xff\xfe" {
		t.Fatalf("raw file: %d %q", rec.Code, rec.Body.String())
	}
}

func TestRecipeEditConflict(t *testing.T) {
	e := newEnv(t)
	_, tok := e.user("alice")
	org := e.createOrg(tok, "Acme")
	id := e.createImage(tok, org, `{"name":"base","scope":"org"}`)
	b64 := func(s string) string { return base64.StdEncoding.EncodeToString([]byte(s)) }
	path := imagePath(org, id) + "/recipe/edits"

	if rec := e.send("POST", path, tok, `{"base":0,"put":[{"path":"Dockerfile","content":"`+b64("FROM scratch\n")+`"},{"path":"a.sh","mode":493,"content":"`+b64("echo")+`"}]}`); rec.Code != 200 {
		t.Fatalf("first edit: %d %s", rec.Code, rec.Body.String())
	}
	if rec := e.send("POST", path, tok, `{"base":0,"delete":["a.sh"]}`); rec.Code != 409 {
		t.Fatalf("stale base: %d %s", rec.Code, rec.Body.String())
	}
	if rec := e.send("POST", path, tok, `{"base":1,"delete":["Dockerfile"]}`); rec.Code != 422 {
		t.Fatalf("delete Dockerfile: %d %s", rec.Code, rec.Body.String())
	}
	if rec := e.send("POST", path, tok, `{"base":1,"delete":["a.sh"]}`); rec.Code != 200 || !strings.Contains(rec.Body.String(), `"revision":2`) {
		t.Fatalf("second edit: %d %s", rec.Code, rec.Body.String())
	}
	rev, err := e.store.RecipeRevision(context.Background(), id, 1)
	if err != nil || len(rev.Files) != 2 || rev.Files[1].Path != "a.sh" || rev.Files[1].Mode != 0o755 {
		t.Fatalf("revision 1 must stay: %+v %v", rev, err)
	}
}

func TestRefsScopeRule(t *testing.T) {
	e := newEnv(t)
	_, aliceTok := e.user("alice")
	bob, bobTok := e.user("bob")
	org := e.createOrg(aliceTok, "Acme")
	e.addMember(org, bob, store.RoleMember)
	team := e.createTeam(aliceTok, org, "core")
	other := e.createTeam(aliceTok, org, "other")
	e.makeTeamAdmin(aliceTok, org, team, bob)
	e.createEnv(aliceTok, org, `{"name":"ORG_TOKEN","value":"v","kind":"secret","scope":"org"}`)
	e.createEnv(aliceTok, org, `{"name":"TEAM_TOKEN","value":"v","kind":"secret","scope":"team","teamId":"`+team.String()+`"}`)
	e.createEnv(aliceTok, org, `{"name":"OTHER_TOKEN","value":"v","kind":"secret","scope":"team","teamId":"`+other.String()+`"}`)
	orgImg := e.createImage(aliceTok, org, `{"name":"base","scope":"org"}`)
	teamImg := e.createImage(bobTok, org, `{"name":"base","scope":"team","teamId":"`+team.String()+`"}`)
	privImg := e.createImage(bobTok, org, `{"name":"base","scope":"user"}`)
	ctx := recipeTar(t, dockerfile)
	ref := func(name, scope string, teamID uuid.UUID) string {
		s := `{"kind":"secret","scope":"` + scope + `","name":"` + name + `"`
		if scope == "team" {
			s += `,"teamId":"` + teamID.String() + `"`
		}
		return s + `}`
	}

	for _, c := range []struct {
		tok  string
		img  uuid.UUID
		refs string
		want int
	}{
		{aliceTok, orgImg, `[` + ref("TEAM_TOKEN", "team", team) + `]`, 422},
		{aliceTok, orgImg, `[` + ref("MISSING", "org", team) + `]`, 422},
		{aliceTok, orgImg, `[{"kind":"integration","scope":"org","name":"github"}]`, 422},
		{aliceTok, orgImg, `[` + ref("ORG_TOKEN", "org", team) + `]`, 200},
		{bobTok, teamImg, `[` + ref("OTHER_TOKEN", "team", other) + `]`, 422},
		{bobTok, teamImg, `[` + ref("TEAM_TOKEN", "team", team) + `,` + ref("ORG_TOKEN", "org", team) + `]`, 200},
		{bobTok, privImg, `[` + ref("OTHER_TOKEN", "team", other) + `]`, 403},
		{bobTok, privImg, `[` + ref("TEAM_TOKEN", "team", team) + `]`, 200},
	} {
		if rec := e.putRecipe(c.tok, org, c.img, ctx, c.refs); rec.Code != c.want {
			t.Fatalf("%s: %d %s", c.refs, rec.Code, rec.Body.String())
		}
	}
}

func TestWidenVisibilityNeedsConfirm(t *testing.T) {
	e := newEnv(t)
	m := e.withManifests()
	_, aliceTok := e.user("alice")
	bob, bobTok := e.user("bob")
	org := e.createOrg(aliceTok, "Acme")
	e.addMember(org, bob, store.RoleMember)
	team := e.createTeam(aliceTok, org, "core")
	e.makeTeamAdmin(aliceTok, org, team, bob)
	id := e.createImage(bobTok, org, `{"name":"tools","scope":"user"}`)
	e.addVersion(id, 1, "latest")
	e.addVersion(id, 2)

	if rec := e.send("PATCH", imagePath(org, id), bobTok, `{"scope":"org"}`); rec.Code != 403 {
		t.Fatalf("member widens to org: %d", rec.Code)
	}
	body := `{"scope":"team","teamId":"` + team.String() + `"`
	rec := e.send("PATCH", imagePath(org, id), bobTok, body+`}`)
	if rec.Code != 409 || !strings.Contains(rec.Body.String(), "widening visibility deletes 2 versions") {
		t.Fatalf("unconfirmed: %d %s", rec.Code, rec.Body.String())
	}
	if rec := e.send("PATCH", imagePath(org, id), bobTok, body+`,"confirmDeleteVersions":true}`); rec.Code != 200 {
		t.Fatalf("confirmed: %d %s", rec.Code, rec.Body.String())
	}
	vs, err := e.store.ImageVersions(context.Background(), id)
	if err != nil || len(vs) != 0 {
		t.Fatalf("versions left: %v %v", vs, err)
	}
	if len(m.deleted) != 2 || !strings.HasPrefix(m.deleted[0], "i/"+id.String()+"@sha256:") {
		t.Fatalf("manifests: %v", m.deleted)
	}
	e.addVersion(id, 3)
	if rec := e.send("PATCH", imagePath(org, id), bobTok, `{"scope":"user"}`); rec.Code != 200 {
		t.Fatalf("narrowing keeps versions without confirm: %d %s", rec.Code, rec.Body.String())
	}
	if vs, _ := e.store.ImageVersions(context.Background(), id); len(vs) != 1 {
		t.Fatalf("narrowing deleted versions: %v", vs)
	}
}

func TestWidenRefusedWithPrivateRefs(t *testing.T) {
	e := newEnv(t)
	_, aliceTok := e.user("alice")
	bob, bobTok := e.user("bob")
	org := e.createOrg(aliceTok, "Acme")
	e.addMember(org, bob, store.RoleMember)
	team := e.createTeam(aliceTok, org, "core")
	e.makeTeamAdmin(aliceTok, org, team, bob)
	e.createEnv(bobTok, org, `{"name":"MY_TOKEN","value":"v","kind":"secret","scope":"user"}`)
	id := e.createImage(bobTok, org, `{"name":"tools","scope":"user"}`)
	if rec := e.putRecipe(bobTok, org, id, recipeTar(t, dockerfile), `[{"kind":"secret","scope":"user","name":"MY_TOKEN"}]`); rec.Code != 200 {
		t.Fatalf("recipe: %d %s", rec.Code, rec.Body.String())
	}
	rec := e.send("PATCH", imagePath(org, id), bobTok, `{"scope":"team","teamId":"`+team.String()+`","confirmDeleteVersions":true}`)
	if rec.Code != 422 || !strings.Contains(rec.Body.String(), "MY_TOKEN") {
		t.Fatalf("widen with private ref: %d %s", rec.Code, rec.Body.String())
	}
}

func TestDeleteImageUsedByVMAndDefault(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	m := e.withManifests()
	alice, tok := e.user("alice")
	org := e.createOrg(tok, "Acme")
	base := e.createImage(tok, org, `{"name":"base","scope":"org"}`)
	d := e.addVersion(base, 1, "latest")
	tools := e.createImage(tok, org, `{"name":"tools","scope":"org"}`)
	td := e.addVersion(tools, 2, "latest")
	defaults := "/api/orgs/" + org.String() + "/image-defaults/"
	if rec := e.send("PUT", defaults+"org", tok, `{"ref":"base"}`); rec.Code != 204 {
		t.Fatalf("set org default: %d %s", rec.Code, rec.Body.String())
	}
	if rec := e.send("PUT", defaults+"user", tok, `{"ref":"org/tools@`+td+`"}`); rec.Code != 204 {
		t.Fatalf("set user default: %d %s", rec.Code, rec.Body.String())
	}
	type defaultsJSON struct {
		Configured *struct {
			Ref     string
			Removed bool
		}
		Effective struct{ Ref, Source, Digest string }
	}
	get := func(level string) defaultsJSON {
		t.Helper()
		rec := e.do("GET", defaults+level, tok)
		if rec.Code != 200 {
			t.Fatalf("get %s: %d %s", level, rec.Code, rec.Body.String())
		}
		var got defaultsJSON
		decode(t, rec, &got)
		return got
	}
	if got := get("user"); got.Configured == nil || got.Configured.Ref != "org/tools@"+td || got.Effective.Source != "user-default" {
		t.Fatalf("user default: %+v", got)
	}

	node, ntok, err := e.store.CreateNode(ctx, store.Node{OrgID: org, Name: "n1", CreatedBy: alice.ID})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := e.store.EnrollNode(ctx, ntok); err != nil {
		t.Fatal(err)
	}
	vm, err := e.store.CreateVM(ctx, store.VM{OrgID: org, NodeID: node.ID, UserID: &alice.ID, Name: "v", CPUs: 1, MemoryMB: 1, DiskGB: 1, CreatedBy: alice.ID})
	if err != nil {
		t.Fatal(err)
	}
	e.exec("UPDATE vms SET image = $2 WHERE id = $1", vm.ID, "i/"+base.String()+"@"+d)

	if rec := e.do("DELETE", imagePath(org, tools)+"/versions/"+td, tok); rec.Code != 204 {
		t.Fatalf("delete version: %d %s", rec.Code, rec.Body.String())
	}
	if got := get("user"); got.Configured == nil || !got.Configured.Removed || got.Effective.Source != "org-default" || got.Effective.Digest != d {
		t.Fatalf("user default after version delete: %+v", got)
	}
	if rec := e.do("DELETE", imagePath(org, base), tok); rec.Code != 204 {
		t.Fatalf("delete image: %d %s", rec.Code, rec.Body.String())
	}
	got, err := e.store.VM(ctx, org, vm.ID)
	if err != nil || got.Image != "i/"+base.String()+"@"+d {
		t.Fatalf("vm changed: %+v %v", got, err)
	}
	if got := get("org"); got.Configured != nil || got.Effective.Source != "platform" || got.Effective.Ref != "observatory/standard" {
		t.Fatalf("org default after delete: %+v", got)
	}
	if len(m.deleted) != 2 || m.deleted[0] != "i/"+tools.String()+"@"+td || m.deleted[1] != "i/"+base.String()+"@"+d {
		t.Fatalf("manifests: %v", m.deleted)
	}
}

func TestPlatformImageReadOnly(t *testing.T) {
	e := newEnv(t)
	_, tok := e.user("alice")
	org := e.createOrg(tok, "Acme")
	e.listImages(tok, org)
	l := e.listImages(tok, org)
	if len(l) != 1 || !l[0].Platform || l[0].Ref != "observatory/standard" {
		t.Fatalf("list: %+v", l)
	}
	var n int
	if err := e.pool.QueryRow(context.Background(), "SELECT count(*) FROM images WHERE platform").Scan(&n); err != nil || n != 1 {
		t.Fatalf("platform rows: %d %v", n, err)
	}
	id := l[0].ID
	if rec := e.do("GET", imagePath(org, id), tok); rec.Code != 200 || !strings.Contains(rec.Body.String(), `"platform":true`) {
		t.Fatalf("get: %d %s", rec.Code, rec.Body.String())
	}
	if rec := e.do("GET", imagePath(org, id)+"/recipe", tok); rec.Code != 200 || !strings.Contains(rec.Body.String(), `"files":[]`) {
		t.Fatalf("recipe: %d %s", rec.Code, rec.Body.String())
	}
	for _, c := range []struct{ method, path, body string }{
		{"PATCH", "", `{"name":"mine"}`},
		{"DELETE", "", ""},
		{"POST", "/recipe/edits", `{"base":0}`},
		{"PUT", "/tags/v1", `{"digest":"` + digestN(1) + `"}`},
		{"DELETE", "/tags/v1", ""},
		{"DELETE", "/versions/" + digestN(1), ""},
	} {
		if rec := e.send(c.method, imagePath(org, id)+c.path, tok, c.body); rec.Code != 403 || !strings.Contains(rec.Body.String(), "read-only") {
			t.Fatalf("%s %s: %d %s", c.method, c.path, rec.Code, rec.Body.String())
		}
	}
	if rec := e.putRecipe(tok, org, id, recipeTar(t, dockerfile), ""); rec.Code != 403 {
		t.Fatalf("recipe put: %d", rec.Code)
	}
	defaults := "/api/orgs/" + org.String() + "/image-defaults/org"
	if rec := e.send("PUT", defaults, tok, `{"ref":"observatory/standard:v1"}`); rec.Code != 422 {
		t.Fatalf("pinned platform default: %d %s", rec.Code, rec.Body.String())
	}
	if rec := e.send("PUT", defaults, tok, `{"ref":"observatory/standard"}`); rec.Code != 204 {
		t.Fatalf("platform default: %d %s", rec.Code, rec.Body.String())
	}
	if rec := e.do("GET", defaults, tok); !strings.Contains(rec.Body.String(), `"configured":{"ref":"observatory/standard","removed":false}`) {
		t.Fatalf("get default: %s", rec.Body.String())
	}
}

func TestImageTagsAndVersions(t *testing.T) {
	e := newEnv(t)
	_, tok := e.user("alice")
	org := e.createOrg(tok, "Acme")
	id := e.createImage(tok, org, `{"name":"base","scope":"org"}`)
	d1 := e.addVersion(id, 1)
	d2 := e.addVersion(id, 2, "latest")
	tags := imagePath(org, id) + "/tags/"
	if rec := e.send("PUT", tags+"stable", tok, `{"digest":"`+d1+`"}`); rec.Code != 204 {
		t.Fatalf("set tag: %d %s", rec.Code, rec.Body.String())
	}
	for _, c := range []struct{ method, tag, body string }{
		{"PUT", "latest", `{"digest":"` + d1 + `"}`},
		{"PUT", "bad%20tag", `{"digest":"` + d1 + `"}`},
		{"DELETE", "latest", ""},
	} {
		if rec := e.send(c.method, tags+c.tag, tok, c.body); rec.Code != 422 {
			t.Fatalf("%s %s: %d %s", c.method, c.tag, rec.Code, rec.Body.String())
		}
	}
	if rec := e.send("PUT", tags+"x", tok, `{"digest":"`+digestN(9)+`"}`); rec.Code != 404 {
		t.Fatalf("unknown digest: %d", rec.Code)
	}
	l := e.listImages(tok, org)
	if len(l) != 2 || l[1].LatestDigest != d2 || l[1].SizeBytes != 2 || l[1].Tags["stable"] != d1 {
		t.Fatalf("list: %+v", l)
	}
	rec := e.do("GET", imagePath(org, id)+"/versions", tok)
	var vs []struct {
		Digest string
		Tags   []string
	}
	decode(t, rec, &vs)
	if len(vs) != 2 {
		t.Fatalf("versions: %s", rec.Body.String())
	}
	if rec := e.do("DELETE", tags+"stable", tok); rec.Code != 204 {
		t.Fatalf("delete tag: %d", rec.Code)
	}
	if rec := e.do("DELETE", tags+"stable", tok); rec.Code != 404 {
		t.Fatalf("delete missing tag: %d", rec.Code)
	}
}

func TestImageDefaultsIntegrity(t *testing.T) {
	e := newEnv(t)
	_, aliceTok := e.user("alice")
	bob, bobTok := e.user("bob")
	org := e.createOrg(aliceTok, "Acme")
	e.addMember(org, bob, store.RoleMember)
	core := e.createTeam(aliceTok, org, "core")
	other := e.createTeam(aliceTok, org, "other")
	foreign := e.createTeam(aliceTok, e.createOrg(aliceTok, "Elsewhere"), "core")
	e.addVersion(e.createImage(aliceTok, org, `{"name":"coreimg","scope":"team","teamId":"`+core.String()+`"}`), 1, "latest")
	e.addVersion(e.createImage(aliceTok, org, `{"name":"base","scope":"org"}`), 2, "latest")
	e.addVersion(e.createImage(bobTok, org, `{"name":"mine","scope":"user"}`), 3, "latest")
	defaults := "/api/orgs/" + org.String() + "/image-defaults/"

	for _, c := range []struct {
		tok, level, ref string
		want            int
	}{
		{bobTok, "org", "base", 403},
		{aliceTok, "org", "core/coreimg", 422},
		{aliceTok, "team/" + other.String(), "core/coreimg", 422},
		{aliceTok, "team/" + foreign.String(), "base", 404},
		{aliceTok, "team/" + core.String(), "core/coreimg", 204},
		{bobTok, "user", "core/coreimg", 422},
		{bobTok, "user", "nope", 422},
		{bobTok, "user", "mine", 204},
		{aliceTok, "org", "base", 204},
	} {
		if rec := e.send("PUT", defaults+c.level, c.tok, `{"ref":"`+c.ref+`"}`); rec.Code != c.want {
			t.Fatalf("%s %s: %d %s", c.level, c.ref, rec.Code, rec.Body.String())
		}
	}
	if rec := e.do("DELETE", defaults+"org", bobTok); rec.Code != 403 {
		t.Fatalf("member clears org default: %d", rec.Code)
	}
	if rec := e.do("DELETE", defaults+"user", bobTok); rec.Code != 204 {
		t.Fatalf("clear own default: %d", rec.Code)
	}
	if rec := e.do("GET", defaults+"user", bobTok); !strings.Contains(rec.Body.String(), `"source":"org-default"`) {
		t.Fatalf("fallback: %s", rec.Body.String())
	}
}

func TestRemoveMemberDeletesPrivateImages(t *testing.T) {
	e := newEnv(t)
	m := e.withManifests()
	_, aliceTok := e.user("alice")
	bob, bobTok := e.user("bob")
	org := e.createOrg(aliceTok, "Acme")
	e.addMember(org, bob, store.RoleMember)
	id := e.createImage(bobTok, org, `{"name":"tools","scope":"user"}`)
	d := e.addVersion(id, 1, "latest")
	if rec := e.send("PUT", "/api/orgs/"+org.String()+"/image-defaults/user", bobTok, `{"ref":"tools"}`); rec.Code != 204 {
		t.Fatalf("default: %d %s", rec.Code, rec.Body.String())
	}
	if rec := e.do("DELETE", "/api/orgs/"+org.String()+"/members/"+bob.ID.String(), aliceTok); rec.Code != 204 {
		t.Fatalf("remove: %d %s", rec.Code, rec.Body.String())
	}
	if _, err := e.store.Image(context.Background(), org, id); err != store.ErrNotFound {
		t.Fatalf("private image kept: %v", err)
	}
	if len(m.deleted) != 1 || m.deleted[0] != "i/"+id.String()+"@"+d {
		t.Fatalf("manifests: %v", m.deleted)
	}
}

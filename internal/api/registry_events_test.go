package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/asterism-dev/observatory/internal/registry"
	"github.com/asterism-dev/observatory/internal/store"
)

const eventsSecret = "events-secret-events-secret-0123"

func (e *env) eventsHandler(m *fakeManifests) http.Handler {
	e.t.Helper()
	e.api.tags = NewTagSync(e.store, m)
	iss, err := registry.LoadIssuer(e.t.TempDir())
	if err != nil {
		e.t.Fatal(err)
	}
	mux := http.NewServeMux()
	RegistryEndpoints(mux, e.store, e.z, Registry{PublicURL: &url.URL{Scheme: "https", Host: "hub.test"}, Issuer: iss, Manifests: m, Tags: e.api.tags, EventsSecret: eventsSecret})
	return mux
}

func pushEvent(t *testing.T, h http.Handler, secret, repo, digest, tag, actor string) int {
	t.Helper()
	body := `{"events":[
		{"action":"push","target":{"mediaType":"application/vnd.oci.image.layer.v1.tar+gzip","digest":"sha256:layer","repository":"` + repo + `"}},
		{"action":"push","target":{"mediaType":"application/vnd.oci.image.manifest.v1+json","digest":"` + digest + `","repository":"` + repo + `","tag":"` + tag + `"},"actor":{"name":"` + actor + `"}}]}`
	req := httptest.NewRequest("POST", "/internal/registry/events", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/vnd.docker.distribution.events.v2+json")
	if secret != "" {
		req.Header.Set("Authorization", "Bearer "+secret)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec.Code
}

func amd64Manifest(layers ...registry.Layer) registry.Manifest {
	return registry.Manifest{Platform: "linux/amd64", Layers: layers, Blobs: layers}
}

func (e *env) pushError(id uuid.UUID) string {
	e.t.Helper()
	img, err := e.store.ImageByID(context.Background(), id)
	if err != nil {
		e.t.Fatal(err)
	}
	return img.PushError
}

func TestPushEventCreatesVersionAndLatest(t *testing.T) {
	e := newEnv(t)
	alice, aliceTok := e.user("alice")
	org := e.createOrg(aliceTok, "Acme")
	img := e.createImage(aliceTok, org, `{"name":"a","scope":"org"}`)
	repo := "i/" + img.String()
	fm := &fakeManifests{manifests: map[string]registry.Manifest{
		digestN(1): amd64Manifest(registry.Layer{Digest: "sha256:l1", Size: 100}, registry.Layer{Digest: "sha256:l2", Size: 50}),
		digestN(2): amd64Manifest(registry.Layer{Digest: "sha256:l1", Size: 100}),
	}}
	h := e.eventsHandler(fm)
	if err := e.store.SetImagePushError(context.Background(), img, "old"); err != nil {
		t.Fatal(err)
	}

	if code := pushEvent(t, h, eventsSecret, repo, digestN(1), "v1", "user:"+alice.ID.String()); code != 200 {
		t.Fatalf("push: %d", code)
	}
	ctx := context.Background()
	v, err := e.store.ImageVersion(ctx, img, digestN(1))
	if err != nil || v.Source != store.VersionFromPush || v.SizeBytes != 150 || v.CreatedBy == nil || *v.CreatedBy != alice.ID || v.BuildID != nil {
		t.Fatalf("version: %+v %v", v, err)
	}
	if used, _ := e.store.OrgImageUsage(ctx, org); used != 150 {
		t.Fatalf("usage %d", used)
	}
	tags, _ := e.store.ImageTags(ctx, img)
	if tags["v1"] != digestN(1) || tags["latest"] != digestN(1) {
		t.Fatalf("tags %v", tags)
	}
	if e.pushError(img) != "" {
		t.Fatal("push error not cleared")
	}
	e.api.tags.wait()
	if fm.tag(repo, "latest") != digestN(1) || fm.tag(repo, "v1") != digestN(1) {
		t.Fatalf("registry tags %v", fm.tags)
	}

	// Redelivery is harmless and a second push moves latest.
	for range 2 {
		if code := pushEvent(t, h, eventsSecret, repo, digestN(2), "v2", "user:"+alice.ID.String()); code != 200 {
			t.Fatalf("push 2: %d", code)
		}
	}
	tags, _ = e.store.ImageTags(ctx, img)
	if tags["v1"] != digestN(1) || tags["v2"] != digestN(2) || tags["latest"] != digestN(2) {
		t.Fatalf("tags %v", tags)
	}
	if used, _ := e.store.OrgImageUsage(ctx, org); used != 150 {
		t.Fatalf("shared layer counted twice: %d", used)
	}

	b, err := e.store.CreateImageBuild(ctx, store.ImageBuild{ImageID: img, Revision: 1, StartedBy: alice.ID})
	if err != nil {
		t.Fatal(err)
	}
	fm.manifests[digestN(3)] = amd64Manifest()
	fm.manifests[digestN(4)] = amd64Manifest()
	if code := pushEvent(t, h, eventsSecret, repo, digestN(3), "build-1", "build:"+b.ID.String()); code != 200 {
		t.Fatalf("build push: %d", code)
	}
	if code := pushEvent(t, h, eventsSecret, repo, digestN(4), "latest", "build:"+b.ID.String()); code != 200 {
		t.Fatalf("build push of latest: %d", code)
	}
	tags, _ = e.store.ImageTags(ctx, img)
	if tags["build-1"] != digestN(3) || tags["latest"] != digestN(2) {
		t.Fatalf("build push tags %v", tags)
	}
	v, err = e.store.ImageVersion(ctx, img, digestN(3))
	if err != nil || v.Source != store.VersionFromBuild || v.BuildID == nil || *v.BuildID != b.ID || v.Revision == nil || *v.Revision != 1 || *v.CreatedBy != alice.ID {
		t.Fatalf("build version: %+v %v", v, err)
	}
	if len(fm.deleted) != 0 {
		t.Fatalf("deleted %v", fm.deleted)
	}
	e.api.tags.wait()
	if fm.tag(repo, "latest") != digestN(2) || fm.tag(repo, "build-1") != "" {
		t.Fatalf("registry latest %s, want %s", fm.tag(repo, "latest"), digestN(2))
	}
}

func TestPushEventIgnoresHubTagMirror(t *testing.T) {
	e := newEnv(t)
	_, aliceTok := e.user("alice")
	org := e.createOrg(aliceTok, "Acme")
	img := e.createImage(aliceTok, org, `{"name":"a","scope":"org"}`)
	fm := &fakeManifests{manifests: map[string]registry.Manifest{digestN(1): amd64Manifest()}}
	if code := pushEvent(t, e.eventsHandler(fm), eventsSecret, "i/"+img.String(), digestN(1), "v1", registry.HubSubject); code != 200 {
		t.Fatalf("push: %d", code)
	}
	if _, err := e.store.ImageVersion(context.Background(), img, digestN(1)); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("hub push became a version: %v", err)
	}
}

func TestPushEventKeepsVersionWhenMirrorFails(t *testing.T) {
	e := newEnv(t)
	alice, aliceTok := e.user("alice")
	org := e.createOrg(aliceTok, "Acme")
	img := e.createImage(aliceTok, org, `{"name":"a","scope":"org"}`)
	fm := &fakeManifests{manifests: map[string]registry.Manifest{digestN(1): amd64Manifest()}, putErr: errors.New("registry down")}
	if code := pushEvent(t, e.eventsHandler(fm), eventsSecret, "i/"+img.String(), digestN(1), "v1", "user:"+alice.ID.String()); code != 200 {
		t.Fatalf("push: %d", code)
	}
	if tags, _ := e.store.ImageTags(context.Background(), img); tags["latest"] != digestN(1) {
		t.Fatalf("tags %v", tags)
	}
}

func TestPushEventRejectsArm64(t *testing.T) {
	e := newEnv(t)
	alice, aliceTok := e.user("alice")
	org := e.createOrg(aliceTok, "Acme")
	img := e.createImage(aliceTok, org, `{"name":"a","scope":"org"}`)
	repo := "i/" + img.String()
	old := e.addVersion(img, 1, "latest")
	fm := &fakeManifests{manifests: map[string]registry.Manifest{
		digestN(2): {Platform: "linux/arm64/v8", Layers: []registry.Layer{{Digest: "sha256:l", Size: 1}}},
		digestN(3): {Platform: "linux/arm64, unknown/unknown"},
	}}
	h := e.eventsHandler(fm)
	ctx := context.Background()
	for _, c := range []struct{ digest, want string }{
		{digestN(2), "image platform linux/arm64/v8 is not linux/amd64"},
		{digestN(3), "image platform linux/arm64, unknown/unknown is not linux/amd64"},
	} {
		if code := pushEvent(t, h, eventsSecret, repo, c.digest, "latest", "user:"+alice.ID.String()); code != 200 {
			t.Fatalf("push: %d", code)
		}
		if !slices.Contains(fm.deleted, repo+"@"+c.digest) {
			t.Fatalf("manifest not deleted: %v", fm.deleted)
		}
		if got := e.pushError(img); got != c.want {
			t.Fatalf("push error %q", got)
		}
		if _, err := e.store.ImageVersion(ctx, img, c.digest); err != store.ErrNotFound {
			t.Fatalf("version recorded: %v", err)
		}
	}
	e.api.tags.wait()
	if got := fm.tag(repo, "latest"); got != old {
		t.Fatalf("registry latest %s after the rejected push, want %s", got, old)
	}
	if d, _ := e.store.ResolveImageTag(ctx, img, "latest"); d != old {
		t.Fatalf("latest moved to %s", d)
	}
}

func TestPushEventOverLimit(t *testing.T) {
	e := newEnv(t)
	alice, aliceTok := e.user("alice")
	org := e.createOrg(aliceTok, "Acme")
	img := e.createImage(aliceTok, org, `{"name":"a","scope":"org"}`)
	repo := "i/" + img.String()
	ctx := context.Background()
	one := 1
	if err := e.store.SetLimits(ctx, org, store.ScopeOrg, store.Limits{ImageStorageGB: &one}); err != nil {
		t.Fatal(err)
	}
	old := e.addVersion(img, 1, "latest")
	e.exec(`INSERT INTO image_version_layers (image_id, version_digest, digest, size) VALUES ($1, $2, 'sha256:base', $3)`, img, old, 1<<29)
	fm := &fakeManifests{manifests: map[string]registry.Manifest{
		// Shares the base layer, so only the second layer counts: exactly at the limit.
		digestN(2): amd64Manifest(registry.Layer{Digest: "sha256:base", Size: 1 << 29}, registry.Layer{Digest: "sha256:a", Size: 1 << 29}),
		digestN(3): amd64Manifest(registry.Layer{Digest: "sha256:b", Size: 1}),
		old:        amd64Manifest(registry.Layer{Digest: "sha256:base", Size: 1 << 29}),
	}}
	h := e.eventsHandler(fm)
	if code := pushEvent(t, h, eventsSecret, repo, digestN(2), "v2", "user:"+alice.ID.String()); code != 200 {
		t.Fatalf("push at limit: %d", code)
	}
	if _, err := e.store.ImageVersion(ctx, img, digestN(2)); err != nil {
		t.Fatalf("at limit: %v", err)
	}
	if code := pushEvent(t, h, eventsSecret, repo, digestN(3), "v3", "user:"+alice.ID.String()); code != 200 {
		t.Fatalf("push over limit: %d", code)
	}
	if _, err := e.store.ImageVersion(ctx, img, digestN(3)); err != store.ErrNotFound {
		t.Fatalf("over limit recorded: %v", err)
	}
	if !slices.Equal(fm.deleted, []string{repo + "@" + digestN(3)}) {
		t.Fatalf("deleted %v", fm.deleted)
	}
	if got := e.pushError(img); got != "image storage limit exceeded (1 GB)" {
		t.Fatalf("push error %q", got)
	}
	if d, _ := e.store.ResolveImageTag(ctx, img, "latest"); d != digestN(2) {
		t.Fatalf("latest moved to %s", d)
	}
	// Retagging an existing version adds no storage, even over the limit.
	e.exec(`UPDATE image_version_layers SET size = size + 1 WHERE digest = 'sha256:a'`)
	if code := pushEvent(t, h, eventsSecret, repo, old, "again", "user:"+alice.ID.String()); code != 200 {
		t.Fatalf("retag: %d", code)
	}
	if d, _ := e.store.ResolveImageTag(ctx, img, "again"); d != old || len(fm.deleted) != 1 {
		t.Fatalf("retag over limit: %s %v", d, fm.deleted)
	}
}

func TestPushEventDeletedImage(t *testing.T) {
	e := newEnv(t)
	_, aliceTok := e.user("alice")
	org := e.createOrg(aliceTok, "Acme")
	img := e.createImage(aliceTok, org, `{"name":"a","scope":"org"}`)
	if err := e.store.DeleteImage(context.Background(), org, img); err != nil {
		t.Fatal(err)
	}
	repo := "i/" + img.String()
	fm := &fakeManifests{manifests: map[string]registry.Manifest{digestN(1): amd64Manifest()}}
	h := e.eventsHandler(fm)
	if code := pushEvent(t, h, eventsSecret, repo, digestN(1), "latest", ""); code != 200 {
		t.Fatalf("push: %d", code)
	}
	if !slices.Equal(fm.deleted, []string{repo + "@" + digestN(1)}) {
		t.Fatalf("deleted %v", fm.deleted)
	}
	if code := pushEvent(t, h, eventsSecret, "library/alpine", digestN(1), "latest", ""); code != 200 || len(fm.deleted) != 1 {
		t.Fatalf("foreign repo: %d %v", code, fm.deleted)
	}
}

func TestEventsNeedSecret(t *testing.T) {
	e := newEnv(t)
	_, aliceTok := e.user("alice")
	org := e.createOrg(aliceTok, "Acme")
	img := e.createImage(aliceTok, org, `{"name":"a","scope":"org"}`)
	fm := &fakeManifests{manifests: map[string]registry.Manifest{digestN(1): amd64Manifest()}}
	h := e.eventsHandler(fm)
	for _, secret := range []string{"", "wrong", eventsSecret + "x"} {
		if code := pushEvent(t, h, secret, "i/"+img.String(), digestN(1), "latest", ""); code != 401 {
			t.Fatalf("secret %q: %d", secret, code)
		}
	}
	if vs, _ := e.store.ImageVersions(context.Background(), img); len(vs) != 0 {
		t.Fatalf("versions %v", vs)
	}
}

func TestPushEventIndexCountsEveryChild(t *testing.T) {
	e := newEnv(t)
	alice, aliceTok := e.user("alice")
	org := e.createOrg(aliceTok, "Acme")
	img := e.createImage(aliceTok, org, `{"name":"a","scope":"org"}`)
	repo := "i/" + img.String()
	ctx := context.Background()
	one := 1
	if err := e.store.SetLimits(ctx, org, store.ScopeOrg, store.Limits{ImageStorageGB: &one}); err != nil {
		t.Fatal(err)
	}
	amd, arm := registry.Layer{Digest: "sha256:amd", Size: 10}, registry.Layer{Digest: "sha256:arm", Size: 1 << 29}
	fm := &fakeManifests{manifests: map[string]registry.Manifest{
		digestN(1): {Platform: "linux/amd64", PlatformDigest: digestN(9), Layers: []registry.Layer{amd}, Blobs: []registry.Layer{arm, amd}},
		digestN(2): {Platform: "linux/amd64", Layers: []registry.Layer{amd}, Blobs: []registry.Layer{{Digest: "sha256:arm2", Size: 1<<29 + 1}, amd}},
	}}
	h := e.eventsHandler(fm)
	if code := pushEvent(t, h, eventsSecret, repo, digestN(1), "multi", "user:"+alice.ID.String()); code != 200 {
		t.Fatalf("push: %d", code)
	}
	v, err := e.store.ImageVersion(ctx, img, digestN(1))
	if used, _ := e.store.OrgImageUsage(ctx, org); err != nil || v.SizeBytes != 10 || v.PlatformDigest != digestN(9) || used != 1<<29+10 {
		t.Fatalf("version %+v %v, usage %d", v, err, used)
	}
	// Only the arm64 child crosses the limit.
	if code := pushEvent(t, h, eventsSecret, repo, digestN(2), "multi2", "user:"+alice.ID.String()); code != 200 {
		t.Fatalf("push 2: %d", code)
	}
	if got := e.pushError(img); got != "image storage limit exceeded (1 GB)" || !slices.Equal(fm.deleted, []string{repo + "@" + digestN(2)}) {
		t.Fatalf("push error %q, deleted %v", got, fm.deleted)
	}
}

func TestPushEventUndecodableConfig(t *testing.T) {
	e := newEnv(t)
	alice, aliceTok := e.user("alice")
	org := e.createOrg(aliceTok, "Acme")
	img := e.createImage(aliceTok, org, `{"name":"a","scope":"org"}`)
	repo := "i/" + img.String()
	fm := &fakeManifests{errs: map[string]error{
		digestN(1): fmt.Errorf("get %s/blobs/sha256:c: invalid character 'x': %w", repo, registry.ErrInvalid),
		digestN(2): errors.New("registry down"),
	}}
	h := e.eventsHandler(fm)
	if code := pushEvent(t, h, eventsSecret, repo, digestN(1), "v1", "user:"+alice.ID.String()); code != 200 {
		t.Fatalf("push: %d", code)
	}
	if !slices.Equal(fm.deleted, []string{repo + "@" + digestN(1)}) {
		t.Fatalf("deleted %v", fm.deleted)
	}
	if got := e.pushError(img); !strings.HasPrefix(got, "unreadable image: ") {
		t.Fatalf("push error %q", got)
	}
	// A registry outage is retried by redelivery instead.
	if code := pushEvent(t, h, eventsSecret, repo, digestN(2), "v2", "user:"+alice.ID.String()); code != 500 {
		t.Fatalf("outage: %d", code)
	}
}

func TestEventsUnreadableBatchAccepted(t *testing.T) {
	e := newEnv(t)
	h := e.eventsHandler(&fakeManifests{})
	for _, body := range []string{"{not json", `{"events":[` + strings.Repeat(" ", maxEventsBody) + `]}`} {
		req := httptest.NewRequest("POST", "/internal/registry/events", strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+eventsSecret)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != 200 {
			t.Fatalf("code %d", rec.Code)
		}
	}
}

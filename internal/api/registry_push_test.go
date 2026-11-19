package api

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"testing"
	"time"

	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/name"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/empty"
	"github.com/google/go-containerregistry/pkg/v1/mutate"
	"github.com/google/go-containerregistry/pkg/v1/random"
	"github.com/google/go-containerregistry/pkg/v1/remote"

	"github.com/asterism-dev/observatory/internal/registry"
	"github.com/asterism-dev/observatory/internal/store"
)

// testHubAddr is where the dev registry sends events when started with HUB_URL=http://host.docker.internal:55090.
const testHubAddr = "127.0.0.1:55090"

func platformImage(t *testing.T, arch string) v1.Image {
	t.Helper()
	img, err := random.Image(1024, 1)
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := img.ConfigFile()
	if err != nil {
		t.Fatal(err)
	}
	cfg.OS, cfg.Architecture = "linux", arch
	if img, err = mutate.ConfigFile(img, cfg); err != nil {
		t.Fatal(err)
	}
	return img
}

func mustRef(t *testing.T, s string) name.Reference {
	t.Helper()
	r, err := name.ParseReference(s)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

// TestRegistryPushRoundTrip needs the dev registry (make registry-up with the HUB_URL above) and deploy/dev/hub.env sourced.
func TestRegistryPushRoundTrip(t *testing.T) {
	regURL := os.Getenv("OBSERVATORY_TEST_REGISTRY_URL")
	if regURL == "" {
		t.Skip("OBSERVATORY_TEST_REGISTRY_URL not set")
	}
	e := newEnv(t)
	alice, aliceTok := e.user("alice")
	org := e.createOrg(aliceTok, "Acme")
	id := e.createImage(aliceTok, org, `{"name":"a","scope":"org"}`)
	iss, err := registry.LoadIssuer(os.Getenv("OBSERVATORY_REGISTRY_KEY_DIR"))
	if err != nil {
		t.Fatal(err)
	}
	if err := iss.WriteCert(os.Getenv("OBSERVATORY_REGISTRY_CERT_DIR")); err != nil {
		t.Fatal(err)
	}
	upstream, _ := url.Parse(regURL)
	mux := http.NewServeMux()
	RegistryEndpoints(mux, e.store, e.z, Registry{Upstream: upstream, PublicURL: &url.URL{Scheme: "http", Host: testHubAddr}, Issuer: iss,
		Manifests: registry.Client{URL: regURL, Issuer: iss}, EventsSecret: os.Getenv("OBSERVATORY_REGISTRY_EVENTS_SECRET")})
	ln, err := net.Listen("tcp", ":55090")
	if err != nil {
		t.Fatal(err)
	}
	hub := httptest.NewUnstartedServer(mux)
	hub.Listener = ln
	hub.Start()
	defer hub.Close()

	auth := remote.WithAuth(&authn.Basic{Username: "observatory", Password: e.registryCredential(e.cliSession(alice))})
	repo := testHubAddr + "/i/" + id.String()
	ctx := context.Background()
	waitVersion := func(digest string) {
		t.Helper()
		for deadline := time.Now().Add(15 * time.Second); time.Now().Before(deadline); time.Sleep(200 * time.Millisecond) {
			if _, err := e.store.ImageVersion(ctx, id, digest); err == nil {
				return
			}
		}
		t.Fatalf("no version %s", digest)
	}

	amd64 := platformImage(t, "amd64")
	if err := remote.Write(mustRef(t, repo+":v1"), amd64, auth); err != nil {
		t.Fatal(err)
	}
	d1, _ := amd64.Digest()
	waitVersion(d1.String())
	if tag, _ := e.store.ResolveImageTag(ctx, id, "latest"); tag != d1.String() {
		t.Fatalf("latest %s", tag)
	}

	armChild := platformImage(t, "arm64")
	idx := mutate.AppendManifests(empty.Index,
		mutate.IndexAddendum{Add: armChild, Descriptor: v1.Descriptor{Platform: &v1.Platform{OS: "linux", Architecture: "arm64"}}},
		mutate.IndexAddendum{Add: amd64, Descriptor: v1.Descriptor{Platform: &v1.Platform{OS: "linux", Architecture: "amd64"}}})
	if err := remote.WriteIndex(mustRef(t, repo+":multi"), idx, auth); err != nil {
		t.Fatal(err)
	}
	d2, _ := idx.Digest()
	waitVersion(d2.String())
	v, _ := e.store.ImageVersion(ctx, id, d2.String())
	layers, _ := amd64.Layers()
	if size, _ := layers[0].Size(); v.Source != store.VersionFromPush || v.SizeBytes != size || v.PlatformDigest != d1.String() {
		t.Fatalf("index version %+v", v)
	}
	armLayers, _ := armChild.Layers()
	amdSize, _ := layers[0].Size()
	armSize, _ := armLayers[0].Size()
	if used, _ := e.store.OrgImageUsage(ctx, org); used != amdSize+armSize {
		t.Fatalf("usage %d, want both children %d", used, amdSize+armSize)
	}

	arm := platformImage(t, "arm64")
	if err := remote.Write(mustRef(t, repo+":arm"), arm, auth); err != nil {
		t.Fatal(err)
	}
	d3, _ := arm.Digest()
	for deadline := time.Now().Add(15 * time.Second); e.pushError(id) == ""; time.Sleep(200 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("arm64 push not rejected")
		}
	}
	if got := e.pushError(id); got != "image platform linux/arm64 is not linux/amd64" {
		t.Fatalf("push error %q", got)
	}
	if _, err := remote.Head(mustRef(t, repo+"@"+d3.String()), auth); err == nil {
		t.Fatal("arm64 manifest still in the registry")
	}
	if tag, _ := e.store.ResolveImageTag(ctx, id, "latest"); tag != d2.String() {
		t.Fatalf("latest moved to %s", tag)
	}
}

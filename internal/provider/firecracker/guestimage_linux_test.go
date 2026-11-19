package firecracker

import (
	"context"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/registry"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/mutate"
	"github.com/google/go-containerregistry/pkg/v1/random"
	"github.com/google/go-containerregistry/pkg/v1/remote"

	"github.com/asterism-dev/observatory/internal/node"
	"github.com/asterism-dev/observatory/internal/node/ociimage"
	"github.com/asterism-dev/observatory/internal/proto"
)

func imageProvider(t *testing.T, ref string) *Provider {
	srv := httptest.NewServer(registry.New(registry.Logger(log.New(io.Discard, "", 0))))
	t.Cleanup(srv.Close)
	return &Provider{imageRef: strings.TrimPrefix(srv.URL, "http://") + "/" + ref, store: ociimage.Store{Dir: filepath.Join(t.TempDir(), "oci")}}
}

// runImages runs RunGuestImages until it reports ready or failed.
func runImages(t *testing.T, p *Provider) []proto.ImageStatus {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var got []proto.ImageStatus
	p.RunGuestImages(ctx, func(st proto.ImageStatus) {
		got = append(got, st)
		if st.State != proto.ImagePulling {
			cancel()
		}
	})
	return got
}

func TestPrepareUserImagePullsWithToken(t *testing.T) {
	reg := registry.New(registry.Logger(log.New(io.Discard, "", 0)))
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer t1" {
			w.Header().Set("WWW-Authenticate", `Bearer realm="http://`+r.Host+`/token",service="test"`)
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		reg.ServeHTTP(w, r)
	}))
	t.Cleanup(srv.Close)
	repo := strings.TrimPrefix(srv.URL, "http://") + "/i/img1"
	img, err := random.Image(1024, 1)
	if err != nil {
		t.Fatal(err)
	}
	cf, err := img.ConfigFile()
	if err != nil {
		t.Fatal(err)
	}
	cf = cf.DeepCopy()
	cf.OS, cf.Architecture, cf.Config = "linux", "amd64", v1.Config{Cmd: []string{"app"}}
	if img, err = mutate.ConfigFile(img, cf); err != nil {
		t.Fatal(err)
	}
	ref, err := name.ParseReference(repo + ":latest")
	if err != nil {
		t.Fatal(err)
	}
	if err := remote.Write(ref, img, remote.WithAuth(authn.FromConfig(authn.AuthConfig{RegistryToken: "t1"}))); err != nil {
		t.Fatal(err)
	}
	digest, _ := img.Digest()
	p := &Provider{VMsDir: t.TempDir(), store: ociimage.Store{Dir: filepath.Join(t.TempDir(), "oci")}}

	for _, tok := range []string{"", "t2"} {
		if _, _, err := p.PullImage(context.Background(), proto.ImageRef{Repo: repo, Digest: digest.String(), PullToken: tok}); err == nil || !strings.Contains(strings.ToLower(err.Error()), "unauthorized") {
			t.Fatalf("token %q: err = %v", tok, err)
		}
	}
	if os.Geteuid() != 0 {
		t.Skip("needs root")
	}
	if _, err := exec.LookPath("mke2fs"); err != nil {
		t.Skip("mke2fs not installed")
	}
	got, cfg, err := p.PullImage(context.Background(), proto.ImageRef{Repo: repo, Digest: digest.String(), PullToken: "t1"})
	if err != nil {
		t.Fatal(err)
	}
	if got != repo+"@"+digest.String() || !reflect.DeepEqual(cfg, proto.ImageConfig{Cmd: []string{"app"}}) {
		t.Fatalf("pulled %q, config %+v", got, cfg)
	}
	if _, err := os.Stat(filepath.Join(p.store.Dir, "disks", digest.Hex+".raw")); err != nil {
		t.Fatalf("root disk: %v", err)
	}
}

func TestGuestImageFailedPullRetries(t *testing.T) {
	p := imageProvider(t, "missing:1")
	got := runImages(t, p)
	if len(got) != 2 || got[0].State != proto.ImagePulling || got[0].Version != p.imageRef || got[1].State != proto.ImageFailed || got[1].Error == "" || got[1].RetryAt == nil {
		t.Fatalf("states = %+v", got)
	}
}

func TestGuestImageReady(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("needs root")
	}
	if _, err := exec.LookPath("mke2fs"); err != nil {
		t.Skip("mke2fs not installed")
	}
	p := imageProvider(t, "guest:1")
	img, err := random.Image(1024, 1)
	if err != nil {
		t.Fatal(err)
	}
	cf, err := img.ConfigFile()
	if err != nil {
		t.Fatal(err)
	}
	cf = cf.DeepCopy()
	cf.OS, cf.Architecture, cf.Config = "linux", "amd64", v1.Config{Cmd: []string{"sleep", "1"}, Env: []string{"A=b"}}
	if img, err = mutate.ConfigFile(img, cf); err != nil {
		t.Fatal(err)
	}
	ref, err := name.ParseReference(p.imageRef)
	if err != nil {
		t.Fatal(err)
	}
	if err := remote.Write(ref, img); err != nil {
		t.Fatal(err)
	}
	digest, err := img.Digest()
	if err != nil {
		t.Fatal(err)
	}
	got := runImages(t, p)
	last := got[len(got)-1]
	if len(got) != 2 || last.State != proto.ImageReady || last.Version != p.imageRef+"@"+digest.String() {
		t.Fatalf("states = %+v", got)
	}
	if want := (proto.ImageConfig{Cmd: []string{"sleep", "1"}, Env: []string{"A=b"}}); !reflect.DeepEqual(last.Config, want) {
		t.Fatalf("config = %+v", last.Config)
	}
	base, err := p.baseFor(context.Background(), node.VMFile{Image: last.Version})
	if err != nil {
		t.Fatal(err)
	}
	if st, err := os.Stat(base); err != nil || st.Size() == 0 {
		t.Fatalf("root disk: %v %v", st, err)
	}
}

func TestPruneKeepsReferenced(t *testing.T) {
	digest := func(c string) string { return "sha256:" + strings.Repeat(c, 64) }
	p := &Provider{VMsDir: t.TempDir(), platform: digest("a")}
	vms := []node.VMFile{
		{Spec: proto.VMSpec{ID: "user", Image: &proto.ImageRef{Repo: "hub/i/x", Digest: digest("b")}}, NeedsPrepare: true},
		{Spec: proto.VMSpec{ID: "pinned"}, Image: "hub/i/y@" + digest("c"), NeedsPrepare: true},
		{Spec: proto.VMSpec{ID: "prepared", Image: &proto.ImageRef{Repo: "hub/i/z", Digest: digest("d")}}, Image: "hub/i/z@" + digest("d")},
	}
	for _, f := range vms {
		if err := node.SaveVM(p.VMsDir, f); err != nil {
			t.Fatal(err)
		}
	}
	tpl := p.templateDir("0123456789abcdef")
	if err := os.MkdirAll(tpl, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(tpl, templateImage), []byte(digest("e")), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := p.keptImages()
	if err != nil {
		t.Fatal(err)
	}
	slices.Sort(got)
	if want := []string{digest("a"), digest("b"), digest("c"), digest("e")}; !slices.Equal(got, want) {
		t.Fatalf("kept %v, want %v", got, want)
	}
}

func TestPruneSkipsWhilePulling(t *testing.T) {
	p := &Provider{VMsDir: t.TempDir(), store: ociimage.Store{Dir: t.TempDir()}}
	orphan := filepath.Join(p.store.Dir, "blobs", "sha256", strings.Repeat("a", 64))
	if err := os.MkdirAll(filepath.Dir(orphan), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(orphan, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	p.storeMu.RLock()
	done := make(chan struct{})
	go func() { p.prune(); close(done) }()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("prune waited for the pull")
	}
	p.storeMu.RUnlock()
	if _, err := os.Stat(orphan); err != nil {
		t.Fatalf("pruned while pulling: %v", err)
	}
	p.prune()
	if _, err := os.Stat(orphan); !os.IsNotExist(err) {
		t.Fatalf("orphan left after prune: %v", err)
	}
}

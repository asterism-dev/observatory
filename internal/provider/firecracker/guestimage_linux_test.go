package firecracker

import (
	"context"
	"io"
	"log"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

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

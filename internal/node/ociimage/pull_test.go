package ociimage

import (
	"context"
	"io"
	"log"
	"net/http/httptest"
	"os"
	"reflect"
	"strings"
	"testing"

	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/registry"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/empty"
	"github.com/google/go-containerregistry/pkg/v1/mutate"
	"github.com/google/go-containerregistry/pkg/v1/random"
	"github.com/google/go-containerregistry/pkg/v1/remote"

	"github.com/asterism-dev/observatory/internal/proto"
)

func testRegistry(t *testing.T) string {
	srv := httptest.NewServer(registry.New(registry.Logger(log.New(io.Discard, "", 0))))
	t.Cleanup(srv.Close)
	return strings.TrimPrefix(srv.URL, "http://")
}

func platformImage(t *testing.T, arch string, cfg v1.Config) v1.Image {
	img, err := random.Image(1024, 2)
	if err != nil {
		t.Fatal(err)
	}
	cf, err := img.ConfigFile()
	if err != nil {
		t.Fatal(err)
	}
	cf = cf.DeepCopy()
	cf.OS, cf.Architecture, cf.Config = "linux", arch, cfg
	img, err = mutate.ConfigFile(img, cf)
	if err != nil {
		t.Fatal(err)
	}
	return img
}

func pushIndex(t *testing.T, ref string, imgs map[string]v1.Image) {
	var idx v1.ImageIndex = empty.Index
	for arch, img := range imgs {
		idx = mutate.AppendManifests(idx, mutate.IndexAddendum{
			Add:        img,
			Descriptor: v1.Descriptor{Platform: &v1.Platform{OS: "linux", Architecture: arch}},
		})
	}
	if err := remote.WriteIndex(mustRef(t, ref), idx); err != nil {
		t.Fatal(err)
	}
}

func TestPullPicksAmd64(t *testing.T) {
	ref := testRegistry(t) + "/guest:1"
	amd, arm := platformImage(t, "amd64", v1.Config{}), platformImage(t, "arm64", v1.Config{})
	pushIndex(t, ref, map[string]v1.Image{"arm64": arm, "amd64": amd})
	s := Store{Dir: t.TempDir()}
	got, err := s.Pull(context.Background(), ref, authn.Anonymous)
	if err != nil {
		t.Fatal(err)
	}
	want, _ := amd.Digest()
	if got.Digest != want.String() {
		t.Fatalf("digest %s, want amd64 %s", got.Digest, want)
	}
	layers, _ := amd.Layers()
	var size int64
	for _, l := range layers {
		d, _ := l.Digest()
		n, _ := l.Size()
		size += n
		if _, err := os.Stat(s.blobPath(d)); err != nil {
			t.Fatalf("layer not cached: %v", err)
		}
	}
	if got.SizeBytes != size {
		t.Fatalf("size %d, want %d", got.SizeBytes, size)
	}
	local, err := s.image(want)
	if err != nil {
		t.Fatal(err)
	}
	if d, _ := local.Digest(); d != want {
		t.Fatalf("local image digest %s", d)
	}
	if _, err := s.Pull(context.Background(), ref, authn.Anonymous); err != nil {
		t.Fatalf("second pull: %v", err)
	}
}

func TestPullRejectsArm64Only(t *testing.T) {
	ref := testRegistry(t) + "/guest:1"
	pushIndex(t, ref, map[string]v1.Image{"arm64": platformImage(t, "arm64", v1.Config{})})
	_, err := Store{Dir: t.TempDir()}.Pull(context.Background(), ref, authn.Anonymous)
	if err == nil || err.Error() != "image platform linux/arm64 is not linux/amd64" {
		t.Fatalf("err = %v", err)
	}

	single := testRegistry(t) + "/single:1"
	if err := remote.Write(mustRef(t, single), platformImage(t, "arm64", v1.Config{})); err != nil {
		t.Fatal(err)
	}
	_, err = Store{Dir: t.TempDir()}.Pull(context.Background(), single, authn.Anonymous)
	if err == nil || err.Error() != "image platform linux/arm64 is not linux/amd64" {
		t.Fatalf("single-arch err = %v", err)
	}
}

func TestPullConfig(t *testing.T) {
	ref := testRegistry(t) + "/guest:1"
	img := platformImage(t, "amd64", v1.Config{
		Entrypoint:   []string{"/bin/app"},
		Cmd:          []string{"--serve"},
		Env:          []string{"PATH=/usr/bin", "MODE=prod"},
		WorkingDir:   "/srv",
		StopSignal:   "SIGINT",
		User:         "nobody",
		ExposedPorts: map[string]struct{}{"80/tcp": {}},
	})
	if err := remote.Write(mustRef(t, ref), img); err != nil {
		t.Fatal(err)
	}
	got, err := Store{Dir: t.TempDir()}.Pull(context.Background(), ref, authn.Anonymous)
	if err != nil {
		t.Fatal(err)
	}
	want := proto.ImageConfig{
		Entrypoint: []string{"/bin/app"},
		Cmd:        []string{"--serve"},
		Env:        []string{"PATH=/usr/bin", "MODE=prod"},
		WorkingDir: "/srv",
		StopSignal: "SIGINT",
	}
	if !reflect.DeepEqual(got.Config, want) {
		t.Fatalf("config %+v, want %+v", got.Config, want)
	}
}

func TestWriteBlobRejectsMismatch(t *testing.T) {
	s := Store{Dir: t.TempDir()}
	if err := os.MkdirAll(s.blobDir(), 0o700); err != nil {
		t.Fatal(err)
	}
	h, _ := v1.NewHash("sha256:" + strings.Repeat("ab", 32))
	if err := s.writeBlob(h, strings.NewReader("not it")); err == nil {
		t.Fatal("mismatching blob accepted")
	}
	if entries, _ := os.ReadDir(s.blobDir()); len(entries) != 0 {
		t.Fatalf("left %d files behind", len(entries))
	}
}

func TestPruneKeepsReferenced(t *testing.T) {
	reg := testRegistry(t)
	keep, drop := platformImage(t, "amd64", v1.Config{}), platformImage(t, "amd64", v1.Config{})
	s := Store{Dir: t.TempDir()}
	var pulled []string
	for i, img := range []v1.Image{keep, drop} {
		ref := reg + "/img:" + string(rune('a'+i))
		if err := remote.Write(mustRef(t, ref), img); err != nil {
			t.Fatal(err)
		}
		p, err := s.Pull(context.Background(), ref, authn.Anonymous)
		if err != nil {
			t.Fatal(err)
		}
		pulled = append(pulled, p.Digest)
	}
	if err := s.Prune(pulled[:1]); err != nil {
		t.Fatal(err)
	}
	if _, err := s.image(mustHash(pulled[0])); err != nil {
		t.Fatalf("kept image gone: %v", err)
	}
	if _, err := s.image(mustHash(pulled[1])); err == nil {
		t.Fatal("dropped image still cached")
	}
	entries, _ := os.ReadDir(s.blobDir())
	if len(entries) != 4 { // manifest, config, 2 layers
		t.Fatalf("%d blobs left, want 4", len(entries))
	}
}

func mustHash(s string) v1.Hash {
	h, err := v1.NewHash(s)
	if err != nil {
		panic(err)
	}
	return h
}

func mustRef(t *testing.T, s string) name.Reference {
	r, err := name.ParseReference(s)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

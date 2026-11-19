package ociimage

import (
	"context"
	"encoding/json"
	"io"
	"log"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/registry"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/empty"
	"github.com/google/go-containerregistry/pkg/v1/mutate"
	"github.com/google/go-containerregistry/pkg/v1/random"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"github.com/google/go-containerregistry/pkg/v1/types"

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

func TestPullByDigestUsesCache(t *testing.T) {
	srv := httptest.NewServer(registry.New(registry.Logger(log.New(io.Discard, "", 0))))
	defer srv.Close()
	img := platformImage(t, "amd64", v1.Config{Cmd: []string{"/bin/app"}})
	d, _ := img.Digest()
	ref := strings.TrimPrefix(srv.URL, "http://") + "/img@" + d.String()
	if err := remote.Write(mustRef(t, ref), img); err != nil {
		t.Fatal(err)
	}
	s := Store{Dir: t.TempDir()}
	first, err := s.Pull(context.Background(), ref, authn.Anonymous)
	if err != nil {
		t.Fatal(err)
	}
	srv.Close()
	again, err := s.Pull(context.Background(), ref, authn.Anonymous)
	if err != nil {
		t.Fatalf("cached pull contacted the registry: %v", err)
	}
	if !reflect.DeepEqual(again, first) {
		t.Fatalf("cached pull %+v, want %+v", again, first)
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

type rawManifest struct {
	raw []byte
	mt  types.MediaType
}

func (r rawManifest) RawManifest() ([]byte, error)        { return r.raw, nil }
func (r rawManifest) MediaType() (types.MediaType, error) { return r.mt, nil }

func TestPullRejectsHugeConfig(t *testing.T) {
	ref := testRegistry(t) + "/guest:1"
	img := platformImage(t, "amd64", v1.Config{})
	if err := remote.Write(mustRef(t, ref), img); err != nil {
		t.Fatal(err)
	}
	m, _ := img.Manifest()
	m = m.DeepCopy()
	m.Config.Size = 5 << 20
	raw, _ := json.Marshal(m)
	if err := remote.Put(mustRef(t, ref), rawManifest{raw, m.MediaType}); err != nil {
		t.Fatal(err)
	}
	_, err := Store{Dir: t.TempDir()}.Pull(context.Background(), ref, authn.Anonymous)
	if err == nil || !strings.Contains(err.Error(), "image config of 5242880 bytes exceeds 4194304") {
		t.Fatalf("err = %v", err)
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

func TestPruneLRU(t *testing.T) {
	reg := testRegistry(t)
	s := Store{Dir: t.TempDir()}
	if err := os.MkdirAll(filepath.Join(s.Dir, "disks"), 0o700); err != nil {
		t.Fatal(err)
	}
	var digests []string
	for i := range 13 {
		ref := reg + "/img:" + strconv.Itoa(i)
		if err := remote.Write(mustRef(t, ref), platformImage(t, "amd64", v1.Config{})); err != nil {
			t.Fatal(err)
		}
		p, err := s.Pull(context.Background(), ref, authn.Anonymous)
		if err != nil {
			t.Fatal(err)
		}
		digests = append(digests, p.Digest)
		disk := filepath.Join(s.Dir, "disks", mustHash(p.Digest).Hex+".raw")
		if err := os.WriteFile(disk, nil, 0o600); err != nil {
			t.Fatal(err)
		}
		used := time.Now().Add(time.Duration(i-20) * time.Minute)
		if err := os.Chtimes(disk, used, used); err != nil {
			t.Fatal(err)
		}
	}
	cached := func() (got []int) {
		for i, d := range digests {
			_, err := s.image(mustHash(d))
			_, derr := os.Stat(filepath.Join(s.Dir, "disks", mustHash(d).Hex+".raw"))
			if (err == nil) != (derr == nil) {
				t.Fatalf("image %d: manifest %v, disk %v", i, err, derr)
			}
			if err == nil {
				got = append(got, i)
			}
		}
		return got
	}
	fsSpace = func(string) (int64, int64, error) { return 50, 100, nil }
	// The oldest unused images 1 and 2 go, leaving 10 besides the kept 0.
	if err := s.Prune([]string{digests[0], "not a digest"}); err != nil {
		t.Fatal(err)
	}
	if got, want := cached(), []int{0, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12}; !slices.Equal(got, want) {
		t.Fatalf("cached %v, want %v", got, want)
	}
	// Low on space: unused images go, oldest first, until 20% are free.
	fsSpace = func(string) (int64, int64, error) {
		entries, _ := os.ReadDir(filepath.Join(s.Dir, "disks"))
		return 100 - int64(len(entries))*10, 100, nil
	}
	t.Cleanup(func() { fsSpace = statfsSpace })
	if err := s.Prune([]string{digests[0]}); err != nil {
		t.Fatal(err)
	}
	if got, want := cached(), []int{0, 6, 7, 8, 9, 10, 11, 12}; !slices.Equal(got, want) {
		t.Fatalf("cached %v, want %v", got, want)
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

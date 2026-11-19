package registry

import (
	"context"
	"errors"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/go-containerregistry/pkg/name"
	ggcrregistry "github.com/google/go-containerregistry/pkg/registry"
	"github.com/google/go-containerregistry/pkg/v1/random"
	"github.com/google/go-containerregistry/pkg/v1/remote"
)

func TestPutTagPointsTagAtDigest(t *testing.T) {
	srv := httptest.NewServer(ggcrregistry.New(ggcrregistry.Logger(log.New(io.Discard, "", 0))))
	defer srv.Close()
	host := strings.TrimPrefix(srv.URL, "http://")
	iss, err := LoadIssuer(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	c := Client{URL: srv.URL, Issuer: iss}
	ctx := context.Background()
	var digests []string
	for range 2 {
		img, err := random.Image(256, 1)
		if err != nil {
			t.Fatal(err)
		}
		d, _ := img.Digest()
		if err := remote.Write(ref(t, host+"/i/x@"+d.String()), img); err != nil {
			t.Fatal(err)
		}
		digests = append(digests, d.String())
	}
	for _, d := range digests {
		if err := c.PutTag(ctx, "i/x", "latest", d); err != nil {
			t.Fatal(err)
		}
		if err := c.PutTag(ctx, "i/x", "latest", d); err != nil {
			t.Fatalf("repeated put: %v", err)
		}
		desc, err := remote.Head(ref(t, host+"/i/x:latest"))
		if err != nil {
			t.Fatal(err)
		}
		if desc.Digest.String() != d {
			t.Fatalf("latest = %s, want %s", desc.Digest, d)
		}
	}
	if err := c.PutTag(ctx, "i/x", "v1", "sha256:"+strings.Repeat("0", 64)); err == nil {
		t.Fatal("tagging a missing manifest succeeded")
	}
}

func ref(t *testing.T, s string) name.Reference {
	t.Helper()
	r, err := name.ParseReference(s)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func TestManifestInvalidDocuments(t *testing.T) {
	big := `{"layers":[` + strings.Repeat(" ", maxDocument) + `]}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v2/i/x/manifests/sha256:badcfg":
			io.WriteString(w, `{"config":{"digest":"sha256:cfg"},"layers":[]}`)
		case "/v2/i/x/blobs/sha256:cfg":
			io.WriteString(w, "not json")
		case "/v2/i/x/manifests/sha256:big":
			io.WriteString(w, big)
		default:
			http.Error(w, "down", http.StatusServiceUnavailable)
		}
	}))
	defer srv.Close()
	iss, err := LoadIssuer(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	c := Client{URL: srv.URL, Issuer: iss}
	for _, d := range []string{"sha256:badcfg", "sha256:big"} {
		if _, err := c.Manifest(context.Background(), "i/x", d); !errors.Is(err, ErrInvalid) {
			t.Errorf("%s: %v", d, err)
		}
	}
	if _, err := c.Manifest(context.Background(), "i/x", "sha256:down"); err == nil || errors.Is(err, ErrInvalid) {
		t.Errorf("outage: %v", err)
	}
}

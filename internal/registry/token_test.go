package registry

import (
	"context"
	"crypto/x509"
	"encoding/base64"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
)

// parse verifies like the registry does: x5c leaf must chain to the root bundle (token.crt).
func parse(t *testing.T, dir, tok string) Claims {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(dir, "token.crt"))
	if err != nil {
		t.Fatal(err)
	}
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(raw) {
		t.Fatal("bad token.crt")
	}
	var c Claims
	_, err = jwt.ParseWithClaims(tok, &c, func(tk *jwt.Token) (any, error) {
		x5c, _ := tk.Header["x5c"].([]any)
		if len(x5c) != 1 {
			t.Fatalf("x5c: %v", tk.Header["x5c"])
		}
		der, err := base64.StdEncoding.DecodeString(x5c[0].(string))
		if err != nil {
			return nil, err
		}
		leaf, err := x509.ParseCertificate(der)
		if err != nil {
			return nil, err
		}
		if _, err := leaf.Verify(x509.VerifyOptions{Roots: roots, KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageAny}}); err != nil {
			return nil, err
		}
		return leaf.PublicKey, nil
	}, jwt.WithValidMethods([]string{"ES256"}), jwt.WithAudience(Service), jwt.WithIssuer(IssuerName))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	return c
}

func TestTokenSignatureVerifies(t *testing.T) {
	dir := t.TempDir()
	iss, err := LoadIssuer(dir)
	if err != nil {
		t.Fatal(err)
	}
	if fi, err := os.Stat(filepath.Join(dir, "token.key")); err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("token.key: %v %v", fi, err)
	}
	// A restart reuses the key, so tokens stay valid for the registry.
	again, err := LoadIssuer(dir)
	if err != nil || !again.Key.Equal(iss.Key) {
		t.Fatalf("reload: %v", err)
	}
	tok, err := again.Token("user:u1", []Access{{Type: "repository", Name: "i/x", Actions: []string{"pull", "push"}}}, TokenTTL)
	if err != nil {
		t.Fatal(err)
	}
	c := parse(t, dir, tok)
	if c.Subject != "user:u1" || len(c.Access) != 1 || c.Access[0].Name != "i/x" || !slices.Equal(c.Access[0].Actions, []string{"pull", "push"}) {
		t.Fatalf("claims: %+v", c)
	}
	if ttl := time.Until(c.ExpiresAt.Time); ttl > TokenTTL || ttl < TokenTTL-time.Minute {
		t.Fatalf("ttl %v", ttl)
	}

	org := uuid.New()
	tok, err = iss.NodePullToken(org, []string{"i/a", "i/b"})
	if err != nil {
		t.Fatal(err)
	}
	c = parse(t, dir, tok)
	if c.Subject != "node-org:"+org.String() || len(c.Access) != 2 || !slices.Equal(c.Access[1].Actions, []string{"pull"}) {
		t.Fatalf("node claims: %+v", c)
	}
	if ttl := time.Until(c.ExpiresAt.Time); ttl > NodePullTTL || ttl < NodePullTTL-time.Minute {
		t.Fatalf("node ttl %v", ttl)
	}
}

func TestClientManifestAndDelete(t *testing.T) {
	dir := t.TempDir()
	iss, err := LoadIssuer(dir)
	if err != nil {
		t.Fatal(err)
	}
	var deleted string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c := parse(t, dir, strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "))
		if c.Access[0].Name != "i/x" {
			t.Errorf("access %+v", c.Access)
		}
		switch r.Method + " " + r.URL.Path {
		case "GET /v2/i/x/manifests/sha256:m":
			_, _ = w.Write([]byte(`{"config":{"digest":"sha256:c"},"layers":[{"digest":"sha256:l1","size":7}]}`))
		case "GET /v2/i/x/blobs/sha256:c":
			_, _ = w.Write([]byte(`{"os":"linux","architecture":"arm64","variant":"v8"}`))
		case "GET /v2/i/x/manifests/sha256:m64":
			_, _ = w.Write([]byte(`{"config":{"digest":"sha256:c64"},"layers":[{"digest":"sha256:l2","size":9}]}`))
		case "GET /v2/i/x/blobs/sha256:c64":
			_, _ = w.Write([]byte(`{"os":"linux","architecture":"amd64"}`))
		case "GET /v2/i/x/manifests/sha256:idx":
			_, _ = w.Write([]byte(`{"manifests":[{"digest":"sha256:m","platform":{"os":"linux","architecture":"arm64","variant":"v8"}},
				{"digest":"sha256:m64","platform":{"os":"linux","architecture":"amd64"}}]}`))
		case "GET /v2/i/x/manifests/sha256:idxarm":
			_, _ = w.Write([]byte(`{"manifests":[{"digest":"sha256:m","platform":{"os":"linux","architecture":"arm64","variant":"v8"}},
				{"digest":"sha256:att","platform":{"os":"unknown","architecture":"unknown"}}]}`))
		case "DELETE /v2/i/x/manifests/sha256:m":
			deleted = c.Access[0].Actions[0]
			w.WriteHeader(http.StatusAccepted)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	cl := Client{URL: srv.URL + "/", Issuer: iss}
	ctx := context.Background()
	m, err := cl.Manifest(ctx, "i/x", "sha256:m")
	if err != nil || m.Platform != "linux/arm64/v8" || len(m.Layers) != 1 || m.Layers[0] != (Layer{"sha256:l1", 7}) {
		t.Fatalf("manifest %+v %v", m, err)
	}
	if m, err := cl.Manifest(ctx, "i/x", "sha256:idx"); err != nil || m.Platform != "linux/amd64" || len(m.Layers) != 1 || m.Layers[0] != (Layer{"sha256:l2", 9}) {
		t.Fatalf("index %+v %v", m, err)
	}
	if m, err := cl.Manifest(ctx, "i/x", "sha256:idxarm"); err != nil || m.Platform != "linux/arm64/v8, unknown/unknown" || m.Layers != nil {
		t.Fatalf("index without amd64 %+v %v", m, err)
	}
	if _, err := cl.Manifest(ctx, "i/x", "sha256:gone"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing manifest: %v", err)
	}
	if err := cl.DeleteManifest(ctx, "i/x", "sha256:m"); err != nil || deleted != "delete" {
		t.Fatalf("delete: %v %q", err, deleted)
	}
	if err := cl.DeleteManifest(ctx, "i/x", "sha256:gone"); err != nil {
		t.Fatalf("missing manifest: %v", err)
	}
}

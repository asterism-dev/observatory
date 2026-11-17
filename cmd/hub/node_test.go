package main

import (
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func get(mux *http.ServeMux, path string) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest("GET", path, nil))
	return rec
}

func TestNodeAssets(t *testing.T) {
	bin := filepath.Join(t.TempDir(), "node")
	if err := os.WriteFile(bin, []byte("binary"), 0o755); err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	nodeAssets(mux, bin)
	sum := sha256.Sum256([]byte("binary"))
	if rec := get(mux, "/node/SHA256SUMS"); rec.Body.String() != hex.EncodeToString(sum[:])+"  observatory-node\n" {
		t.Fatalf("sums: %q", rec.Body.String())
	}
	if rec := get(mux, "/node/observatory-node"); rec.Body.String() != "binary" {
		t.Fatalf("binary: %q", rec.Body.String())
	}
	if rec := get(mux, "/node/install.sh"); !strings.HasPrefix(rec.Body.String(), "#!") {
		t.Fatalf("install.sh: %q", rec.Body.String())
	}

	missing := http.NewServeMux()
	nodeAssets(missing, filepath.Join(t.TempDir(), "absent"))
	if get(missing, "/node/observatory-node").Code != 404 || get(missing, "/node/SHA256SUMS").Code != 404 || get(missing, "/node/install.sh").Code != 200 {
		t.Fatal("missing binary handling")
	}
}

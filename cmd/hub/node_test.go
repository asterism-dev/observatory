package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
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
	missing.Handle("/", http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("spa")) }))
	nodeAssets(missing, filepath.Join(t.TempDir(), "absent"))
	if get(missing, "/node/observatory-node").Code != 404 || get(missing, "/node/SHA256SUMS").Code != 404 || get(missing, "/node/install.sh").Code != 200 {
		t.Fatal("missing binary handling")
	}
}

func TestInstallTokenStdinRefusesPipedScript(t *testing.T) {
	cmd := exec.Command("sh", "-s", "--", "--hub", "http://127.0.0.1:9", "--token-stdin")
	cmd.Stdin = bytes.NewReader(installScript)
	out, err := cmd.CombinedOutput()
	if err == nil || !strings.Contains(string(out), "--token-stdin needs the script as a file") {
		t.Fatalf("err = %v, out = %s", err, out)
	}
}

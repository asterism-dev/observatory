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
	"regexp"
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
	nodeAssets(mux, bin, "")
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
	nodeAssets(missing, filepath.Join(t.TempDir(), "absent"), "")
	if get(missing, "/node/observatory-node").Code != 404 || get(missing, "/node/SHA256SUMS").Code != 404 || get(missing, "/node/install.sh").Code != 200 {
		t.Fatal("missing binary handling")
	}
}

func TestNodeAssetsServesGuestboot(t *testing.T) {
	dir := t.TempDir()
	bin, tarball := filepath.Join(dir, "node"), filepath.Join(dir, "guestboot.tar")
	for path, content := range map[string]string{bin: "binary", tarball: "tarball"} {
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	mux := http.NewServeMux()
	nodeAssets(mux, bin, tarball)
	if rec := get(mux, "/node/guestboot.tar"); rec.Body.String() != "tarball" {
		t.Fatalf("guestboot.tar: %q", rec.Body.String())
	}
	binSum, tarSum := sha256.Sum256([]byte("binary")), sha256.Sum256([]byte("tarball"))
	want := hex.EncodeToString(binSum[:]) + "  observatory-node\n" + hex.EncodeToString(tarSum[:]) + "  guestboot.tar\n"
	if rec := get(mux, "/node/SHA256SUMS"); rec.Body.String() != want {
		t.Fatalf("sums: %q", rec.Body.String())
	}

	noTar := http.NewServeMux()
	nodeAssets(noTar, bin, filepath.Join(dir, "absent.tar"))
	if get(noTar, "/node/guestboot.tar").Code != 404 || strings.Contains(get(noTar, "/node/SHA256SUMS").Body.String(), "guestboot.tar") {
		t.Fatal("missing guestboot handling")
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

func TestInstallUpdateRefusesToken(t *testing.T) {
	cmd := exec.Command("sh", "-s", "--", "--hub", "http://x", "--update", "--token", "t")
	cmd.Stdin = bytes.NewReader(installScript)
	out, err := cmd.CombinedOutput()
	if cmd.ProcessState.ExitCode() != 2 {
		t.Fatalf("err = %v, out = %s", err, out)
	}
}

func TestInstallUpdateRefusesTokenStdin(t *testing.T) {
	script := filepath.Join(t.TempDir(), "install.sh")
	if err := os.WriteFile(script, installScript, 0o644); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("sh", script, "--hub", "http://x", "--update", "--token-stdin")
	out, err := cmd.CombinedOutput()
	if cmd.ProcessState.ExitCode() != 2 {
		t.Fatalf("err = %v, out = %s", err, out)
	}
}

func TestInstallUpdateNeedsEnrolledNode(t *testing.T) {
	if _, err := os.Stat("/etc/observatory-node"); err == nil {
		t.Skip("/etc/observatory-node exists on this machine")
	}
	cmd := exec.Command("sh", "-s", "--", "--hub", "http://x", "--update")
	cmd.Stdin = bytes.NewReader(installScript)
	out, err := cmd.CombinedOutput()
	if err == nil || !strings.Contains(string(out), "--update needs an enrolled node") {
		t.Fatalf("err = %v, out = %s", err, out)
	}
}

func TestFirecrackerVersion(t *testing.T) {
	script, err := os.ReadFile("install.sh")
	if err != nil {
		t.Fatal(err)
	}
	want := regexp.MustCompile(`FIRECRACKER_VERSION=(v[0-9.]+)\n`).FindSubmatch(script)
	if want == nil {
		t.Fatal("install.sh has no FIRECRACKER_VERSION")
	}
	if got := firecrackerVersion(string(installScript)); got != string(want[1]) {
		t.Fatalf("got %q, want %q", got, want[1])
	}
	if got := firecrackerVersion("echo hi"); got != "" {
		t.Fatalf("no version: got %q", got)
	}
}

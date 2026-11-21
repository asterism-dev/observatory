package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/asterism-dev/observatory/internal/version"
)

func setVersion(t *testing.T, v string) {
	old := version.Version
	version.Version = v
	t.Cleanup(func() { version.Version = old })
}

func tarball(t *testing.T, name, content string) []byte {
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o755, Size: int64(len(content))}); err != nil {
		t.Fatal(err)
	}
	tw.Write([]byte(content))
	tw.Close()
	gz.Close()
	return buf.Bytes()
}

// releaseCLI serves release tag on h and points c's release URLs and executable at it; the binary holds "old".
func releaseCLI(t *testing.T, h *fakeHub, c *cli, tag string, sums func(file string, tgz []byte) string) string {
	h.on("GET /latest", 200, fmt.Sprintf(`{"tag_name":%q}`, tag))
	file := fmt.Sprintf("observatory_%s_%s_%s.tar.gz", tag, runtime.GOOS, runtime.GOARCH)
	tgz := tarball(t, "observatory", "new")
	h.on("GET /download/"+tag+"/"+file, 200, string(tgz))
	h.on("GET /download/"+tag+"/SHA256SUMS", 200, sums(file, tgz))
	exe := filepath.Join(t.TempDir(), "observatory")
	if err := os.WriteFile(exe, []byte("old"), 0o755); err != nil {
		t.Fatal(err)
	}
	c.latestURL, c.downloadURL = h.URL+"/latest", h.URL+"/download"
	c.executable = func() (string, error) { return exe, nil }
	return exe
}

func goodSums(file string, tgz []byte) string {
	return fmt.Sprintf("%x  other.tar.gz\n%x  %s\n", sha256.Sum256(nil), sha256.Sum256(tgz), file)
}

func TestUpdateReplacesBinary(t *testing.T) {
	setVersion(t, "v0.3.0")
	h := newFakeHub(t)
	c, stdout, stderr := newTestCLI(t, h)
	exe := releaseCLI(t, h, c, "v0.4.0", goodSums)
	if code := run(c, []string{"update"}); code != 0 {
		t.Fatalf("exit %d: %s", code, stderr)
	}
	b, _ := os.ReadFile(exe)
	fi, _ := os.Stat(exe)
	if string(b) != "new" || fi.Mode().Perm() != 0o755 {
		t.Fatalf("binary %q mode %v", b, fi.Mode())
	}
	if !strings.Contains(stdout.String(), "v0.3.0 → v0.4.0") {
		t.Fatalf("stdout %q", stdout)
	}
}

func TestUpdateChecksumMismatch(t *testing.T) {
	setVersion(t, "v0.3.0")
	h := newFakeHub(t)
	c, _, _ := newTestCLI(t, h)
	exe := releaseCLI(t, h, c, "v0.4.0", func(file string, _ []byte) string {
		return fmt.Sprintf("%x  %s\n", sha256.Sum256([]byte("other")), file)
	})
	if code := run(c, []string{"update"}); code == 0 {
		t.Fatal("want failure")
	}
	if b, _ := os.ReadFile(exe); string(b) != "old" {
		t.Fatalf("binary replaced: %q", b)
	}
	if left, _ := filepath.Glob(filepath.Join(filepath.Dir(exe), ".observatory-*")); len(left) > 0 {
		t.Fatalf("temp files left: %v", left)
	}
}

func TestUpdateUpToDate(t *testing.T) {
	setVersion(t, "v0.3.0")
	h := newFakeHub(t)
	c, stdout, _ := newTestCLI(t, h)
	exe := releaseCLI(t, h, c, "v0.3.0", goodSums)
	if code := run(c, []string{"update"}); code != 0 {
		t.Fatalf("exit %d", code)
	}
	if stdout.String() != "already up to date (v0.3.0)\n" {
		t.Fatalf("stdout %q", stdout)
	}
	if b, _ := os.ReadFile(exe); string(b) != "old" {
		t.Fatalf("binary replaced: %q", b)
	}
}

func TestUpdateDevBuild(t *testing.T) {
	h := newFakeHub(t)
	c, _, stderr := newTestCLI(t, h)
	releaseCLI(t, h, c, "v0.4.0", goodSums)
	if code := run(c, []string{"update"}); code == 0 || !strings.Contains(stderr.String(), "dev build") {
		t.Fatalf("exit %d, stderr %q", code, stderr)
	}
}

func TestUpdateHint(t *testing.T) {
	setVersion(t, "v0.3.0")
	h := newFakeHub(t)
	meOneOrg(h)
	c, _, stderr := newTestCLI(t, h)
	releaseCLI(t, h, c, "v0.4.0", goodSums)
	for range 2 {
		if code := run(c, []string{"whoami"}); code != 0 {
			t.Fatalf("exit %d", code)
		}
	}
	if n := strings.Count(stderr.String(), "version v0.4.0 is available (running v0.3.0), run observatory update"); n != 2 {
		t.Fatalf("hint printed %d times: %q", n, stderr)
	}
	if n := len(h.calls("GET /latest")); n != 1 {
		t.Fatalf("release checks: %d, want 1 (cached)", n)
	}
}

func TestUpdateHintSilentOnError(t *testing.T) {
	setVersion(t, "v0.3.0")
	h := newFakeHub(t)
	meOneOrg(h)
	c, _, stderr := newTestCLI(t, h)
	releaseCLI(t, h, c, "v0.4.0", goodSums)
	h.on("GET /latest", 403, `{"message":"rate limited"}`)
	if code := run(c, []string{"whoami"}); code != 0 {
		t.Fatalf("exit %d", code)
	}
	if strings.Contains(stderr.String(), "available") || strings.Contains(stderr.String(), "403") {
		t.Fatalf("stderr %q", stderr)
	}
	if _, err := os.Stat(filepath.Join(c.cfgDir, "update-check.json")); err != nil {
		t.Fatalf("cache not written: %v", err)
	}
}

func TestUpdateHintDisabled(t *testing.T) {
	setVersion(t, "v0.3.0")
	t.Setenv("OBSERVATORY_NO_UPDATE_CHECK", "1")
	h := newFakeHub(t)
	meOneOrg(h)
	c, _, _ := newTestCLI(t, h)
	releaseCLI(t, h, c, "v0.4.0", goodSums)
	if code := run(c, []string{"whoami"}); code != 0 {
		t.Fatalf("exit %d", code)
	}
	if n := len(h.calls("GET /latest")); n != 0 {
		t.Fatalf("release checks: %d, want 0", n)
	}
}

func TestUpdateHintCorruptCache(t *testing.T) {
	setVersion(t, "v0.3.0")
	h := newFakeHub(t)
	meOneOrg(h)
	c, _, stderr := newTestCLI(t, h)
	releaseCLI(t, h, c, "v0.4.0", goodSums)
	if err := os.WriteFile(filepath.Join(c.cfgDir, "update-check.json"), []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	if code := run(c, []string{"whoami"}); code != 0 {
		t.Fatalf("exit %d", code)
	}
	if !strings.Contains(stderr.String(), "version v0.4.0 is available") {
		t.Fatalf("stderr %q", stderr)
	}
}

func TestUpdateHintCachedWithoutConfigDir(t *testing.T) {
	setVersion(t, "v0.3.0")
	h := newFakeHub(t)
	meOneOrg(h)
	c, _, stderr := newTestCLI(t, h)
	releaseCLI(t, h, c, "v0.4.0", goodSums)
	if err := os.RemoveAll(c.cfgDir); err != nil {
		t.Fatal(err)
	}
	t.Setenv("OBSERVATORY_HUB", h.URL)
	t.Setenv("OBSERVATORY_TOKEN", "tok-test")
	for range 2 {
		if code := run(c, []string{"whoami"}); code != 0 {
			t.Fatalf("exit %d: %s", code, stderr)
		}
	}
	if n := len(h.calls("GET /latest")); n != 1 {
		t.Fatalf("release checks: %d, want 1 (cached)", n)
	}
}

func TestUpdateReadOnlyDir(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root can write anywhere")
	}
	setVersion(t, "v0.3.0")
	h := newFakeHub(t)
	c, _, stderr := newTestCLI(t, h)
	exe := releaseCLI(t, h, c, "v0.4.0", goodSums)
	dir := filepath.Dir(exe)
	if err := os.Chmod(dir, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(dir, 0o755) })
	if code := run(c, []string{"update"}); code == 0 {
		t.Fatal("want failure")
	}
	if !strings.Contains(stderr.String(), exe) || !strings.Contains(stderr.String(), "sudo") {
		t.Fatalf("stderr %q", stderr)
	}
	if b, _ := os.ReadFile(exe); string(b) != "old" {
		t.Fatalf("binary replaced: %q", b)
	}
}

func TestUpdateHintSkippedOnFailedCommand(t *testing.T) {
	setVersion(t, "v0.3.0")
	h := newFakeHub(t)
	h.on(iamRPC+"GetMe", 500, `{"error":"boom"}`)
	c, _, stderr := newTestCLI(t, h)
	releaseCLI(t, h, c, "v0.4.0", goodSums)
	if code := run(c, []string{"whoami"}); code == 0 {
		t.Fatal("want failure")
	}
	if strings.Contains(stderr.String(), "is available") {
		t.Fatalf("hint after a failed command: %q", stderr)
	}
}

func TestUpdateNoSudoHintForOtherErrors(t *testing.T) {
	setVersion(t, "v0.3.0")
	h := newFakeHub(t)
	c, _, stderr := newTestCLI(t, h)
	releaseCLI(t, h, c, "v0.4.0", goodSums)
	// Renaming a file over a non-empty directory fails, but not with a permission error.
	exe := filepath.Join(t.TempDir(), "observatory")
	if err := os.MkdirAll(filepath.Join(exe, "x"), 0o755); err != nil {
		t.Fatal(err)
	}
	c.executable = func() (string, error) { return exe, nil }
	if code := run(c, []string{"update"}); code == 0 {
		t.Fatal("want failure")
	}
	if strings.Contains(stderr.String(), "sudo") {
		t.Fatalf("sudo suggested for a non-permission error: %q", stderr)
	}
}

func TestUpdateNoBuildForPlatform(t *testing.T) {
	setVersion(t, "v0.3.0")
	h := newFakeHub(t)
	c, _, stderr := newTestCLI(t, h)
	releaseCLI(t, h, c, "v0.4.0", func(string, []byte) string { return "" })
	if code := run(c, []string{"update"}); code == 0 {
		t.Fatal("want failure")
	}
	if want := fmt.Sprintf("no release build for %s/%s", runtime.GOOS, runtime.GOARCH); !strings.Contains(stderr.String(), want) {
		t.Fatalf("stderr %q, want %q", stderr, want)
	}
}

func TestUpdateWithCorruptConfig(t *testing.T) {
	setVersion(t, "v0.3.0")
	h := newFakeHub(t)
	c, _, stderr := newTestCLI(t, h)
	exe := releaseCLI(t, h, c, "v0.4.0", goodSums)
	if err := os.WriteFile(filepath.Join(c.cfgDir, "config.json"), []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	if code := run(c, []string{"update"}); code != 0 {
		t.Fatalf("exit %d: %s", code, stderr)
	}
	if b, _ := os.ReadFile(exe); string(b) != "new" {
		t.Fatalf("binary %q", b)
	}
}

func TestUpdateDownloadTooLarge(t *testing.T) {
	setVersion(t, "v0.3.0")
	old := maxDownload
	maxDownload = 8
	t.Cleanup(func() { maxDownload = old })
	h := newFakeHub(t)
	c, _, stderr := newTestCLI(t, h)
	exe := releaseCLI(t, h, c, "v0.4.0", goodSums)
	if code := run(c, []string{"update"}); code == 0 || !strings.Contains(stderr.String(), "larger than") {
		t.Fatalf("exit %d, stderr %q", code, stderr)
	}
	if b, _ := os.ReadFile(exe); string(b) != "old" {
		t.Fatalf("binary replaced: %q", b)
	}
}

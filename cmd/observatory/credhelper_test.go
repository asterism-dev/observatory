package main

import (
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCredHelperGet(t *testing.T) {
	h := newFakeHub(t)
	h.onFunc("POST /api/registry/credentials", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusCreated)
		io.WriteString(w, `{"username":"observatory","secret":"s3cret","expiresAt":"2026-11-20T02:00:00Z"}`)
	})
	c, out, errb := newTestCLI(t, h)
	host := strings.TrimPrefix(h.URL, "http://")
	c.stdin = strings.NewReader(host + "\n")
	if code := runCredHelper(c, []string{"get"}); code != 0 {
		t.Fatal(code, errb.String())
	}
	var got map[string]string
	if err := json.Unmarshal(out.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got["ServerURL"] != host || got["Username"] != "observatory" || got["Secret"] != "s3cret" {
		t.Errorf("output %v", got)
	}
	if a := h.calls("POST /api/registry/credentials")[0].Auth; a != "Bearer tok-test" {
		t.Errorf("auth %q", a)
	}
}

func TestCredHelperGetRefusesOtherServers(t *testing.T) {
	h := newFakeHub(t)
	c, out, errb := newTestCLI(t, h)
	c.stdin = strings.NewReader("registry.example.com")
	if code := runCredHelper(c, []string{"get"}); code != 1 || out.Len() != 0 {
		t.Errorf("code %d stdout %q stderr %s", code, out, errb)
	}
	if n := len(h.calls("POST /api/registry/credentials")); n != 0 {
		t.Errorf("%d credential requests", n)
	}
}

func TestCredHelperStoreEraseAreNoOps(t *testing.T) {
	c, out, _ := newTestCLI(t, nil)
	for _, cmd := range []string{"store", "erase"} {
		if code := runCredHelper(c, []string{cmd}); code != 0 || out.Len() != 0 {
			t.Errorf("%s: code %d stdout %q", cmd, code, out)
		}
	}
}

func TestIsCredHelper(t *testing.T) {
	if !isCredHelper("/home/a/.local/bin/docker-credential-observatory") || isCredHelper("/usr/bin/observatory") {
		t.Error("argv[0] detection")
	}
}

func TestImageLoginWritesDockerConfig(t *testing.T) {
	h := newFakeHub(t)
	c, out, errb := newTestCLI(t, h)
	t.Setenv("DOCKER_CONFIG", "")
	home := os.Getenv("HOME")
	cfgPath := filepath.Join(home, ".docker", "config.json")
	if err := os.MkdirAll(filepath.Dir(cfgPath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cfgPath, []byte(`{"auths":{"ghcr.io":{"auth":"abc"}},"credHelpers":{"gcr.io":"gcloud"},"currentContext":"x"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	bin := t.TempDir()
	exe := filepath.Join(bin, "observatory")
	if err := os.WriteFile(exe, []byte("x"), 0o755); err != nil {
		t.Fatal(err)
	}
	exe, _ = filepath.EvalSymlinks(exe)
	c.executable = func() (string, error) { return exe, nil }
	mustRun(t, c, errb.String, 0, "image", "login")

	b, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	var cfg struct {
		Auths          map[string]any
		CredHelpers    map[string]string
		CurrentContext string
	}
	if err := json.Unmarshal(b, &cfg); err != nil {
		t.Fatal(err)
	}
	host := strings.TrimPrefix(h.URL, "http://")
	if cfg.CredHelpers[host] != "observatory" || cfg.CredHelpers["gcr.io"] != "gcloud" || cfg.Auths["ghcr.io"] == nil || cfg.CurrentContext != "x" {
		t.Errorf("config %s", b)
	}
	link := filepath.Join(bin, "docker-credential-observatory")
	if got, err := os.Readlink(link); err != nil || got != exe {
		t.Errorf("link %q %v", got, err)
	}
	if !strings.Contains(out.String(), link) || !strings.Contains(out.String(), cfgPath) {
		t.Errorf("stdout %s", out)
	}
}

func TestImageLoginFallsBackToLocalBin(t *testing.T) {
	c, _, errb := newTestCLI(t, nil)
	t.Setenv("DOCKER_CONFIG", "")
	ro := t.TempDir()
	exe := filepath.Join(ro, "observatory")
	if err := os.WriteFile(exe, []byte("x"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(ro, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(ro, 0o700) })
	if os.Geteuid() == 0 {
		t.Skip("root ignores directory permissions")
	}
	exe, _ = filepath.EvalSymlinks(exe)
	c.executable = func() (string, error) { return exe, nil }
	mustRun(t, c, errb.String, 0, "image", "login")
	if _, err := os.Lstat(filepath.Join(os.Getenv("HOME"), ".local", "bin", "docker-credential-observatory")); err != nil {
		t.Error(err)
	}
}

func TestSetCredHelperOddConfigs(t *testing.T) {
	for _, in := range []string{"", "  \n", "null", `{"credHelpers":null,"x":1}`} {
		cfgPath := filepath.Join(t.TempDir(), "config.json")
		if err := os.WriteFile(cfgPath, []byte(in), 0o640); err != nil {
			t.Fatal(err)
		}
		if err := setCredHelper(cfgPath, "hub.test"); err != nil {
			t.Fatalf("%q: %v", in, err)
		}
		var cfg struct{ CredHelpers map[string]string }
		b, _ := os.ReadFile(cfgPath)
		if err := json.Unmarshal(b, &cfg); err != nil || cfg.CredHelpers["hub.test"] != "observatory" {
			t.Errorf("%q: config %s %v", in, b, err)
		}
		if st, _ := os.Stat(cfgPath); st.Mode().Perm() != 0o640 {
			t.Errorf("%q: mode %v", in, st.Mode())
		}
		if entries, _ := os.ReadDir(filepath.Dir(cfgPath)); len(entries) != 1 {
			t.Errorf("%q: leftover files %v", in, entries)
		}
	}
}

func TestSetCredHelperKeepsSymlinkedConfig(t *testing.T) {
	dir := t.TempDir()
	real, cfgPath := filepath.Join(dir, "real.json"), filepath.Join(dir, "config.json")
	if err := os.WriteFile(real, []byte(`{}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(real, cfgPath); err != nil {
		t.Fatal(err)
	}
	if err := setCredHelper(cfgPath, "hub.test"); err != nil {
		t.Fatal(err)
	}
	if st, err := os.Lstat(cfgPath); err != nil || st.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("symlink replaced: %v %v", st, err)
	}
	if b, _ := os.ReadFile(real); !strings.Contains(string(b), "hub.test") {
		t.Errorf("real config %s", b)
	}
}

func TestReplaceSymlinkRefusesRegularFile(t *testing.T) {
	dir := t.TempDir()
	link := filepath.Join(dir, credHelperName)
	if err := os.WriteFile(link, []byte("mine"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := replaceSymlink("/bin/observatory", link); err == nil || !strings.Contains(err.Error(), "not a symlink") {
		t.Fatalf("err = %v", err)
	}
	if b, _ := os.ReadFile(link); string(b) != "mine" {
		t.Errorf("file replaced: %q", b)
	}
	if err := os.Remove(link); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("/old", link); err != nil {
		t.Fatal(err)
	}
	if err := replaceSymlink("/bin/observatory", link); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.Readlink(link); got != "/bin/observatory" {
		t.Errorf("link -> %s", got)
	}
}

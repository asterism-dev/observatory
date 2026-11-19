package guestinit

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func put(t *testing.T, r Root, rel, content string, mode os.FileMode) {
	t.Helper()
	p := filepath.Join(r.Path, rel)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), mode); err != nil {
		t.Fatal(err)
	}
}

func get(t *testing.T, r Root, rel string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(r.Path, rel))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func exists(r Root, rel string) bool {
	_, err := os.Lstat(filepath.Join(r.Path, rel))
	return err == nil
}

func ensure(t *testing.T, r Root) {
	t.Helper()
	if err := r.EnsureDev(); err != nil {
		t.Fatal(err)
	}
}

func TestEnsureDevCreatesUser(t *testing.T) {
	r := Root{t.TempDir()}
	ensure(t, r)
	if got := get(t, r, "etc/passwd"); got != "dev:x:1000:1000::/home/dev:/bin/false\n" {
		t.Fatalf("passwd = %q", got)
	}
	if got := get(t, r, "etc/group"); got != "dev:x:1000:\n" {
		t.Fatalf("group = %q", got)
	}
	if !strings.HasPrefix(get(t, r, "etc/shadow"), "dev:") {
		t.Fatal("shadow has no dev entry")
	}
	st, err := os.Stat(filepath.Join(r.Path, "home/dev"))
	if err != nil || st.Mode().Perm() != 0o750 {
		t.Fatalf("home/dev: %v %v", st, err)
	}
}

func TestEnsureDevRenamesUID1000(t *testing.T) {
	r := Root{t.TempDir()}
	put(t, r, "etc/passwd", "root:x:0:0:root:/root:/bin/bash\nubuntu:x:1000:1000:Ubuntu:/home/ubuntu:/bin/bash\n", 0o644)
	put(t, r, "etc/group", "root:x:0:\nsudo:x:27:ubuntu\nubuntu:x:1000:\n", 0o644)
	put(t, r, "etc/shadow", "root:*:1:0:::::\nubuntu:$6$x:1:0:99999:7:::\n", 0o640)
	put(t, r, "bin/bash", "", 0o755)
	put(t, r, "home/ubuntu/.bashrc", "x", 0o644)
	ensure(t, r)

	if got := get(t, r, "etc/passwd"); !strings.Contains(got, "dev:x:1000:1000:Ubuntu:/home/dev:/bin/bash\n") || strings.Contains(got, "ubuntu") {
		t.Fatalf("passwd = %q", got)
	}
	if got := get(t, r, "etc/group"); !strings.Contains(got, "sudo:x:27:dev\n") || !strings.Contains(got, "dev:x:1000:\n") {
		t.Fatalf("group = %q", got)
	}
	if got := get(t, r, "etc/shadow"); !strings.Contains(got, "dev:$6$x:") || strings.Contains(got, "ubuntu") {
		t.Fatalf("shadow = %q", got)
	}
	if !exists(r, "home/dev/.bashrc") || exists(r, "home/ubuntu") {
		t.Fatal("home not moved")
	}
	st, _ := os.Stat(filepath.Join(r.Path, "etc/shadow"))
	if st.Mode().Perm() != 0o640 {
		t.Fatalf("shadow mode = %v", st.Mode())
	}
}

func TestEnsureDevKeepsExistingHome(t *testing.T) {
	r := Root{t.TempDir()}
	put(t, r, "etc/passwd", "ubuntu:x:1000:1000::/home/ubuntu:/bin/sh\n", 0o644)
	put(t, r, "home/ubuntu/a", "", 0o644)
	put(t, r, "home/dev/b", "", 0o644)
	ensure(t, r)
	if !exists(r, "home/dev/b") || !exists(r, "home/ubuntu/a") || exists(r, "home/dev/a") {
		t.Fatal("existing home/dev must be kept and old home left")
	}
}

func TestEnsureDevIdempotent(t *testing.T) {
	r := Root{t.TempDir()}
	put(t, r, "etc/passwd", "ubuntu:x:1000:1000::/home/ubuntu:/bin/sh\n", 0o644)
	put(t, r, "etc/group", "sudo:x:27:ubuntu\n", 0o644)
	ensure(t, r)
	files := []string{"etc/passwd", "etc/group", "etc/shadow"}
	before := make([]string, len(files))
	for i, f := range files {
		before[i] = get(t, r, f)
	}
	ensure(t, r)
	for i, f := range files {
		if get(t, r, f) != before[i] {
			t.Fatalf("%s changed on second run", f)
		}
	}
}

func TestEnsureDevSudoersOnlyWithSudo(t *testing.T) {
	r := Root{t.TempDir()}
	ensure(t, r)
	if exists(r, "etc/sudoers.d/observatory-dev") {
		t.Fatal("sudoers written without sudo")
	}
	put(t, r, "usr/bin/sudo", "", 0o755)
	ensure(t, r)
	if got := get(t, r, "etc/sudoers.d/observatory-dev"); got != "dev ALL=(ALL) NOPASSWD:ALL\n" {
		t.Fatalf("sudoers = %q", got)
	}
	st, _ := os.Stat(filepath.Join(r.Path, "etc/sudoers.d/observatory-dev"))
	if st.Mode().Perm() != 0o440 {
		t.Fatalf("mode = %v", st.Mode())
	}
}

func TestEnsureDevKeepsUID1000Group(t *testing.T) {
	r := Root{t.TempDir()}
	put(t, r, "etc/passwd", "node:x:1000:1000::/home/node:/bin/bash\n", 0o644)
	put(t, r, "etc/group", "node:x:1000:\n", 0o644)
	ensure(t, r)
	if got := get(t, r, "etc/group"); got != "dev:x:1000:\n" {
		t.Fatalf("group = %q", got)
	}
}

func TestDevShellFollowsSymlinksInsideRoot(t *testing.T) {
	r := Root{t.TempDir()}
	put(t, r, "etc/passwd", "u:x:1000:1000::/home/u:/bin/zsh\n", 0o644)
	put(t, r, "usr/bin/zsh", "", 0o755)
	if err := os.MkdirAll(filepath.Join(r.Path, "bin"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("/usr/bin/zsh", filepath.Join(r.Path, "bin/zsh")); err != nil {
		t.Fatal(err)
	}
	if got := r.DevShell(); got != "/bin/zsh" {
		t.Fatalf("shell = %q", got)
	}
	put(t, r, "bin/sh", "", 0o755)
	if err := os.Remove(filepath.Join(r.Path, "usr/bin/zsh")); err != nil {
		t.Fatal(err)
	}
	if got := r.DevShell(); got != "/bin/sh" {
		t.Fatalf("shell = %q", got)
	}
}

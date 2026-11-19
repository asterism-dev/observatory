package firecracker

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/asterism-dev/observatory/internal/version"
)

func fakeGuestboot(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	for n, content := range map[string]string{"vmlinux": "kernel", "observatory-init": "init", "mke2fs": "mke2fs", "resize2fs": "resize2fs", "VERSION": version.Version + "\n"} {
		if err := os.WriteFile(filepath.Join(dir, n), []byte(content), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func TestGuestbootVersionMismatch(t *testing.T) {
	src := fakeGuestboot(t)
	if err := os.WriteFile(filepath.Join(src, "VERSION"), []byte("v9.9.9\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := loadGuestboot(src, t.TempDir())
	if want := "guestboot v9.9.9 does not match node " + version.Version + "; rerun the installer"; err == nil || err.Error() != want {
		t.Fatalf("err = %v, want %q", err, want)
	}
}

func TestLoadGuestboot(t *testing.T) {
	dir := filepath.Join(t.TempDir(), ".guestboot")
	g, err := loadGuestboot(fakeGuestboot(t), dir)
	if err != nil {
		t.Fatal(err)
	}
	if b, err := os.ReadFile(g.kernel); err != nil || string(b) != "kernel" {
		t.Fatalf("kernel = %q, %v", b, err)
	}
	for _, p := range []string{g.kernel, g.initrd} {
		if st, err := os.Stat(p); err != nil || st.Mode().Perm() != 0o644 {
			t.Fatalf("%s: %v %v", p, st, err)
		}
	}
	if st, _ := os.Stat(dir); st.Mode().Perm() != 0o700 {
		t.Fatalf("dir mode = %v", st.Mode())
	}
}

func TestGuestImageRef(t *testing.T) {
	if got := guestImageRef(""); got != StandardImage+":latest" {
		t.Errorf("dev build ref = %q", got)
	}
	if got := guestImageRef("reg:5000/guest:x"); got != "reg:5000/guest:x" {
		t.Errorf("configured ref = %q", got)
	}
	if got := pinned("reg:5000/guest:x@sha256:aa", "sha256:bb"); got != "reg:5000/guest:x@sha256:bb" {
		t.Errorf("pinned = %q", got)
	}
}

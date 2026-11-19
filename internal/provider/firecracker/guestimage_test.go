package firecracker

import (
	"os"
	"path/filepath"
	"testing"
)

func TestRemoveLegacyImages(t *testing.T) {
	dir := t.TempDir()
	legacy := []string{"guest-1.2.3.raw", "guest-dev.raw.part", "0123456789abcdef.raw", "0123456789abcdef.raw.part", "0123456789abcdef.raw.download"}
	kept := []string{"oci/blobs/x", "0123456789abcdef.img", "guest-1.2.3.img", "abc.raw", "vm-0123456789abcdef.raw"}
	for _, f := range append(legacy, kept...) {
		p := filepath.Join(dir, f)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, nil, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	removeLegacyImages(dir)
	for _, f := range legacy {
		if _, err := os.Stat(filepath.Join(dir, f)); !os.IsNotExist(err) {
			t.Errorf("%s left: %v", f, err)
		}
	}
	for _, f := range kept {
		if _, err := os.Stat(filepath.Join(dir, f)); err != nil {
			t.Errorf("%s removed: %v", f, err)
		}
	}
}

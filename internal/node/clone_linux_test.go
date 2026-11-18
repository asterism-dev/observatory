package node

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/sys/unix"
)

func TestCloneFileFallback(t *testing.T) {
	dir := t.TempDir()
	src, dst := filepath.Join(dir, "src"), filepath.Join(dir, "dst")
	want := bytes.Repeat([]byte("vmm"), 100000)
	if err := os.WriteFile(src, want, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := cloneFile(src, dst); err != nil {
		t.Fatal(err)
	}
	if got, err := os.ReadFile(dst); err != nil || !bytes.Equal(got, want) {
		t.Fatalf("clone differs: %v", err)
	}
}

func TestCloneResizeKeepsHoles(t *testing.T) {
	dir := t.TempDir()
	src, dst := filepath.Join(dir, "src"), filepath.Join(dir, "dst")
	const size, off, n = 1 << 30, 512 << 20, 1 << 20
	data := bytes.Repeat([]byte("vmm"), n/3+1)[:n]
	f, err := os.Create(src)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Truncate(size); err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteAt(data, off); err != nil {
		t.Fatal(err)
	}
	f.Close()
	if err := copyFile(src, dst); err != nil {
		t.Fatal(err)
	}
	out, err := os.Open(dst)
	if err != nil {
		t.Fatal(err)
	}
	defer out.Close()
	got := make([]byte, n)
	if _, err := out.ReadAt(got, off); err != nil || !bytes.Equal(got, data) {
		t.Fatalf("data range differs: %v", err)
	}
	var st unix.Stat_t
	if err := unix.Stat(dst, &st); err != nil {
		t.Fatal(err)
	}
	if st.Size != size || st.Blocks*512 >= 8<<20 {
		t.Fatalf("size=%d allocated=%d", st.Size, st.Blocks*512)
	}
}

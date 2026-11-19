package node

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

func writeBase(t *testing.T, size int) (string, []byte) {
	t.Helper()
	data := bytes.Repeat([]byte("base"), size/4)
	p := filepath.Join(t.TempDir(), "base.raw")
	if err := os.WriteFile(p, data, 0o644); err != nil {
		t.Fatal(err)
	}
	return p, data
}

func TestCloneResize(t *testing.T) {
	base, data := writeBase(t, 1<<20)
	dst := filepath.Join(t.TempDir(), "root.img")
	if err := CloneResize(base, dst, 4); err != nil {
		t.Fatal(err)
	}
	st, err := os.Stat(dst)
	if err != nil {
		t.Fatal(err)
	}
	if st.Size() != 4<<30 {
		t.Fatalf("size = %d", st.Size())
	}
	f, _ := os.Open(dst)
	defer f.Close()
	head := make([]byte, len(data))
	f.ReadAt(head, 0)
	if !bytes.Equal(head, data) {
		t.Fatal("content differs from base")
	}
}

func TestCloneResizeRejectsSmallerThanImage(t *testing.T) {
	base, _ := writeBase(t, 1<<20)
	if err := os.Truncate(base, 2<<30); err != nil {
		t.Fatal(err)
	}
	dst := filepath.Join(t.TempDir(), "root.img")
	err := CloneResize(base, dst, 1)
	if want := "disk 1 GB is smaller than image " + base + " (2048 MB)"; err == nil || err.Error() != want {
		t.Fatalf("err = %v, want %q", err, want)
	}
	if _, err := os.Stat(dst); err == nil {
		t.Fatal("clone written")
	}
}

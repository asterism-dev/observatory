package storage

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/asterism-dev/observatory/internal/proto"
)

func stat(t *testing.T, path string) (size, alloc int64) {
	t.Helper()
	st, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	return st.Size(), st.Sys().(*syscall.Stat_t).Blocks * 512
}

func apply(t *testing.T, d Dir, disks []proto.StorageDisk, deleted []string) {
	t.Helper()
	if err := d.Apply(disks, deleted); err != nil {
		t.Fatal(err)
	}
}

func TestApplyCreatesSparse(t *testing.T) {
	d := Dir{t.TempDir()}
	apply(t, d, []proto.StorageDisk{{ID: "a", SizeBytes: 1 << 30, Create: true}}, nil)
	size, alloc := stat(t, filepath.Join(d.Path, "a.img"))
	if size != 1<<30 || alloc >= 1<<20 {
		t.Fatalf("size %d alloc %d", size, alloc)
	}
}

func TestApplyGrowsKeepsData(t *testing.T) {
	d := Dir{t.TempDir()}
	apply(t, d, []proto.StorageDisk{{ID: "a", SizeBytes: 1 << 30, Create: true}}, nil)
	p := filepath.Join(d.Path, "a.img")
	f, _ := os.OpenFile(p, os.O_WRONLY, 0)
	f.WriteAt([]byte("hello"), 0)
	f.Close()
	apply(t, d, []proto.StorageDisk{{ID: "a", SizeBytes: 2 << 30}}, nil)
	size, _ := stat(t, p)
	b := make([]byte, 5)
	f, _ = os.Open(p)
	defer f.Close()
	f.ReadAt(b, 0)
	if size != 2<<30 || string(b) != "hello" {
		t.Fatalf("size %d data %q", size, b)
	}
}

func TestApplyNeverShrinks(t *testing.T) {
	d := Dir{t.TempDir()}
	apply(t, d, []proto.StorageDisk{{ID: "a", SizeBytes: 2 << 30, Create: true}}, nil)
	apply(t, d, []proto.StorageDisk{{ID: "a", SizeBytes: 1 << 30, Create: true}}, nil)
	if size, _ := stat(t, filepath.Join(d.Path, "a.img")); size != 2<<30 {
		t.Fatalf("size %d", size)
	}
}

func TestApplyCopies(t *testing.T) {
	d := Dir{t.TempDir()}
	apply(t, d, []proto.StorageDisk{{ID: "a", SizeBytes: 1 << 30, Create: true}}, nil)
	f, _ := os.OpenFile(filepath.Join(d.Path, "a.img"), os.O_WRONLY, 0)
	f.WriteAt([]byte("start"), 0)
	f.WriteAt([]byte("middle"), 512<<20)
	f.Close()
	apply(t, d, []proto.StorageDisk{{ID: "a", SizeBytes: 1 << 30, Create: true}, {ID: "b", SizeBytes: 1 << 30, CopyFrom: "a", Create: true}}, nil)
	waitCopies(t)
	p := filepath.Join(d.Path, "b.img")
	f, _ = os.Open(p)
	defer f.Close()
	b1, b2 := make([]byte, 5), make([]byte, 6)
	f.ReadAt(b1, 0)
	f.ReadAt(b2, 512<<20)
	size, alloc := stat(t, p)
	if string(b1) != "start" || string(b2) != "middle" || size != 1<<30 || alloc >= 4<<20 {
		t.Fatalf("%q %q size %d alloc %d", b1, b2, size, alloc)
	}
}

func TestApplyCopyMissingSourceSkips(t *testing.T) {
	d := Dir{t.TempDir()}
	b := proto.StorageDisk{ID: "b", SizeBytes: 1 << 20, CopyFrom: "a", Create: true}
	apply(t, d, []proto.StorageDisk{b}, nil)
	if _, err := os.Stat(filepath.Join(d.Path, "b.img")); !os.IsNotExist(err) {
		t.Fatalf("b.img exists: %v", err)
	}
	apply(t, d, []proto.StorageDisk{{ID: "a", SizeBytes: 1 << 20, Create: true}}, nil)
	apply(t, d, []proto.StorageDisk{b}, nil)
	waitCopies(t)
	if _, err := os.Stat(filepath.Join(d.Path, "b.img")); err != nil {
		t.Fatal(err)
	}
}

func TestApplyDeletesOnlyTombstones(t *testing.T) {
	d := Dir{t.TempDir()}
	for _, n := range []string{"a.img", "b.img", "x.img"} {
		os.WriteFile(filepath.Join(d.Path, n), nil, 0o644)
	}
	apply(t, d, []proto.StorageDisk{{ID: "a", SizeBytes: 0, Create: true}}, []string{"b"})
	files, _ := d.Scan()
	if len(files) != 2 || files[0].Name != "a.img" || files[1].Name != "x.img" {
		t.Fatalf("%+v", files)
	}
}

func TestPartialNotScanned(t *testing.T) {
	d := Dir{t.TempDir()}
	os.MkdirAll(filepath.Join(d.Path, ".partial"), 0o755)
	os.WriteFile(filepath.Join(d.Path, ".partial", "p.img"), nil, 0o644)
	if files, _ := d.Scan(); len(files) != 0 {
		t.Fatalf("%+v", files)
	}
}

func TestApplyRejectsPathIDs(t *testing.T) {
	root := t.TempDir()
	d := Dir{filepath.Join(root, "disks")}
	os.MkdirAll(d.Path, 0o755)
	os.WriteFile(filepath.Join(root, "victim.img"), nil, 0o644)
	os.WriteFile(filepath.Join(d.Path, "ok.img"), []byte("data"), 0o644)
	bad := []string{"", ".", "..", "../victim", "sub/x", `sub\x`}
	var disks []proto.StorageDisk
	for _, id := range bad {
		disks = append(disks, proto.StorageDisk{ID: id, SizeBytes: 1 << 20, Create: true})
	}
	disks = append(disks, proto.StorageDisk{ID: "copy", SizeBytes: 1 << 20, CopyFrom: "../victim", Create: true}, proto.StorageDisk{ID: "good", SizeBytes: 1 << 20, Create: true})
	if err := d.Apply(disks, append(bad, "ok/../../victim")); err == nil {
		t.Fatal("no error")
	}
	if _, err := os.Stat(filepath.Join(root, "victim.img")); err != nil {
		t.Fatalf("victim: %v", err)
	}
	files, _ := d.Scan()
	if len(files) != 2 || files[0].Name != "good.img" || files[1].Name != "ok.img" {
		t.Fatalf("%+v", files)
	}
}

func TestApplyCreatesOnlyWhenAsked(t *testing.T) {
	d := Dir{t.TempDir()}
	os.WriteFile(filepath.Join(d.Path, "src.img"), []byte("data"), 0o644)
	apply(t, d, []proto.StorageDisk{{ID: "a", SizeBytes: 1 << 20}, {ID: "b", SizeBytes: 1 << 20, CopyFrom: "src"}}, nil)
	for _, n := range []string{"a.img", "b.img"} {
		if _, err := os.Stat(filepath.Join(d.Path, n)); !os.IsNotExist(err) {
			t.Fatalf("%s created without Create: %v", n, err)
		}
	}
}

func TestApplyNeverCreatesWithoutFlag(t *testing.T) {
	d := Dir{t.TempDir()}
	apply(t, d, []proto.StorageDisk{{ID: "a", SizeBytes: 1 << 20, Create: false}}, nil)
	if files, _ := d.Scan(); len(files) != 0 {
		t.Fatalf("%+v", files)
	}
}

func TestApplyNeverDeletesUnknown(t *testing.T) {
	d := Dir{t.TempDir()}
	os.WriteFile(filepath.Join(d.Path, "x.img"), []byte("x"), 0o644)
	apply(t, d, []proto.StorageDisk{{ID: "a", SizeBytes: 1 << 20, Create: true}}, []string{"b"})
	if _, err := os.Stat(filepath.Join(d.Path, "x.img")); err != nil {
		t.Fatal(err)
	}
	if err := d.Apply(nil, []string{"../etc"}); err == nil {
		t.Fatal("no error for ../etc")
	}
	if _, err := os.Stat(filepath.Join(d.Path, "x.img")); err != nil {
		t.Fatal(err)
	}
}

func withClone(t *testing.T, f func(dst, src *os.File) error) {
	t.Helper()
	orig := clone
	clone = f
	t.Cleanup(func() { clone = orig })
}

func TestCopyFallsBackWhenCloneUnsupported(t *testing.T) {
	withClone(t, func(dst, src *os.File) error { return errors.ErrUnsupported })
	d := Dir{t.TempDir()}
	src := filepath.Join(d.Path, "a.img")
	f, _ := os.Create(src)
	f.Truncate(3 << 20)
	f.WriteAt([]byte("data"), 2<<20)
	f.Close()
	apply(t, d, []proto.StorageDisk{{ID: "b", SizeBytes: 3 << 20, CopyFrom: "a", Create: true}}, nil)
	waitCopies(t)
	got, err := os.ReadFile(filepath.Join(d.Path, "b.img"))
	want, _ := os.ReadFile(src)
	if err != nil || !bytes.Equal(got, want) {
		t.Fatalf("copy differs: %v", err)
	}
}

func TestCopyUsesClone(t *testing.T) {
	var calls int
	withClone(t, func(dst, src *os.File) error {
		calls++
		if filepath.Base(src.Name()) != "a.img" {
			t.Errorf("source %s", src.Name())
		}
		return nil
	})
	d := Dir{t.TempDir()}
	os.WriteFile(filepath.Join(d.Path, "a.img"), []byte("data"), 0o644)
	apply(t, d, []proto.StorageDisk{{ID: "b", SizeBytes: 1 << 20, CopyFrom: "a", Create: true}}, nil)
	waitCopies(t)
	if size, _ := stat(t, filepath.Join(d.Path, "b.img")); calls != 1 || size != 1<<20 {
		t.Fatalf("calls %d size %d", calls, size)
	}
}

func waitCopies(t *testing.T) {
	t.Helper()
	for range 200 {
		copiesMu.Lock()
		n := len(copies)
		copiesMu.Unlock()
		if n == 0 {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("copies still running")
}

// A sparse copy of a large disk takes hours; the poll loop must keep exporting and reporting meanwhile.
func TestApplyDoesNotWaitForCopies(t *testing.T) {
	release := make(chan struct{})
	var calls atomic.Int32
	withClone(t, func(dst, src *os.File) error { calls.Add(1); <-release; return nil })
	d := Dir{t.TempDir()}
	os.WriteFile(filepath.Join(d.Path, "a.img"), []byte("data"), 0o644)
	b := []proto.StorageDisk{{ID: "b", SizeBytes: 1 << 20, CopyFrom: "a", Create: true}}
	done := make(chan struct{})
	go func() { apply(t, d, b, nil); apply(t, d, b, nil); close(done) }()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		close(release)
		t.Fatal("Apply waited for the copy")
	}
	if _, err := os.Stat(filepath.Join(d.Path, "b.img")); !os.IsNotExist(err) {
		t.Fatalf("b.img before the copy finished: %v", err)
	}
	close(release)
	waitCopies(t)
	if size, _ := stat(t, filepath.Join(d.Path, "b.img")); calls.Load() != 1 || size != 1<<20 {
		t.Fatalf("calls %d size %d", calls.Load(), size)
	}
}

func TestDeleteCancelsRunningCopy(t *testing.T) {
	release := make(chan struct{})
	withClone(t, func(dst, src *os.File) error { <-release; return nil })
	d := Dir{t.TempDir()}
	os.WriteFile(filepath.Join(d.Path, "a.img"), []byte("data"), 0o644)
	apply(t, d, []proto.StorageDisk{{ID: "b", SizeBytes: 1 << 20, CopyFrom: "a", Create: true}}, nil)
	apply(t, d, nil, []string{"b"})
	close(release)
	waitCopies(t)
	for _, p := range []string{filepath.Join(d.Path, "b.img"), filepath.Join(d.Path, partialDir, "b.img")} {
		if _, err := os.Stat(p); !os.IsNotExist(err) {
			t.Fatalf("%s after delete: %v", p, err)
		}
	}
}

func writeDisk(t *testing.T, path, content string, size int64) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Truncate(path, size); err != nil {
		t.Fatal(err)
	}
}

func TestRestoreReplacesContent(t *testing.T) {
	d := Dir{t.TempDir()}
	writeDisk(t, d.File("a"), "AAAA", 8<<20)
	writeDisk(t, filepath.Join(d.Path, "snap.img"), "BBBB", 4<<20)
	if err := d.Restore(proto.StorageDisk{ID: "a", SizeBytes: 8 << 20, RestoreFrom: "snap.img", RestoreToken: "t"}); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(d.File("a"))
	if len(b) != 8<<20 || string(b[:4]) != "BBBB" {
		t.Fatalf("len %d head %q", len(b), b[:4])
	}
}

func TestRestoreMissingSource(t *testing.T) {
	d := Dir{t.TempDir()}
	writeDisk(t, d.File("a"), "AAAA", 1<<20)
	err := d.Restore(proto.StorageDisk{ID: "a", SizeBytes: 1 << 20, RestoreFrom: "snap.img"})
	if err == nil || err.Error() != "file missing" {
		t.Fatalf("err %v", err)
	}
	if b, _ := os.ReadFile(d.File("a")); string(b[:4]) != "AAAA" {
		t.Fatalf("target changed: %q", b[:4])
	}
}

func TestRestoreRejectsEscapingSource(t *testing.T) {
	outside := t.TempDir()
	writeDisk(t, filepath.Join(outside, "x.img"), "XXXX", 1<<20)
	d := Dir{t.TempDir()}
	writeDisk(t, d.File("a"), "AAAA", 1<<20)
	if err := os.Symlink(outside, filepath.Join(d.Path, "link")); err != nil {
		t.Fatal(err)
	}
	for _, from := range []string{"../x.img", filepath.Join(outside, "x.img"), "link/x.img", "sub/../../x.img", ""} {
		if err := d.Restore(proto.StorageDisk{ID: "a", SizeBytes: 1 << 20, RestoreFrom: from}); err == nil || err.Error() == "file missing" {
			t.Errorf("%q: err %v", from, err)
		}
	}
	if b, _ := os.ReadFile(d.File("a")); string(b[:4]) != "AAAA" {
		t.Fatalf("target changed: %q", b[:4])
	}
}

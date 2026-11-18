package firecracker

import (
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"
)

func TestPruneGuestImages(t *testing.T) {
	dir := t.TempDir()
	names := []string{"guest-1.raw", "guest-2.raw", "guest-3.raw", "guest-4.raw.part", "0123456789abcdef.raw"}
	for _, n := range names {
		if err := os.WriteFile(filepath.Join(dir, n), nil, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := pruneGuestImages(dir, "3", "2"); err != nil {
		t.Fatal(err)
	}
	entries, _ := os.ReadDir(dir)
	var got []string
	for _, e := range entries {
		got = append(got, e.Name())
	}
	if want := []string{"0123456789abcdef.raw", "guest-2.raw", "guest-3.raw"}; !slices.Equal(got, want) {
		t.Fatalf("left %v, want %v", got, want)
	}
}

func TestPreviousGuestImage(t *testing.T) {
	dir := t.TempDir()
	if got := previousGuestImage(dir, "3"); got != "" {
		t.Fatalf("empty dir: %q", got)
	}
	now := time.Now()
	for i, v := range []string{"2", "1", "3"} {
		p := guestImagePath(dir, v)
		if err := os.WriteFile(p, nil, 0o644); err != nil {
			t.Fatal(err)
		}
		mod := now.Add(time.Duration(i) * time.Minute)
		if err := os.Chtimes(p, mod, mod); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(dir, "guest-9.raw.part"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if got := previousGuestImage(dir, "3"); got != "1" {
		t.Fatalf("previous = %q, want 1", got)
	}
	os.Remove(guestImagePath(dir, "1"))
	os.Remove(guestImagePath(dir, "2"))
	if got := previousGuestImage(dir, "3"); got != "" {
		t.Fatalf("only current: %q", got)
	}
}

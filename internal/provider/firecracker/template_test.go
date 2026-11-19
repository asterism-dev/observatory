package firecracker

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/asterism-dev/observatory/internal/node"
	"github.com/asterism-dev/observatory/internal/proto"
)

const testDigest = "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

func TestTemplateKey(t *testing.T) {
	f := node.VMFile{Image: "img@" + testDigest, Spec: proto.VMSpec{ID: "a", Name: "a", CPUs: 1, MemoryMB: 512, DiskGB: 10, NICs: []proto.NIC{{MAC: "02:00:00:00:00:01"}}}}
	fcVersion, bootID := "Firecracker v1.18.0", "boot1"
	key := func(f node.VMFile) string {
		t.Helper()
		k, err := templateKey(f, fcVersion, bootID)
		if err != nil {
			t.Fatal(err)
		}
		return k
	}
	k := key(f)
	if len(k) != 16 {
		t.Fatalf("key %q is not 16 hex", k)
	}
	same := f
	same.Spec.ID, same.Spec.Name, same.Spec.DiskGB, same.MAC = "b", "b", 20, "02:00:00:00:00:02"
	same.Spec.NICs = []proto.NIC{{MAC: "02:00:00:00:00:03"}}
	same.Image = "other-name@" + testDigest
	if key(same) != k {
		t.Error("id, name, disk size, MACs or image name changed the key")
	}
	changes := map[string]func(f *node.VMFile){
		"cpus":   func(f *node.VMFile) { f.Spec.CPUs = 2 },
		"memory": func(f *node.VMFile) { f.Spec.MemoryMB = 1024 },
		"nics":   func(f *node.VMFile) { f.Spec.NICs = nil },
		"digest": func(f *node.VMFile) { f.Image = "img@sha256:" + strings.Repeat("b", 64) },
		"config": func(f *node.VMFile) { f.ImageConfig.Cmd = []string{"sleep", "1"} },
	}
	for name, change := range changes {
		g := f
		change(&g)
		if key(g) == k {
			t.Errorf("changed %s gave the same key", name)
		}
	}
	fcVersion = "Firecracker v1.19.0"
	if key(f) == k {
		t.Error("changed Firecracker version gave the same key")
	}
	if _, err := templateKey(node.VMFile{Image: "2026.11.1-aaaaaaaa"}, fcVersion, bootID); err == nil {
		t.Error("key for an image without digest")
	}
}

func TestTemplateKeyChangesWithInitVersion(t *testing.T) {
	src := fakeGuestboot(t)
	f := node.VMFile{Image: "img@" + testDigest, Spec: proto.VMSpec{ID: "a", Name: "a", CPUs: 1, MemoryMB: 512, DiskGB: 10}}
	key := func() string {
		t.Helper()
		g, err := loadGuestboot(src, t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		k, err := templateKey(f, "Firecracker v1.18.0", g.id)
		if err != nil {
			t.Fatal(err)
		}
		return k
	}
	k := key()
	if key() != k {
		t.Fatal("same guestboot gave different keys")
	}
	for _, n := range []string{"observatory-init", "vmlinux"} {
		if err := os.WriteFile(filepath.Join(src, n), []byte("newer "+n), 0o755); err != nil {
			t.Fatal(err)
		}
		if next := key(); next == k {
			t.Errorf("changed %s gave the same key", n)
		} else {
			k = next
		}
	}
}

func TestEvictTemplates(t *testing.T) {
	dir := t.TempDir()
	now := time.Now()
	for i, name := range []string{"a", "b", "c", "d", "e"} {
		p := filepath.Join(dir, name)
		if err := os.Mkdir(p, 0o700); err != nil {
			t.Fatal(err)
		}
		mod := now.Add(time.Duration(i) * time.Minute)
		if err := os.Chtimes(p, mod, mod); err != nil {
			t.Fatal(err)
		}
	}
	if err := evictTemplates(dir, 4); err != nil {
		t.Fatal(err)
	}
	entries, _ := os.ReadDir(dir)
	var got []string
	for _, e := range entries {
		got = append(got, e.Name())
	}
	if len(got) != 4 || got[0] != "b" {
		t.Fatalf("left %v, want b c d e", got)
	}
}

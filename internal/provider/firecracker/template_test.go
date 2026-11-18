package firecracker

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/asterism-dev/observatory/internal/node"
	"github.com/asterism-dev/observatory/internal/node/guest"
	"github.com/asterism-dev/observatory/internal/proto"
)

func TestTemplateKey(t *testing.T) {
	base := filepath.Join(t.TempDir(), "base.raw")
	if err := os.WriteFile(base, []byte("img"), 0o644); err != nil {
		t.Fatal(err)
	}
	f := node.VMFile{Spec: proto.VMSpec{ID: "a", Name: "a", CPUs: 1, MemoryMB: 512, DiskGB: 10, NICs: []proto.NIC{{MAC: "02:00:00:00:00:01"}}}}
	fcVersion := "Firecracker v1.18.0"
	key := func(f node.VMFile) string {
		t.Helper()
		k, err := templateKey(base, fcVersion, f)
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
	if key(same) != k {
		t.Error("id, name, disk size or MACs changed the key")
	}
	changes := map[string]func(f *node.VMFile){
		"cpus":   func(f *node.VMFile) { f.Spec.CPUs = 2 },
		"memory": func(f *node.VMFile) { f.Spec.MemoryMB = 1024 },
		"nics":   func(f *node.VMFile) { f.Spec.NICs = nil },
	}
	for name, change := range changes {
		g := f
		change(&g)
		if key(g) == k {
			t.Errorf("changed %s gave the same key", name)
		}
	}
	if err := os.Chtimes(base, time.Now(), time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if key(f) == k {
		t.Error("changed base mtime gave the same key")
	}
	k = key(f)
	fcVersion = "Firecracker v1.19.0"
	if key(f) == k {
		t.Error("changed Firecracker version gave the same key")
	}
	k = key(f)
	oldBoot := guest.BootScript
	defer func() { guest.BootScript = oldBoot }()
	guest.BootScript += "\n"
	if key(f) == k {
		t.Error("changed seed gave the same key")
	}
	k = key(f)
	old := guest.AgentScript
	defer func() { guest.AgentScript = old }()
	guest.AgentScript += "\n"
	if key(f) == k {
		t.Error("changed agent script gave the same key")
	}
}

func TestTemplateKeyFollowsImage(t *testing.T) {
	dir := t.TempDir()
	a, b := guestImagePath(dir, "a"), guestImagePath(dir, "b")
	mod := time.Now()
	for _, p := range []string{a, b} {
		if err := os.WriteFile(p, []byte("img"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(p, mod, mod); err != nil {
			t.Fatal(err)
		}
	}
	key := func(base, image string) string {
		t.Helper()
		f := node.VMFile{Image: image, Spec: proto.VMSpec{ID: "x", Name: "x", CPUs: 1, MemoryMB: 512, DiskGB: 10}}
		k, err := templateKey(base, "Firecracker v1.18.0", f)
		if err != nil {
			t.Fatal(err)
		}
		return k
	}
	if key(a, "a") == key(b, "b") {
		t.Error("two guest images gave the same key")
	}
	if key(a, "a") != key(a, "a") {
		t.Error("same image gave different keys")
	}
	if templateFile("tpl", node.VMFile{Image: "a"}).Image != "a" {
		t.Error("template file lost the image")
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

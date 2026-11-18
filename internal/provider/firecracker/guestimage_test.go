package firecracker

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/asterism-dev/observatory/internal/node/guestimage"
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

func TestBuildSeedFiles(t *testing.T) {
	img := guestimage.Image{Version: "2026.11.1-3f9a0c1e", Files: map[string][]byte{"vmm-image-build.sh": []byte("#!/bin/sh\n"), "manifest.json": []byte("{}")}}
	seed := buildSeedFiles(img, img.Version)
	ud := string(seed["user-data"])
	for _, want := range []string{"#cloud-config\n", "hostname: vmm-image-build\n", "users: []\n", "mount -o ro LABEL=cidata /run/vmm-seed", "sh /run/vmm-seed/vmm-image-build.sh /run/vmm-seed 2026.11.1-3f9a0c1e; poweroff"} {
		if !strings.Contains(ud, want) {
			t.Errorf("user-data lacks %q:\n%s", want, ud)
		}
	}
	if strings.Contains(ud, "ssh") {
		t.Errorf("user-data mentions ssh:\n%s", ud)
	}
	for _, n := range []string{"vmm-image-build.sh", "manifest.json", "meta-data", "network-config"} {
		if len(seed[n]) == 0 {
			t.Errorf("seed lacks %s", n)
		}
	}
	for _, n := range []string{"vmm-boot.sh", "vmm-env", "vmm-authorized-keys", "vmm-agent.py"} {
		if _, ok := seed[n]; ok {
			t.Errorf("seed has %s", n)
		}
	}
	if !strings.Contains(string(seed["network-config"]), templateNet.Address) {
		t.Errorf("network-config:\n%s", seed["network-config"])
	}
}

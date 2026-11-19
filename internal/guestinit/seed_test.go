package guestinit

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/asterism-dev/observatory/internal/node"
	"github.com/asterism-dev/observatory/internal/proto"
)

func writeSeed(t *testing.T, seed proto.GuestSeed) string {
	t.Helper()
	raw, err := json.Marshal(seed)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	if err := node.WriteSeedISO(dir, map[string][]byte{proto.GuestSeedFile: raw}); err != nil {
		t.Fatal(err)
	}
	return filepath.Join(dir, node.SeedISO)
}

func TestReadSeedSanitizesHostname(t *testing.T) {
	seed, err := ReadSeed(writeSeed(t, proto.GuestSeed{Hostname: "vm-"}))
	if err != nil || seed.Hostname != "vm" {
		t.Fatalf("hostname %q, %v; want vm", seed.Hostname, err)
	}
}

func TestSanitizeHostname(t *testing.T) {
	long := strings.Repeat("a", 62) + "-b"
	for in, want := range map[string]string{
		"vm1":   "vm1",
		"a;b":   "ab",
		"-x-":   "x",
		"my-vm": "my-vm",
		"":      "vm",
		";;":    "vm",
		long:    strings.Repeat("a", 62),
	} {
		if got := sanitizeHostname(in); got != want {
			t.Errorf("sanitizeHostname(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestReadSeedDropsKeysWithNewlines(t *testing.T) {
	seed, err := ReadSeed(writeSeed(t, proto.GuestSeed{
		Hostname: "vm1",
		SSHKeys:  []string{"ssh-ed25519 AAA good", "ssh-ed25519 BBB\nssh-ed25519 CCC evil", "  "},
	}))
	if err != nil {
		t.Fatal(err)
	}
	if len(seed.SSHKeys) != 1 || seed.SSHKeys[0] != "ssh-ed25519 AAA good" {
		t.Fatalf("keys = %q", seed.SSHKeys)
	}
}

func TestReadSeedDropsBadEnvNames(t *testing.T) {
	seed, err := ReadSeed(writeSeed(t, proto.GuestSeed{
		Hostname: "vm1",
		Env:      map[string]string{"A-B": "1", "1X": "2", "PATH": "/bin"},
	}))
	if err != nil {
		t.Fatal(err)
	}
	if len(seed.Env) != 1 || seed.Env["PATH"] != "/bin" {
		t.Fatalf("env = %v", seed.Env)
	}
}

func fakeSysBlock(t *testing.T, ro map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	for name, v := range ro {
		if err := os.MkdirAll(filepath.Join(dir, name), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, name, "ro"), []byte(v+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func TestFindSeedDevice(t *testing.T) {
	got, err := FindSeedDevice(fakeSysBlock(t, map[string]string{"vda": "0", "vdb": "0", "vdc": "1"}))
	if err != nil || got != "/dev/vdc" {
		t.Fatalf("got %q, %v", got, err)
	}
}

func TestFindSeedDeviceNoReadOnlyDisk(t *testing.T) {
	if _, err := FindSeedDevice(fakeSysBlock(t, map[string]string{"vda": "0", "vdb": "0"})); err == nil {
		t.Fatal("want error")
	}
}

package guestinit

import (
	"encoding/json"
	"path/filepath"
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

func TestReadSeedRejectsBadHostname(t *testing.T) {
	if _, err := ReadSeed(writeSeed(t, proto.GuestSeed{Hostname: "a;b"})); err == nil {
		t.Fatal("want error for hostname a;b")
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

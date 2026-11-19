package guestinit

import (
	"bytes"
	"os"
	"testing"
)

func TestHostKeysStableUnlessRegenerate(t *testing.T) {
	r := Root{t.TempDir()}
	marshal := func(regenerate bool) [][]byte {
		t.Helper()
		signers, err := r.HostKeys(regenerate)
		if err != nil {
			t.Fatal(err)
		}
		var out [][]byte
		for _, s := range signers {
			out = append(out, s.PublicKey().Marshal())
		}
		return out
	}
	first := marshal(false)
	if len(first) != 2 {
		t.Fatalf("%d keys, want ed25519 + ecdsa", len(first))
	}
	for _, name := range hostKeyFiles {
		fi, err := os.Stat(r.join(hostKeyDir + "/" + name))
		if err != nil || fi.Mode().Perm() != 0o600 {
			t.Fatalf("%s: %v %v", name, fi, err)
		}
	}
	again := marshal(false)
	for i := range first {
		if !bytes.Equal(first[i], again[i]) {
			t.Fatal("keys changed without regenerate")
		}
	}
	fresh := marshal(true)
	for i := range first {
		if bytes.Equal(first[i], fresh[i]) {
			t.Fatal("regenerate kept a key")
		}
	}
}

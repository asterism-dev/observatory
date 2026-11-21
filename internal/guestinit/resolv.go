package guestinit

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"slices"
	"strings"

	"github.com/asterism-dev/observatory/internal/proto"
)

const (
	mgmtNameserver = "10.192.0.1"
	instanceIDFile = "var/lib/observatory/instance-id"
)

// WriteResolv points the VM at the gateway's resolver, reached over the mgmt NIC; without one it leaves the image's file alone rather than pick resolvers itself.
func (r Root) WriteResolv(nics []proto.GuestNIC) error {
	if !slices.ContainsFunc(nics, func(n proto.GuestNIC) bool { return n.Mgmt }) {
		return nil
	}
	path := r.join("etc/resolv.conf")
	// Images often link it to a resolver stub that nothing runs here; writing through the link would also follow it out of the root.
	if fi, err := os.Lstat(path); err == nil && fi.Mode()&os.ModeSymlink != 0 {
		if err := os.Remove(path); err != nil {
			return err
		}
	}
	return writeFileAtomic(path, []byte("nameserver "+mgmtNameserver+"\n"), 0o644)
}

// WriteHostname writes hostname and its hosts entry, and a new machine-id whenever instanceID differs from the last boot's.
func (r Root) WriteHostname(name, instanceID string) error {
	if !hostnameRE.MatchString(name) {
		return fmt.Errorf("invalid hostname %q", name)
	}
	if err := writeFileAtomic(r.join("etc/hostname"), []byte(name+"\n"), 0o644); err != nil {
		return err
	}
	if err := r.writeHosts(name); err != nil {
		return err
	}
	last, _ := os.ReadFile(r.join(instanceIDFile))
	if mid, err := os.ReadFile(r.join("etc/machine-id")); err == nil && len(mid) == 33 && string(last) == instanceID+"\n" {
		return nil
	}
	id := make([]byte, 16)
	rand.Read(id)
	if err := writeFileAtomic(r.join("etc/machine-id"), []byte(hex.EncodeToString(id)+"\n"), 0o444); err != nil {
		return err
	}
	return writeFileAtomic(r.join(instanceIDFile), []byte(instanceID+"\n"), 0o644)
}

func (r Root) writeHosts(name string) error {
	path := r.join("etc/hosts")
	lines, err := readLines(path)
	if err != nil {
		return err
	}
	if len(lines) == 0 {
		lines = []string{"127.0.0.1\tlocalhost", "::1\tlocalhost ip6-localhost ip6-loopback"}
	}
	kept := lines[:0]
	for _, l := range lines {
		if f := strings.Fields(l); len(f) == 0 || f[0] != "127.0.1.1" {
			kept = append(kept, l)
		}
	}
	return writeLines(path, append(kept, "127.0.1.1\t"+name), 0o644)
}

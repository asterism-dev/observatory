package guestinit

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/asterism-dev/observatory/internal/proto"
)

func TestWriteResolvMgmtFirst(t *testing.T) {
	r := Root{t.TempDir()}
	if err := r.WriteResolv([]proto.GuestNIC{{Name: "eth0"}, {Name: "mgmt0", Mgmt: true, DHCP: true}}); err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(r.join("etc/resolv.conf"))
	if want := "nameserver 10.192.0.1\nnameserver 1.1.1.1\nnameserver 9.9.9.9\n"; string(got) != want {
		t.Fatalf("resolv.conf = %q, want %q", got, want)
	}
	if err := r.WriteResolv([]proto.GuestNIC{{Name: "eth0"}}); err != nil {
		t.Fatal(err)
	}
	got, _ = os.ReadFile(r.join("etc/resolv.conf"))
	if want := "nameserver 1.1.1.1\nnameserver 9.9.9.9\n"; string(got) != want {
		t.Fatalf("without mgmt: resolv.conf = %q, want %q", got, want)
	}
}

func TestWriteResolvReplacesSymlink(t *testing.T) {
	r := Root{t.TempDir()}
	outside := filepath.Join(t.TempDir(), "stub-resolv.conf")
	if err := os.WriteFile(outside, []byte("nameserver 127.0.0.53\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(r.join("etc"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, r.join("etc/resolv.conf")); err != nil {
		t.Fatal(err)
	}
	if err := r.WriteResolv(nil); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Lstat(r.join("etc/resolv.conf"))
	if err != nil || !fi.Mode().IsRegular() || fi.Mode().Perm() != 0o644 {
		t.Fatalf("resolv.conf not a 0644 file: %v %v", fi, err)
	}
	if got, _ := os.ReadFile(outside); string(got) != "nameserver 127.0.0.53\n" {
		t.Fatalf("symlink target changed: %q", got)
	}
}

func TestWriteHostnameNewMachineIDOnTakeoff(t *testing.T) {
	r := Root{t.TempDir()}
	machineID := func() string {
		t.Helper()
		b, err := os.ReadFile(r.join("etc/machine-id"))
		if err != nil {
			t.Fatal(err)
		}
		if len(b) != 33 || b[32] != '\n' {
			t.Fatalf("machine-id %q", b)
		}
		return string(b)
	}
	if err := r.WriteHostname("vm-a", "inst-1"); err != nil {
		t.Fatal(err)
	}
	first := machineID()
	if err := r.WriteHostname("vm-a", "inst-1"); err != nil {
		t.Fatal(err)
	}
	if machineID() != first {
		t.Fatal("machine-id changed on a plain reboot")
	}
	if err := r.WriteHostname("vm-b", "inst-2"); err != nil {
		t.Fatal(err)
	}
	if machineID() == first {
		t.Fatal("machine-id kept on takeoff")
	}
	if got, _ := os.ReadFile(r.join("etc/hostname")); string(got) != "vm-b\n" {
		t.Fatalf("hostname = %q", got)
	}
	hosts, _ := os.ReadFile(r.join("etc/hosts"))
	if !strings.Contains(string(hosts), "127.0.1.1\tvm-b\n") || strings.Contains(string(hosts), "vm-a") || !strings.Contains(string(hosts), "localhost") {
		t.Fatalf("hosts:\n%s", hosts)
	}
	if err := r.WriteHostname("bad name\n", "inst-3"); err == nil {
		t.Fatal("invalid hostname accepted")
	}
}

package linuxvm

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestAttachNBD needs root, VMM_TEST_NBD pointing at an export served with TLS-PSK (e.g. nbdkit --tls=require --tls-psk=<file>) and its key in VMM_TEST_NBD_PSK.
func TestAttachNBD(t *testing.T) {
	url, psk := os.Getenv("VMM_TEST_NBD"), os.Getenv("VMM_TEST_NBD_PSK")
	if url == "" || os.Geteuid() != 0 {
		t.Skip("needs root, VMM_TEST_NBD and VMM_TEST_NBD_PSK")
	}
	dir := t.TempDir()
	dev, err := AttachNBD(dir, "d1", url, psk)
	if err != nil {
		t.Fatal(err)
	}
	if !NBDConnected(dev) {
		t.Fatalf("%s not connected", dev)
	}
	again, err := AttachNBD(dir, "d1", url, psk)
	if err != nil || again != dev {
		t.Fatalf("second attach = %s, %v; want %s reused", again, err, dev)
	}
	if err := DetachNBD(dir, "d1"); err != nil {
		t.Fatal(err)
	}
	if NBDConnected(dev) {
		t.Fatalf("%s still connected after detach", dev)
	}
	if _, err := os.Stat(filepath.Join(dir, "nbd-d1-tls")); err == nil {
		t.Fatal("key dir left after detach")
	}
	start := time.Now()
	if _, err := AttachNBD(dir, "d1", url, strings.Repeat("0", 64)); err == nil {
		DetachNBD(dir, "d1")
		t.Fatal("wrong key accepted")
	} else if time.Since(start) > 5*time.Second {
		t.Errorf("wrong key refused after %s: %v", time.Since(start), err)
	} else {
		t.Logf("wrong key refused after %s: %v", time.Since(start), err)
	}
}

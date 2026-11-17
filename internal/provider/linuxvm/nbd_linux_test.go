package linuxvm

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/asterism-dev/observatory/internal/storage"
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

func TestAttachNBDRejectsOtherExport(t *testing.T) {
	dir := t.TempDir()
	_, err := AttachNBD(dir, "d1", "nbd://127.0.0.1:10810/d2", strings.Repeat("0", 64))
	if err == nil || !strings.Contains(err.Error(), "export d2") {
		t.Fatalf("err = %v, want an export mismatch", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "nbd-d1-tls")); err == nil {
		t.Fatal("key dir written for a mismatched export")
	}
}

// TestDetachNBDFenced needs root, nbdkit and qemu-nbd, and VMM_TEST_NBD_FENCE=1: it serves a local export, attaches it and detaches it healthy and fenced.
func TestDetachNBDFenced(t *testing.T) {
	if os.Getenv("VMM_TEST_NBD_FENCE") == "" || os.Geteuid() != 0 {
		t.Skip("needs root and VMM_TEST_NBD_FENCE=1")
	}
	old := nbdStopWait
	nbdStopWait = 3 * time.Second
	t.Cleanup(func() { nbdStopWait = old })
	dir, srv := t.TempDir(), t.TempDir()
	img, keys, psk := filepath.Join(srv, "d1.img"), filepath.Join(srv, "keys.psk"), strings.Repeat("ab", 32)
	if err := os.WriteFile(keys, []byte("d1:"+psk+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	f, err := os.Create(img)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Truncate(64 << 20); err != nil {
		t.Fatal(err)
	}
	f.Close()
	serve := func(port int) *exec.Cmd {
		cmd := exec.Command("nbdkit", storage.NBDKitArgs("127.0.0.1", port, keys, img)...)
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { cmd.Process.Kill(); cmd.Wait() })
		time.Sleep(500 * time.Millisecond)
		return cmd
	}
	url := "nbd://127.0.0.1:10899/d1"
	dd := func(args ...string) *exec.Cmd {
		return exec.Command("dd", append([]string{"bs=1M", "count=4", "status=none"}, args...)...)
	}

	server := serve(10899)
	dev, err := AttachNBD(dir, "d1", url, psk)
	if err != nil {
		t.Fatal(err)
	}
	if out, err := dd("if=/dev/urandom", "of="+dev, "oflag=direct").CombinedOutput(); err != nil {
		t.Fatalf("write: %v %s", err, out)
	}
	want, err := os.ReadFile(img)
	if err != nil {
		t.Fatal(err)
	}
	pid, err := nbdClientPID(dev)
	if err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	if err := DetachNBD(dir, "d1"); err != nil || NBDConnected(dev) {
		t.Fatalf("healthy detach: %v, connected %v", err, NBDConnected(dev))
	}
	if took := time.Since(start); took >= nbdStopWait {
		t.Errorf("healthy detach needed SIGKILL (%s)", took)
	}
	t.Logf("healthy detach took %s", time.Since(start))
	if _, err := os.Stat(fmt.Sprintf("/proc/%d", pid)); err == nil {
		t.Errorf("qemu-nbd %d still running after the healthy detach", pid)
	}

	if dev, err = AttachNBD(dir, "d1", url, psk); err != nil {
		t.Fatal(err)
	}
	got, err := exec.Command("dd", "if="+dev, "bs=1M", "count=64", "iflag=direct", "status=none").Output()
	if err != nil || !bytes.Equal(got, want) {
		t.Fatalf("data after reattach differs (err %v)", err)
	}

	// A later attach of the same disk gets a new port: the device of the old one must not be reused.
	server.Process.Kill()
	server.Wait()
	if pid, err = nbdClientPID(dev); err != nil {
		t.Fatal(err)
	}
	server = serve(10898)
	url2 := "nbd://127.0.0.1:10898/d1"
	if dev, err = AttachNBD(dir, "d1", url2, psk); err != nil {
		t.Fatal(err)
	}
	if rec, _ := os.ReadFile(NBDRecord(dir, "d1")); !strings.Contains(string(rec), url2) {
		t.Fatalf("record %q lacks the new URL", rec)
	}
	if _, err := os.Stat(fmt.Sprintf("/proc/%d", pid)); err == nil {
		t.Errorf("qemu-nbd %d of the old URL still running", pid)
	}
	if got, err = exec.Command("dd", "if="+dev, "bs=1M", "count=64", "iflag=direct", "status=none").Output(); err != nil || !bytes.Equal(got, want) {
		t.Fatalf("data via the new URL differs (err %v)", err)
	}
	server.Process.Kill()
	server.Wait()
	writer := dd("if=/dev/zero", "of="+dev, "oflag=direct")
	if err := writer.Start(); err != nil {
		t.Fatal(err)
	}
	exited := make(chan error, 1)
	go func() { exited <- writer.Wait() }()
	select {
	case err := <-exited:
		t.Fatalf("writer to a fenced device did not block: %v", err)
	case <-time.After(2 * time.Second):
	}
	if pid, err = nbdClientPID(dev); err != nil {
		t.Fatal(err)
	}
	start = time.Now()
	if err := DetachNBD(dir, "d1"); err != nil || NBDConnected(dev) {
		t.Fatalf("fenced detach: %v, connected %v", err, NBDConnected(dev))
	}
	t.Logf("fenced detach took %s", time.Since(start))
	if _, err := os.Stat(fmt.Sprintf("/proc/%d", pid)); err == nil {
		t.Errorf("qemu-nbd %d still running after the fenced detach", pid)
	}
	select {
	case err := <-exited:
		t.Logf("blocked writer exited: %v", err)
	case <-time.After(10 * time.Second):
		t.Fatal("writer still blocked after the fenced detach")
	}
}

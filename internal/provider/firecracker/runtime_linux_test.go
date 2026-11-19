package firecracker

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/asterism-dev/observatory/internal/node"
	"github.com/asterism-dev/observatory/internal/proto"
)

// TestAlpineImageBoots needs VMM_TEST_ALPINE_IMAGE, ociimage's ext4 disk of alpine:3: no systemd, no sudo, no bash.
func TestAlpineImageBoots(t *testing.T) {
	p := testProviderImage(t, "VMM_TEST_ALPINE_IMAGE", "")
	p.fastBoot = false
	key := filepath.Join(t.TempDir(), "id")
	if out, err := exec.Command("ssh-keygen", "-q", "-t", "ed25519", "-N", "", "-f", key).CombinedOutput(); err != nil {
		t.Fatalf("%v: %s", err, out)
	}
	pub, _ := os.ReadFile(key + ".pub")
	ca := testCAPEM(t)
	f := testVM("fcalpine")
	f.Spec.MemoryMB = 512
	f.Boot = &proto.Boot{SSHPublicKeys: []string{string(pub)}, EgressCAs: map[string]string{"0b6a2d1e-7c1f-4c55-9a43-2f0f3c5c1e01": ca}}
	// dev cannot signal the root-owned service in an image without sudo, so the command ends itself once dev creates /tmp/stop.
	f.ImageConfig = proto.ImageConfig{Cmd: []string{"sh", "-c", "echo up >>/tmp/cmd; until [ -e /tmp/stop ]; do sleep 1; done; rm /tmp/stop"}}
	inst := startVM(t, p, f)
	defer func() { inst.Kill(); <-inst.Done() }()
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	ip, err := inst.WaitIP(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := waitAgent(ctx, node.VMDir(p.VMsDir, f.Spec.ID), inst); err != nil {
		t.Fatal(err)
	}
	if got := sshVM(t, key, ip, "id -un; grep ^dev: /etc/passwd | cut -d: -f7; readlink -f /proc/$$/exe"); got != "dev\n/bin/sh\n/bin/busybox" {
		t.Fatalf("login = %q, want dev with /bin/sh (busybox)", got)
	}
	// Container runtimes in the image (the builder's BuildKit) need the image's own cgroup2 mount.
	if got := sshVM(t, key, ip, "awk '$2==\"/sys/fs/cgroup\"{print $3}' /proc/mounts"); got != "cgroup2" {
		t.Fatalf("/sys/fs/cgroup mounted as %q, want cgroup2", got)
	}
	bundle := sshVM(t, key, ip, "cat /etc/ssl/certs/ca-certificates.crt")
	if !strings.Contains(bundle, "# BEGIN observatory egress\n"+ca+"# END observatory egress") {
		t.Fatalf("CA bundle lacks the egress block:\n%s", bundle[max(0, len(bundle)-2000):])
	}
	if got := waitSSH(t, key, ip, "cat /tmp/cmd", "up"); got != "up" {
		t.Fatalf("/tmp/cmd = %q, want the command's first run", got)
	}
	sshVM(t, key, ip, "touch /tmp/stop")
	if got := waitSSH(t, key, ip, "cat /tmp/cmd", "up\nup"); got != "up\nup" {
		t.Fatalf("/tmp/cmd = %q: command not restarted", got)
	}
}

// waitSSH runs cmd until it prints want or 30 s pass, and returns the last output.
func waitSSH(t *testing.T, key, ip, cmd, want string) string {
	t.Helper()
	var got string
	for deadline := time.Now().Add(30 * time.Second); time.Now().Before(deadline); time.Sleep(time.Second) {
		if got = sshVM(t, key, ip, cmd+" 2>/dev/null; true"); got == want {
			break
		}
	}
	return got
}

func testCAPEM(t *testing.T) string {
	t.Helper()
	k, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "test egress CA"}, NotBefore: time.Now(), NotAfter: time.Now().Add(time.Hour), IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign}
	der, err := x509.CreateCertificate(rand.Reader, tpl, tpl, &k.PublicKey, k)
	if err != nil {
		t.Fatal(err)
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}))
}

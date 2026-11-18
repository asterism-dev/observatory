package firecracker

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/asterism-dev/observatory/internal/node"
	"github.com/asterism-dev/observatory/internal/proto"
)

func templateVM(t *testing.T, id, pub string) node.VMFile {
	f := testVM(id)
	f.MAC = node.RandomMAC()
	f.Spec.MemoryMB = 512
	// The port matches no OVN logical port; the NIC only has to exist for the NIC count and MAC checks.
	f.Spec.NICs = []proto.NIC{{Network: "net", MAC: node.RandomMAC(), Port: "fctpl-" + id}}
	f.Boot = &proto.Boot{SSHPublicKeys: []string{pub}}
	return f
}

func testKey(t *testing.T) (key, pub string) {
	key = filepath.Join(t.TempDir(), "id")
	if out, err := exec.Command("ssh-keygen", "-q", "-t", "ed25519", "-N", "", "-f", key).CombinedOutput(); err != nil {
		t.Fatalf("%v: %s", err, out)
	}
	b, _ := os.ReadFile(key + ".pub")
	return key, string(b)
}

func keyFor(t *testing.T, p *Provider, f node.VMFile) string {
	t.Helper()
	base, err := node.EnsureBaseImage(p.ImagesDir, p.ImageURL)
	if err != nil {
		t.Fatal(err)
	}
	key, err := templateKey(base, p.fcVersion, f)
	if err != nil {
		t.Fatal(err)
	}
	return key
}

func prepare(t *testing.T, p *Provider, f node.VMFile) string {
	t.Helper()
	dir := node.VMDir(p.VMsDir, f.Spec.ID)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := p.Prepare(context.Background(), f, dir); err != nil {
		t.Fatal(err)
	}
	return dir
}

func fileSum(t *testing.T, path string) string {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		t.Fatal(err)
	}
	return hex.EncodeToString(h.Sum(nil))
}

func TestFastBootOff(t *testing.T) {
	for _, on := range []bool{false, true} {
		p := testProviderWith(t, fmt.Sprintf(`,"fastBoot":%v`, on))
		f := testVM("fcoff")
		tpl := p.templateDir(keyFor(t, p, f))
		if err := os.MkdirAll(tpl, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(tpl, "root.img"), nil, 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.Truncate(filepath.Join(tpl, "root.img"), templateDiskGB<<30); err != nil {
			t.Fatal(err)
		}
		dir := prepare(t, p, f)
		_, err := os.Stat(filepath.Join(dir, templateMarker))
		if on != (err == nil) {
			t.Errorf("fastBoot %v: marker stat = %v", on, err)
		}
	}
}

func TestFastBoot(t *testing.T) {
	p := testProviderWith(t, "")
	sshKey, pub := testKey(t)
	f1 := templateVM(t, "fcfb1", pub)
	key := keyFor(t, p, f1)
	tpl := p.templateDir(key)
	inst1 := startVM(t, p, f1)
	if _, err := os.Stat(filepath.Join(node.VMDir(p.VMsDir, f1.Spec.ID), templateMarker)); err == nil {
		t.Fatal("first VM of its shape got a template marker")
	}
	began := time.Now()
	for {
		if _, err := os.Stat(tpl); err == nil {
			break
		}
		if time.Since(began) > templateBuildTimeout {
			inst1.Kill()
			t.Fatal("no template built")
		}
		time.Sleep(2 * time.Second)
	}
	t.Logf("template %s built %s after the first VM started", key, time.Since(began).Round(time.Second))
	inst1.Kill()
	<-inst1.Done()
	memSum := fileSum(t, filepath.Join(tpl, node.MemFile))

	type guestID struct{ hostname, machineID, macs, hostKey, random, rootGB string }
	var ids []guestID
	for _, id := range []string{"fcfb2", "fcfb3"} {
		f := templateVM(t, id, pub)
		dir := prepare(t, p, f)
		if _, err := os.Stat(filepath.Join(dir, templateMarker)); err != nil {
			t.Fatalf("%s: no template marker: %v", id, err)
		}
		start := time.Now()
		inst, err := p.Start(context.Background(), f, dir)
		if err != nil {
			t.Fatal(err)
		}
		took := time.Since(start)
		defer func() { inst.Kill(); <-inst.Done() }()
		t.Logf("%s started from the template in %s", id, took.Round(time.Millisecond))
		if !inst.(*instance).restored {
			t.Fatalf("%s booted cold", id)
		}
		// The target is < 2 s on bare metal; the reference host (2 cores, nested KVM) gets 3 s.
		if took > 3*time.Second {
			t.Errorf("%s took %s to start from the template, want < 3s", id, took)
		}
		if _, err := os.Stat(filepath.Join(dir, templateMarker)); err == nil {
			t.Errorf("%s: marker left after start", id)
		}
		ip, err := inst.WaitIP(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		// The RSA host key is made in the background after takeoff; waiting for it also keeps it off the next VM's clock.
		if got := sshVM(t, sshKey, ip, "timeout 60 sh -c 'until [ -s /etc/ssh/ssh_host_rsa_key.pub ]; do sleep 0.2; done' && echo ok"); got != "ok" {
			t.Errorf("%s: no RSA host key: %q", id, got)
		}
		out := sshVM(t, sshKey, ip, "hostname; cat /etc/machine-id; cat /sys/class/net/*/address | grep -v 00:00:00:00:00:00 | sort | tr '\\n' ' '; echo; "+
			"head -c 16 /dev/urandom | od -An -tx1 | tr -d ' \\n'; echo; df -B1G --output=size / | tail -1; ip -4 -o addr show | grep -c 192.168.64.; ls /sys/bus/acpi/drivers/vmgenid 2>/dev/null | tr '\\n' ' '; echo")
		lines := strings.Split(out, "\n")
		if len(lines) < 6 {
			t.Fatalf("%s: guest output %q", id, out)
		}
		scan, err := exec.Command("sh", "-c", "ssh-keyscan -t ed25519 "+ip+" 2>/dev/null | ssh-keygen -lf -").Output()
		if err != nil {
			t.Fatal(err)
		}
		g := guestID{hostname: lines[0], machineID: lines[1], macs: strings.TrimSpace(lines[2]), random: lines[3], rootGB: strings.TrimSpace(lines[4]), hostKey: strings.TrimSpace(string(scan))}
		if lines[5] != "1" {
			t.Errorf("%s: %s addresses in 192.168.64.0/24, want only its own", id, lines[5])
		}
		vmgenid := ""
		if len(lines) > 6 {
			vmgenid = lines[6]
		}
		t.Logf("%s: %+v, vmgenid driver: %q", id, g, vmgenid)
		if g.hostname != f.Spec.Name {
			t.Errorf("%s: hostname %q", id, g.hostname)
		}
		want := []string{f.MAC, f.Spec.NICs[0].MAC}
		sort.Strings(want)
		if g.macs != strings.Join(want, " ") {
			t.Errorf("%s: MACs %q, want %q", id, g.macs, want)
		}
		if gb, _ := strconv.Atoi(g.rootGB); gb < f.Spec.DiskGB-1 {
			t.Errorf("%s: root filesystem %s GB, want %d", id, g.rootGB, f.Spec.DiskGB)
		}
		if g.hostKey == "" {
			t.Errorf("%s: no SSH host key", id)
		}
		ids = append(ids, g)
		if id == "fcfb3" {
			// A cold boot of a clone must not look like a new cloud-init instance, which would make new host keys.
			if err := inst.Stop(); err != nil {
				t.Fatal(err)
			}
			if inst, err = p.Start(context.Background(), f, dir); err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
			defer cancel()
			if _, err := inst.WaitIP(ctx); err != nil {
				t.Fatal(err)
			}
			sshVM(t, sshKey, ip, "cloud-init status --wait >/dev/null || true")
			after, err := exec.Command("sh", "-c", "ssh-keyscan -t ed25519 "+ip+" 2>/dev/null | ssh-keygen -lf -").Output()
			if err != nil {
				t.Fatal(err)
			}
			if got := strings.TrimSpace(string(after)); got != g.hostKey {
				t.Errorf("%s: host key changed on reboot: %s, was %s", id, got, g.hostKey)
			}
		}
	}
	a, b := ids[0], ids[1]
	if a.machineID == b.machineID || a.macs == b.macs || a.hostKey == b.hostKey || a.random == b.random {
		t.Errorf("clones share identity or randomness:\n%+v\n%+v", a, b)
	}
	for _, n := range []string{node.StateFile, node.MemFile, "root.img"} {
		st, err := os.Stat(filepath.Join(tpl, n))
		if err != nil {
			t.Fatal(err)
		}
		if sys := st.Sys().(*syscall.Stat_t); sys.Uid != 0 || sys.Gid != 0 || st.Mode().Perm() != 0o644 {
			t.Errorf("template %s: uid %d gid %d mode %v, want root 0644", n, sys.Uid, sys.Gid, st.Mode().Perm())
		}
	}
	if fileSum(t, filepath.Join(tpl, node.MemFile)) != memSum {
		t.Error("template memory file changed while VMs ran from it")
	}
}

func TestFastBootTakeoffFailureFallsBack(t *testing.T) {
	p := testProviderWith(t, "")
	sshKey, pub := testKey(t)
	f0 := templateVM(t, "fcbad0", pub)
	p.fastBoot = false
	inst0 := startVM(t, p, f0)
	p.fastBoot = true
	defer func() { inst0.Kill(); <-inst0.Done() }()
	ip, err := inst0.WaitIP(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	templateID := sshVM(t, sshKey, ip, "cloud-init status --wait >/dev/null; sudo systemctl stop vmm-agent; cat /etc/machine-id")
	tpl := p.templateDir(keyFor(t, p, f0))
	if err := saveTemplate(inst0, node.VMDir(p.VMsDir, f0.Spec.ID), tpl); err != nil {
		t.Fatal(err)
	}
	inst0.Kill()

	f := templateVM(t, "fcbad1", pub)
	dir := prepare(t, p, f)
	if _, err := os.Stat(filepath.Join(dir, templateMarker)); err != nil {
		t.Fatalf("no template marker: %v", err)
	}
	inst, err := p.Start(context.Background(), f, dir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { inst.Kill(); <-inst.Done() }()
	if inst.(*instance).restored {
		t.Fatal("started from a template whose agent does not answer")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
	defer cancel()
	ip, err = inst.WaitIP(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if id := sshVM(t, sshKey, ip, "cloud-init status --wait >/dev/null; cat /etc/machine-id"); id == templateID {
		t.Errorf("fallback VM booted the template's disk: machine id %s", id)
	}
	if _, err := os.Stat(tpl); !os.IsNotExist(err) {
		t.Errorf("template left after a failed takeoff: %v", err)
	}
}

func TestFastBootCancelledTakeoffReclonesBase(t *testing.T) {
	p := testProviderWith(t, "")
	_, pub := testKey(t)
	f := templateVM(t, "fccancel", pub)
	key := keyFor(t, p, f)
	if err := os.MkdirAll(p.VMsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := p.buildTemplate(context.Background(), key, f); err != nil {
		t.Fatal(err)
	}
	dir := prepare(t, p, f)
	if _, err := os.Stat(filepath.Join(dir, templateMarker)); err != nil {
		t.Fatalf("no template marker: %v", err)
	}
	// Cancelled before the takeoff call: the restore runs up to the guest agent call and fails there.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if inst, err := p.Start(ctx, f, dir); err == nil {
		inst.Kill()
		t.Fatal("start with a cancelled context succeeded")
	}
	base, err := node.EnsureBaseImage(p.ImagesDir, p.ImageURL)
	if err != nil {
		t.Fatal(err)
	}
	st, _ := os.Stat(base)
	if out, err := exec.Command("cmp", "-n", strconv.FormatInt(st.Size(), 10), base, filepath.Join(dir, "root.img")).CombinedOutput(); err != nil {
		t.Errorf("root.img is not a fresh base clone after a cancelled takeoff: %v: %s", err, out)
	}
	if _, err := os.Stat(p.templateDir(key)); err != nil {
		t.Errorf("template deleted after a cancelled takeoff: %v", err)
	}
}

package firecracker

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/asterism-dev/observatory/internal/node"
	"github.com/asterism-dev/observatory/internal/proto"
	"github.com/asterism-dev/observatory/internal/provider/linuxvm"
)

const testImageURL = "test://noble"

// testProvider needs root, KVM, bridge vmm0 and VMM_TEST_IMAGE (a raw bootable noble disk).
func testProvider(t *testing.T) *Provider {
	t.Helper()
	img := os.Getenv("VMM_TEST_IMAGE")
	if img == "" || os.Geteuid() != 0 {
		t.Skip("needs root and VMM_TEST_IMAGE")
	}
	base := t.TempDir()
	images := filepath.Join(base, "images")
	if err := os.MkdirAll(images, 0o755); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256([]byte(testImageURL))
	if err := os.Link(img, filepath.Join(images, hex.EncodeToString(sum[:8])+".raw")); err != nil {
		t.Fatal(err)
	}
	p, err := New(node.Config{Provider: "firecracker", Paths: struct {
		VMs    string `json:"vms"`
		Images string `json:"images"`
	}{VMs: filepath.Join(base, "vms"), Images: images}, Raw: map[string]json.RawMessage{"firecracker": json.RawMessage(`{"imageURL":"` + testImageURL + `"}`)}})
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func testVM(id string) node.VMFile {
	return node.VMFile{MAC: "06:00:00:00:00:01", Spec: proto.VMSpec{ID: id, Name: id, CPUs: 1, MemoryMB: 1024, DiskGB: 10}}
}

func startVM(t *testing.T, p *Provider, f node.VMFile) node.Instance {
	t.Helper()
	dir := node.VMDir(p.VMsDir, f.Spec.ID)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := p.Prepare(context.Background(), f, dir); err != nil {
		t.Fatal(err)
	}
	inst, err := p.Start(f, dir)
	if err != nil {
		t.Fatal(err)
	}
	return inst
}

func TestReseedKeepsSeedWithoutBoot(t *testing.T) {
	p := &Provider{VMsDir: t.TempDir()}
	f := testVM("fcreseed")
	dir := node.VMDir(p.VMsDir, f.Spec.ID)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	seedFile := func(name string) []byte {
		t.Helper()
		b, err := node.ReadSeedFile(dir, name)
		if err != nil {
			t.Fatal(err)
		}
		return b
	}
	f.Boot = &proto.Boot{SSHPublicKeys: []string{"ssh-ed25519 AAAA one", "ssh-rsa BBBB two"}, Env: map[string]string{"A": "x\ny", "B": `it's $HOME \ "q"`}}
	if err := p.Reseed(context.Background(), f, dir); err != nil {
		t.Fatal(err)
	}
	env, keys := seedFile("vmm-env"), seedFile("vmm-authorized-keys")

	f.Boot = nil
	f.Spec.NICs = []proto.NIC{{Network: "mgmt", MAC: "02:0a:c8:00:00:0a"}}
	if err := p.Reseed(context.Background(), f, dir); err != nil {
		t.Fatal(err)
	}
	if got := seedFile("vmm-env"); !bytes.Equal(got, env) {
		t.Fatalf("vmm-env after nil-Boot reseed = %q, want %q", got, env)
	}
	if got := seedFile("vmm-authorized-keys"); !bytes.Equal(got, keys) {
		t.Fatalf("vmm-authorized-keys after nil-Boot reseed = %q, want %q", got, keys)
	}
	if nc := seedFile("network-config"); !bytes.Contains(nc, []byte("02:0a:c8:00:00:0a")) {
		t.Fatalf("new NIC missing from network-config:\n%s", nc)
	}

	f.Boot = &proto.Boot{Env: map[string]string{"A": "two"}}
	if err := p.Reseed(context.Background(), f, dir); err != nil {
		t.Fatal(err)
	}
	if got := seedFile("vmm-env"); string(got) != `{"A":"two"}` {
		t.Fatalf("vmm-env after new Boot = %q", got)
	}
	if got := seedFile("vmm-authorized-keys"); len(got) != 0 {
		t.Fatalf("vmm-authorized-keys after new Boot = %q", got)
	}
}

func TestReseedWithoutBootBuildsFirstSeed(t *testing.T) {
	p := &Provider{VMsDir: t.TempDir()}
	f := testVM("fcfirstseed")
	dir := node.VMDir(p.VMsDir, f.Spec.ID)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := p.Reseed(context.Background(), f, dir); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, node.SeedISO)); err != nil {
		t.Fatal(err)
	}
}

func TestStartStopVM(t *testing.T) {
	p := testProvider(t)
	f := testVM("fcstartstop")
	inst := startVM(t, p, f)
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	ip, err := inst.WaitIP(ctx)
	if err != nil {
		inst.Kill()
		t.Fatalf("WaitIP: %v", err)
	}
	if want, _ := linuxvm.AllocNATIP(p.VMsDir, f.Spec.ID); ip != want {
		t.Errorf("WaitIP = %q, want the NAT address %q", ip, want)
	}
	if err := inst.Stop(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-inst.Done():
	case <-time.After(15 * time.Second):
		t.Fatal("not stopped within 15s")
	}
	if _, err := net.InterfaceByName(linuxvm.TapName(f.Spec.ID, 0)); err == nil {
		t.Error("tap still exists after stop")
	}
}

func TestStartCleansStaleTaps(t *testing.T) {
	p := testProvider(t)
	f := testVM("fcstaletap")
	if out, err := exec.Command("ip", "tuntap", "add", "dev", linuxvm.TapName(f.Spec.ID, 0), "mode", "tap").CombinedOutput(); err != nil {
		t.Fatalf("%v: %s", err, out)
	}
	inst := startVM(t, p, f)
	inst.Kill()
	<-inst.Done()
}

func TestGuestPoweroffEndsInstance(t *testing.T) {
	p := testProvider(t)
	key := filepath.Join(t.TempDir(), "id")
	if out, err := exec.Command("ssh-keygen", "-q", "-t", "ed25519", "-N", "", "-f", key).CombinedOutput(); err != nil {
		t.Fatalf("%v: %s", err, out)
	}
	pub, _ := os.ReadFile(key + ".pub")
	f := testVM("fcpoweroff")
	f.Boot = &proto.Boot{SSHPublicKeys: []string{string(pub)}}
	inst := startVM(t, p, f)
	defer inst.Kill()
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	if _, err := inst.WaitIP(ctx); err != nil {
		t.Fatal(err)
	}
	ip, err := linuxvm.AllocNATIP(p.VMsDir, f.Spec.ID)
	if err != nil {
		t.Fatal(err)
	}
	// Firecracker has no ACPI power-off: a halt from inside the guest must still end the instance.
	ssh := exec.Command("ssh", "-i", key, "-o", "StrictHostKeyChecking=no", "-o", "UserKnownHostsFile=/dev/null", "-o", "ConnectTimeout=30", "ubuntu@"+ip, "sudo poweroff")
	if out, err := ssh.CombinedOutput(); err != nil && len(out) > 0 && !strings.Contains(string(out), "closed") {
		t.Logf("ssh: %v: %s", err, out)
	}
	select {
	case <-inst.Done():
	case <-time.After(60 * time.Second):
		t.Fatal("instance still running after guest poweroff")
	}
}

func TestKillAfterNodeCrash(t *testing.T) {
	if os.Getenv("VMM_FC_HELPER") == "1" {
		p := testProvider(t)
		inst := startVM(t, p, testVM("fccrash"))
		// The direct child is the PID namespace's shell; its only child is the jailer that becomes Firecracker.
		sh := strconv.Itoa(inst.(*instance).Cmd.Process.Pid)
		var kid string
		for deadline := time.Now().Add(5 * time.Second); kid == "" && time.Now().Before(deadline); time.Sleep(50 * time.Millisecond) {
			b, _ := os.ReadFile("/proc/" + sh + "/task/" + sh + "/children")
			kid = strings.TrimSpace(string(b))
		}
		if kid == "" {
			t.Fatal("no jailer under the VM's shell")
		}
		fmt.Println("PID", kid)
		time.Sleep(time.Hour)
		return
	}
	testProvider(t)
	cmd := exec.Command(os.Args[0], "-test.run", "^TestKillAfterNodeCrash$", "-test.v")
	cmd.Env = append(os.Environ(), "VMM_FC_HELPER=1")
	out, _ := cmd.StdoutPipe()
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	sc := bufio.NewScanner(out)
	pid := 0
	for pid == 0 && sc.Scan() {
		fmt.Sscanf(sc.Text(), "PID %d", &pid)
	}
	if pid == 0 {
		cmd.Process.Kill()
		t.Fatal("helper did not start a VM")
	}
	cmd.Process.Kill()
	cmd.Wait()
	deadline := time.Now().Add(5 * time.Second)
	for syscall.Kill(pid, 0) == nil {
		if time.Now().After(deadline) {
			syscall.Kill(pid, syscall.SIGKILL)
			t.Fatalf("firecracker %s survived its node", strconv.Itoa(pid))
		}
		time.Sleep(100 * time.Millisecond)
	}
}

func TestStartRunsJailed(t *testing.T) {
	p := testProvider(t)
	f := testVM("fcjailed")
	inst := startVM(t, p, f)
	defer inst.Kill()
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	if _, err := inst.WaitIP(ctx); err != nil {
		t.Fatalf("WaitIP: %v", err)
	}
	ip, err := linuxvm.AllocNATIP(p.VMsDir, f.Spec.ID)
	if err != nil {
		t.Fatal(err)
	}
	uid, err := jailUID(ip)
	if err != nil {
		t.Fatal(err)
	}
	// Firecracker creates the API socket itself, so its owner is the UID the VMM runs as.
	st, err := os.Stat(inst.(*instance).sock)
	if err != nil {
		t.Fatal(err)
	}
	if got := int(st.Sys().(*syscall.Stat_t).Uid); got != uid {
		t.Fatalf("socket owner uid %d, want %d", got, uid)
	}
}

func TestRestartVM(t *testing.T) {
	p := testProvider(t)
	f := testVM("fcrestart")
	inst := startVM(t, p, f)
	inst.Kill()
	<-inst.Done()
	// A stopped VM must not leave device nodes owned by its UID behind.
	if _, err := os.Stat(jailBase(node.VMDir(p.VMsDir, f.Spec.ID))); !os.IsNotExist(err) {
		t.Fatalf("jail still exists after the VM stopped: %v", err)
	}
	inst, err := p.Start(f, node.VMDir(p.VMsDir, f.Spec.ID))
	if err != nil {
		t.Fatal(err)
	}
	defer inst.Kill()
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	if _, err := inst.WaitIP(ctx); err != nil {
		t.Fatalf("WaitIP after restart: %v", err)
	}
}

// A deep paths.vms pushes the API socket beyond the 107-byte unix socket limit; the node must still reach it.
func TestCtrlAltDelWithLongSocketPath(t *testing.T) {
	p := testProvider(t)
	inst := startVM(t, p, testVM("fclong"+strings.Repeat("x", 44)))
	defer inst.Kill()
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	if _, err := inst.WaitIP(ctx); err != nil {
		t.Fatalf("WaitIP: %v", err)
	}
	if err := inst.(*instance).api.CtrlAltDel(); err != nil {
		t.Fatal(err)
	}
}

// TestSnapshotRestore checks the control API against a real Firecracker: pause, full snapshot, kill, restore in a fresh jail.
func TestSnapshotRestore(t *testing.T) {
	p := testProvider(t)
	key := filepath.Join(t.TempDir(), "id")
	if out, err := exec.Command("ssh-keygen", "-q", "-t", "ed25519", "-N", "", "-f", key).CombinedOutput(); err != nil {
		t.Fatalf("%v: %s", err, out)
	}
	pub, _ := os.ReadFile(key + ".pub")
	f := testVM("fcsnap")
	f.Spec.MemoryMB = 512
	f.Boot = &proto.Boot{SSHPublicKeys: []string{string(pub)}}
	inst := startVM(t, p, f)
	defer inst.Kill()
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	ip, err := inst.WaitIP(ctx)
	if err != nil {
		t.Fatal(err)
	}
	ssh := func(cmd string) string {
		t.Helper()
		out, err := exec.Command("ssh", "-i", key, "-o", "StrictHostKeyChecking=no", "-o", "UserKnownHostsFile=/dev/null", "-o", "ConnectTimeout=30", "-o", "LogLevel=ERROR", "ubuntu@"+ip, cmd).CombinedOutput()
		if err != nil {
			t.Fatalf("ssh %q: %v: %s", cmd, err, out)
		}
		return string(out)
	}
	t.Logf("guest devices: %s", ssh("ls -l /dev/vsock; ls -d /sys/bus/virtio/drivers/virtio_balloon/virtio*; echo before-snapshot > /dev/shm/marker"))
	api := inst.(*instance).api
	var stats balloonStats
	for deadline := time.Now().Add(30 * time.Second); stats.TotalMemory == 0 && time.Now().Before(deadline); time.Sleep(time.Second) {
		if stats, err = api.BalloonStats(); err != nil {
			t.Fatal(err)
		}
	}
	t.Logf("balloon stats: %+v", stats)
	if stats.TotalMemory == 0 || stats.AvailableMemory == 0 {
		t.Fatalf("no balloon statistics from the guest: %+v", stats)
	}
	if err := api.Pause(); err != nil {
		t.Fatal(err)
	}
	if err := api.CreateSnapshot("/vmstate", "/mem"); err != nil {
		t.Fatal(err)
	}
	dir := node.VMDir(p.VMsDir, f.Spec.ID)
	state := filepath.Join(dir, "state")
	if err := os.MkdirAll(state, 0o700); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"vmstate", "mem"} {
		if err := os.Link(filepath.Join(jailRoot(dir), name), filepath.Join(state, name)); err != nil {
			t.Fatal(err)
		}
	}
	inst.Kill()
	<-inst.Done()

	kernel, initrd, err := bootFiles(filepath.Join(dir, "root.img"), dir)
	if err != nil {
		t.Fatal(err)
	}
	uid, err := jailUID(ip)
	if err != nil {
		t.Fatal(err)
	}
	files := []jailFile{{path: kernel, readOnly: true}, {path: initrd, readOnly: true}, {path: filepath.Join(dir, "root.img")},
		{path: filepath.Join(dir, node.SeedISO), readOnly: true}, {path: filepath.Join(state, "vmstate")}, {path: filepath.Join(state, "mem")}}
	if _, err := buildJail(dir, uid, files); err != nil {
		t.Fatal(err)
	}
	fcPath, err := exec.LookPath("firecracker")
	if err != nil {
		t.Fatal(err)
	}
	proc, err := linuxvm.Spawn(f, dir, jailerCmd(fcPath, dir, uid, false), nil, uid, func() { os.RemoveAll(jailBase(dir)) })
	if err != nil {
		t.Fatal(err)
	}
	defer proc.Kill()
	sock := filepath.Join(jailRoot(dir), "fc.sock")
	for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(50 * time.Millisecond) {
		if _, err := os.Stat(sock); err == nil {
			break
		} else if time.Now().After(deadline) {
			t.Fatal(err)
		}
	}
	sockDir, err := os.Open(jailRoot(dir))
	if err != nil {
		t.Fatal(err)
	}
	defer sockDir.Close()
	api = apiAt(fmt.Sprintf("/proc/self/fd/%d/fc.sock", sockDir.Fd()))
	start := time.Now()
	if err := api.LoadSnapshot("/vmstate", "/mem", []netOverride{{IfaceID: "eth0", HostDevName: linuxvm.TapName(f.Spec.ID, 0)}}); err != nil {
		t.Fatal(err)
	}
	if err := api.Resume(); err != nil {
		t.Fatal(err)
	}
	t.Logf("restored and resumed in %v", time.Since(start))
	// tmpfs content survives only if the guest memory was restored rather than rebooted.
	if got := strings.TrimSpace(ssh("cat /dev/shm/marker")); got != "before-snapshot" {
		t.Fatalf("marker after restore = %q", got)
	}
	if stats, err := api.BalloonStats(); err != nil {
		t.Fatal(err)
	} else {
		t.Logf("balloon stats after restore: %+v", stats)
	}
}

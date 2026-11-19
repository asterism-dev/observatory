package firecracker

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/url"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/asterism-dev/observatory/internal/node"
	"github.com/asterism-dev/observatory/internal/proto"
	"github.com/asterism-dev/observatory/internal/provider/linuxvm"
	"github.com/asterism-dev/observatory/internal/version"
)

// testProvider needs root, KVM, bridge vmm0, VMM_TEST_IMAGE (ociimage's ext4 disk of an image with sudo) and VMM_TEST_GUESTBOOT (unpacked guestboot.tar).
// testProvider has fast boot off: a background template build in one test's VMs dir would take NAT addresses another test's VMs use.
func testProvider(t *testing.T) *Provider {
	t.Helper()
	p := testProviderWith(t, "")
	p.fastBoot = false
	return p
}

// testProviderWith adds settings (JSON object members) to the firecracker config section.
func testProviderWith(t *testing.T, settings string) *Provider {
	t.Helper()
	img, gb := os.Getenv("VMM_TEST_IMAGE"), os.Getenv("VMM_TEST_GUESTBOOT")
	if img == "" || gb == "" || os.Geteuid() != 0 {
		t.Skip("needs root, VMM_TEST_IMAGE and VMM_TEST_GUESTBOOT")
	}
	base := t.TempDir()
	// The node refuses a guestboot of another version, and test binaries are "dev".
	boot := filepath.Join(base, "guestboot")
	if err := os.Mkdir(boot, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, n := range []string{"vmlinux", "observatory-init", "mke2fs", "resize2fs"} {
		if err := os.Symlink(filepath.Join(gb, n), filepath.Join(boot, n)); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(boot, "VERSION"), []byte(version.Version+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	old := guestbootDir
	guestbootDir = boot
	t.Cleanup(func() { guestbootDir = old })
	images := filepath.Join(base, "images")
	disks := filepath.Join(images, "oci", "disks")
	if err := os.MkdirAll(disks, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(img, filepath.Join(disks, strings.TrimPrefix(testDigest, "sha256:")+".raw")); err != nil {
		t.Fatal(err)
	}
	p, err := New(node.Config{Provider: "firecracker", Paths: struct {
		VMs    string `json:"vms"`
		Images string `json:"images"`
	}{VMs: filepath.Join(base, "vms"), Images: images}, Raw: map[string]json.RawMessage{"firecracker": json.RawMessage(`{"guestImage":"test"` + settings + `}`)}})
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func testVM(id string) node.VMFile {
	return node.VMFile{MAC: "06:00:00:00:00:01", Image: "test@" + testDigest, Spec: proto.VMSpec{ID: id, Name: id, CPUs: 1, MemoryMB: 1024, DiskGB: 10}}
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
	inst, err := p.Start(context.Background(), f, dir)
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
	seed := func() proto.GuestSeed {
		t.Helper()
		b, err := node.ReadSeedFile(dir, proto.GuestSeedFile)
		if err != nil {
			t.Fatal(err)
		}
		var s proto.GuestSeed
		if err := json.Unmarshal(b, &s); err != nil {
			t.Fatal(err)
		}
		return s
	}
	f.Boot = &proto.Boot{SSHPublicKeys: []string{"ssh-ed25519 AAAA one", "ssh-rsa BBBB two"}, Env: map[string]string{"A": "x\ny", "B": `it's $HOME \ "q"`}}
	if err := p.Reseed(context.Background(), f, dir); err != nil {
		t.Fatal(err)
	}
	first := seed()

	f.Boot = nil
	f.Spec.NICs = []proto.NIC{{Network: "mgmt", MAC: "02:0a:c8:00:00:0a"}}
	if err := p.Reseed(context.Background(), f, dir); err != nil {
		t.Fatal(err)
	}
	got := seed()
	if !reflect.DeepEqual(got.Env, first.Env) || !slices.Equal(got.SSHKeys, first.SSHKeys) {
		t.Fatalf("after nil-Boot reseed: env %v keys %q, want %v %q", got.Env, got.SSHKeys, first.Env, first.SSHKeys)
	}
	if len(got.NICs) != 2 || got.NICs[1].MAC != "02:0a:c8:00:00:0a" {
		t.Fatalf("new NIC missing: %+v", got.NICs)
	}

	f.Boot = &proto.Boot{Env: map[string]string{"A": "two"}}
	if err := p.Reseed(context.Background(), f, dir); err != nil {
		t.Fatal(err)
	}
	if got := seed(); !reflect.DeepEqual(got.Env, map[string]string{"A": "two"}) || len(got.SSHKeys) != 0 {
		t.Fatalf("after new Boot: env %v keys %q", got.Env, got.SSHKeys)
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
	ssh := exec.Command("ssh", "-i", key, "-o", "StrictHostKeyChecking=no", "-o", "UserKnownHostsFile=/dev/null", "-o", "ConnectTimeout=30", "dev@"+ip, "sudo poweroff")
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
	// The killed helper never removes its own temp dirs, which hold a disk clone.
	cmd.Env = append(os.Environ(), "VMM_FC_HELPER=1", "TMPDIR="+t.TempDir())
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
	inst, err := p.Start(context.Background(), f, node.VMDir(p.VMsDir, f.Spec.ID))
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
		out, err := exec.Command("ssh", "-i", key, "-o", "StrictHostKeyChecking=no", "-o", "UserKnownHostsFile=/dev/null", "-o", "ConnectTimeout=30", "-o", "LogLevel=ERROR", "dev@"+ip, cmd).CombinedOutput()
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

	uid, err := jailUID(ip)
	if err != nil {
		t.Fatal(err)
	}
	files := []jailFile{{path: p.boot.kernel, shared: true}, {path: p.boot.initrd, shared: true}, {path: filepath.Join(dir, "root.img")},
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
	js, err := openJailSock(sock)
	if err != nil {
		t.Fatal(err)
	}
	defer js.close()
	api = fcAPI{dial: js.dial}
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

// A stopped instance's API must not reach the VM that reuses its socket directory's fd number.
func TestStoppedInstanceAPIDoesNotReachOtherVM(t *testing.T) {
	p := testProvider(t)
	a := startVM(t, p, testVM("fcstalea"))
	a.Kill()
	<-a.Done()
	b := startVM(t, p, testVM("fcstaleb"))
	defer b.Kill()
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	if _, err := b.WaitIP(ctx); err != nil {
		t.Fatal(err)
	}
	if err := a.(*instance).api.CtrlAltDel(); err == nil {
		t.Fatal("CtrlAltDel on a stopped instance succeeded")
	}
	select {
	case <-b.Done():
		t.Fatal("the other VM stopped")
	case <-time.After(10 * time.Second):
	}
}

// hibernate saves inst's state into <dir>/state as the node agent does and kills it.
func hibernate(t *testing.T, inst node.Instance, dir string) string {
	t.Helper()
	tmp, state := filepath.Join(dir, "state.tmp"), filepath.Join(dir, node.StateDir)
	if err := inst.SaveState(tmp); err != nil {
		inst.Kill()
		t.Fatal(err)
	}
	if err := os.Rename(tmp, state); err != nil {
		t.Fatal(err)
	}
	inst.Kill()
	<-inst.Done()
	for _, n := range []string{node.StateFile, node.MemFile} {
		st, err := os.Stat(filepath.Join(state, n))
		if err != nil {
			t.Fatal(err)
		}
		if st.Mode().Perm() != 0o600 {
			t.Errorf("%s mode = %v, want 0600", n, st.Mode().Perm())
		}
	}
	return state
}

func sshVM(t *testing.T, key, ip, cmd string) string {
	t.Helper()
	var out []byte
	var err error
	// The SSH server may answer before the init has the seed's keys.
	for range 10 {
		if out, err = exec.Command("ssh", "-i", key, "-o", "StrictHostKeyChecking=no", "-o", "UserKnownHostsFile=/dev/null", "-o", "ConnectTimeout=30", "-o", "LogLevel=ERROR", "dev@"+ip, cmd).CombinedOutput(); err == nil || !bytes.Contains(out, []byte("Permission denied")) {
			break
		}
		time.Sleep(3 * time.Second)
	}
	if err != nil {
		t.Fatalf("ssh %q: %v: %s", cmd, err, out)
	}
	return strings.TrimSpace(string(out))
}

func testHibernateResume(t *testing.T, f node.VMFile, logFile string) {
	p := testProvider(t)
	key := filepath.Join(t.TempDir(), "id")
	if out, err := exec.Command("ssh-keygen", "-q", "-t", "ed25519", "-N", "", "-f", key).CombinedOutput(); err != nil {
		t.Fatalf("%v: %s", err, out)
	}
	pub, _ := os.ReadFile(key + ".pub")
	f.Spec.MemoryMB = 512
	f.Boot = &proto.Boot{SSHPublicKeys: []string{string(pub)}}
	inst := startVM(t, p, f)
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	ip, err := inst.WaitIP(ctx)
	if err != nil {
		inst.Kill()
		t.Fatal(err)
	}
	dir := node.VMDir(p.VMsDir, f.Spec.ID)
	// The resume call needs the agent, which answers once the guest is set up.
	if err := waitAgent(ctx, dir, inst); err != nil {
		inst.Kill()
		t.Fatal(err)
	}
	pid := sshVM(t, key, ip, "sudo sh -c 'nohup sh -c \"while :; do date +%s >> "+logFile+"; sleep 1; done\" >/dev/null 2>&1 & echo $!'")
	time.Sleep(3 * time.Second)
	state := hibernate(t, inst, dir)
	hibernatedAt := time.Now()
	time.Sleep(30 * time.Second)
	inst, err = p.Restore(context.Background(), f, dir, node.RestoreFrom{StateDir: state})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { inst.Kill(); <-inst.Done() }()
	t.Logf("restored %s after hibernating for %s", f.Spec.ID, time.Since(hibernatedAt).Round(time.Second))
	if _, err := os.Stat(state); !os.IsNotExist(err) {
		t.Errorf("state/ left after restore: %v", err)
	}
	if got, err := inst.WaitIP(ctx); err != nil || got != ip {
		t.Fatalf("WaitIP = %q, %v", got, err)
	}
	if out := sshVM(t, key, ip, "sudo kill -0 "+pid+" && echo alive"); out != "alive" {
		t.Fatalf("loop %s: %q", pid, out)
	}
	guest, err := strconv.ParseInt(sshVM(t, key, ip, "date +%s"), 10, 64)
	if err != nil {
		t.Fatal(err)
	}
	if d := time.Now().Unix() - guest; d > 2 || d < -2 {
		t.Fatalf("guest clock off by %ds", d)
	}
	if len(f.Spec.Attachments) > 0 {
		if src := sshVM(t, key, ip, "findmnt -no SOURCE /data"); src != "/dev/vdb" {
			t.Fatalf("/data is on %q, want the NBD disk /dev/vdb", src)
		}
	}
	time.Sleep(2 * time.Second)
	tail := sshVM(t, key, ip, "tail -n 4 "+logFile)
	t.Logf("loop output after resume: %s", strings.ReplaceAll(tail, "\n", " "))
	if lines := strings.Fields(tail); len(lines) < 2 || lines[len(lines)-1] <= lines[0] {
		t.Fatalf("loop not running after resume: %q", tail)
	}
}

func TestHibernateResume(t *testing.T) {
	testHibernateResume(t, testVM("fchib"), "/tmp/t")
}

// The loop writes to the attached disk, so its NBD device is busy across the hibernation.
func TestHibernateResumeWithNBDDisk(t *testing.T) {
	url, psk := os.Getenv("VMM_TEST_NBD"), os.Getenv("VMM_TEST_NBD_PSK")
	if url == "" {
		t.Skip("needs VMM_TEST_NBD and VMM_TEST_NBD_PSK")
	}
	f := testVM("fchibnbd")
	f.Spec.Attachments = []proto.DiskAttachment{{DiskID: path.Base(url), MountPath: "/data", URL: url, PSK: psk}}
	testHibernateResume(t, f, "/data/t")
}

func TestRestoreCorruptStateFails(t *testing.T) {
	p := testProvider(t)
	f := testVM("fccorrupt")
	f.Spec.MemoryMB = 512
	inst := startVM(t, p, f)
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	if _, err := inst.WaitIP(ctx); err != nil {
		inst.Kill()
		t.Fatal(err)
	}
	dir := node.VMDir(p.VMsDir, f.Spec.ID)
	state := hibernate(t, inst, dir)
	if err := os.WriteFile(filepath.Join(state, node.StateFile), []byte("garbage"), 0o600); err != nil {
		t.Fatal(err)
	}
	if inst, err := p.Restore(context.Background(), f, dir, node.RestoreFrom{StateDir: state}); err == nil {
		inst.Kill()
		t.Fatal("restore of a corrupt state succeeded")
	} else {
		t.Logf("restore error: %v", err)
	}
	if _, err := net.InterfaceByName(linuxvm.TapName(f.Spec.ID, 0)); err == nil {
		t.Error("tap left after a failed restore")
	}
	if _, err := os.Stat(jailBase(dir)); !os.IsNotExist(err) {
		t.Errorf("jail left after a failed restore: %v", err)
	}
	inst, err := p.Start(context.Background(), f, dir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { inst.Kill(); <-inst.Done() }()
	ctx, cancel = context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	if _, err := inst.WaitIP(ctx); err != nil {
		b, _ := os.ReadFile(filepath.Join(dir, node.ConsoleLog))
		t.Fatalf("cold boot after a failed restore: %v\n%s", err, b[max(0, len(b)-3000):])
	}
}

// blockStorage rejects the node's connections to the NBD server until the returned func runs.
func blockStorage(t *testing.T, rawURL string) func() {
	t.Helper()
	u, err := url.Parse(rawURL)
	if err != nil {
		t.Fatal(err)
	}
	rule := fmt.Sprintf("add table inet t3block\nadd chain inet t3block out { type filter hook output priority 0; }\nadd rule inet t3block out ip daddr %s tcp dport %s reject with tcp reset\n", u.Hostname(), u.Port())
	cmd := exec.Command("nft", "-f", "-")
	cmd.Stdin = strings.NewReader(rule)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("nft: %v: %s", err, out)
	}
	unblock := func() { exec.Command("nft", "delete", "table", "inet", "t3block").Run() }
	t.Cleanup(unblock)
	return unblock
}

// A hibernated VM with a disk keeps its state while storage is down; once storage is back it resumes where it was, and its shutdown writes the guest's cached data.
func TestHibernatedWithDiskWaitsForStorage(t *testing.T) {
	nbdURL, psk := os.Getenv("VMM_TEST_NBD"), os.Getenv("VMM_TEST_NBD_PSK")
	if nbdURL == "" {
		t.Skip("needs VMM_TEST_NBD and VMM_TEST_NBD_PSK")
	}
	p := testProvider(t)
	key := filepath.Join(t.TempDir(), "id")
	if out, err := exec.Command("ssh-keygen", "-q", "-t", "ed25519", "-N", "", "-f", key).CombinedOutput(); err != nil {
		t.Fatalf("%v: %s", err, out)
	}
	pub, _ := os.ReadFile(key + ".pub")
	f := testVM("fchibstop")
	f.Spec.MemoryMB = 512
	f.Boot = &proto.Boot{SSHPublicKeys: []string{string(pub)}}
	f.Spec.Attachments = []proto.DiskAttachment{{DiskID: path.Base(nbdURL), MountPath: "/data", URL: nbdURL, PSK: psk}}
	inst := startVM(t, p, f)
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	ip, err := inst.WaitIP(ctx)
	if err != nil {
		inst.Kill()
		t.Fatal(err)
	}
	marker := fmt.Sprintf("unsynced-%d", time.Now().UnixNano())
	sshVM(t, key, ip, "echo "+marker+" | sudo tee /data/marker /dev/shm/marker >/dev/null")
	dir := node.VMDir(p.VMsDir, f.Spec.ID)
	state := hibernate(t, inst, dir)

	unblock := blockStorage(t, nbdURL)
	u, _ := url.Parse(nbdURL)
	if c, err := net.DialTimeout("tcp", u.Host, 3*time.Second); err == nil {
		c.Close()
		t.Fatal("storage still reachable while blocked")
	}
	if inst, err := p.Restore(context.Background(), f, dir, node.RestoreFrom{StateDir: state}); err == nil {
		inst.Kill()
		t.Fatal("restored while storage is unreachable")
	} else {
		t.Logf("restore while storage is down: %v", err)
	}
	if _, err := os.Stat(filepath.Join(state, node.MemFile)); err != nil {
		t.Fatalf("state lost while storage is down: %v", err)
	}
	unblock()

	inst, err = p.Restore(context.Background(), f, dir, node.RestoreFrom{StateDir: state})
	if err != nil {
		t.Fatal(err)
	}
	if got := sshVM(t, key, ip, "cat /dev/shm/marker /data/marker"); got != marker+"\n"+marker {
		inst.Kill()
		t.Fatalf("after resume = %q", got)
	}
	began := time.Now()
	if err := inst.Stop(); err != nil {
		t.Fatal(err)
	}
	<-inst.Done()
	t.Logf("resumed VM shut down in %s", time.Since(began).Round(100*time.Millisecond))
	if time.Since(began) >= 10*time.Second {
		t.Fatal("guest did not shut down on its own")
	}
	if inst, err = p.Start(context.Background(), f, dir); err != nil {
		t.Fatal(err)
	}
	defer func() { inst.Kill(); <-inst.Done() }()
	ctx, cancel = context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	if _, err := inst.WaitIP(ctx); err != nil {
		t.Fatal(err)
	}
	if got := sshVM(t, key, ip, "cat /data/marker"); got != marker {
		t.Fatalf("marker after shutdown = %q, want %q", got, marker)
	}
}

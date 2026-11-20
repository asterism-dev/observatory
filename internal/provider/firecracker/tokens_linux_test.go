package firecracker

import (
	"context"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/asterism-dev/observatory/internal/node"
	"github.com/asterism-dev/observatory/internal/proto"
	"github.com/asterism-dev/observatory/internal/provider/linuxvm"
)

const dummyToken = "obsph_dummy0000000000"

func githubLookup(vmID, kind string) (proto.IntegrationToken, string) {
	if kind == "github" {
		return proto.IntegrationToken{Kind: kind, Token: dummyToken}, ""
	}
	return proto.IntegrationToken{}, node.TokenNotConnected
}

const tokenSockName = "v.sock_1051"

// askAs connects as uid through the jail root's fd, which Firecracker reaches through its chroot instead.
func askAs(t *testing.T, root string, uid int) (string, error) {
	t.Helper()
	d, err := os.Open(root)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	cmd := exec.Command("python3", "-c", `import socket
s=socket.socket(socket.AF_UNIX)
s.connect("/proc/self/fd/3/`+tokenSockName+`")
s.sendall(b'{"op":"token","kind":"github"}\n')
print(s.recv(4096).decode().strip())`)
	cmd.ExtraFiles = []*os.File{d}
	cmd.SysProcAttr = &syscall.SysProcAttr{Credential: &syscall.Credential{Uid: uint32(uid), Gid: uint32(uid)}}
	out, err := cmd.CombinedOutput()
	return strings.TrimSpace(string(out)), err
}

func serve(t *testing.T, l net.Listener) {
	done := make(chan struct{})
	go func() { node.ServeTokens(l, "x", githubLookup); close(done) }()
	t.Cleanup(func() { l.Close(); <-done })
}

func TestTokenSocketOwnerAndMode(t *testing.T) {
	dir := jailTestDir(t)
	if _, err := buildJail(dir, 70007, nil); err != nil {
		t.Fatal(err)
	}
	l, err := listenGuestPort(dir, 70007, node.TokenPort)
	if err != nil {
		t.Fatal(err)
	}
	serve(t, l)
	st, err := os.Lstat(filepath.Join(jailRoot(dir), tokenSockName))
	if err != nil {
		t.Fatal(err)
	}
	if sys := st.Sys().(*syscall.Stat_t); sys.Uid != 70007 || sys.Gid != 70007 || st.Mode().String() != "Srw-------" {
		t.Fatalf("socket uid %d gid %d mode %s", sys.Uid, sys.Gid, st.Mode())
	}
	if out, err := askAs(t, jailRoot(dir), 70007); err != nil || !strings.Contains(out, `"token":"`+dummyToken+`"`) {
		t.Fatalf("as the jail uid: %v: %s", err, out)
	}
	if out, err := askAs(t, jailRoot(dir), 70008); err == nil || !strings.Contains(out, "Permission denied") {
		t.Fatalf("as another uid: %v: %s", err, out)
	}
}

func TestFirecrackerListensTelemetryPort(t *testing.T) {
	dir := jailTestDir(t)
	if _, err := buildJail(dir, 70007, nil); err != nil {
		t.Fatal(err)
	}
	l, err := listenGuestPort(dir, 70007, node.TelemetryPort)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	st, err := os.Lstat(filepath.Join(jailRoot(dir), "v.sock_1052"))
	if err != nil {
		t.Fatal(err)
	}
	if sys := st.Sys().(*syscall.Stat_t); sys.Uid != 70007 || sys.Gid != 70007 || st.Mode().String() != "Srw-------" {
		t.Fatalf("socket uid %d gid %d mode %s", sys.Uid, sys.Gid, st.Mode())
	}
}

func TestTokenSocketLongPath(t *testing.T) {
	dir := filepath.Join(jailTestDir(t), strings.Repeat("d", 60), "vm")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := buildJail(dir, 70007, nil); err != nil {
		t.Fatal(err)
	}
	sock := filepath.Join(jailRoot(dir), tokenSockName)
	if len(sock) <= 107 {
		t.Fatalf("socket path only %d bytes", len(sock))
	}
	l, err := listenGuestPort(dir, 70007, node.TokenPort)
	if err != nil {
		t.Fatal(err)
	}
	serve(t, l)
	// The fd number listenGuestPort used is free again; a decoy directory takes it.
	decoyDir := t.TempDir()
	decoy := filepath.Join(decoyDir, tokenSockName)
	if err := os.WriteFile(decoy, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	d, err := os.Open(decoyDir)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	if out, err := askAs(t, jailRoot(dir), 70007); err != nil || !strings.Contains(out, dummyToken) {
		t.Fatalf("%v: %s", err, out)
	}
	l.Close()
	if _, err := os.Lstat(sock); err != nil {
		t.Errorf("socket in the jail after Close: %v", err)
	}
	if _, err := os.Lstat(decoy); err != nil {
		t.Errorf("file outside the jail removed by Close: %v", err)
	}
}

// serveTokensRunning waits up to 2s for every token service goroutine to end.
func serveTokensRunning() bool {
	buf := make([]byte, 1<<20)
	for deadline := time.Now().Add(2 * time.Second); time.Now().Before(deadline); time.Sleep(50 * time.Millisecond) {
		if !strings.Contains(string(buf[:runtime.Stack(buf, true)]), "node.ServeTokens(") {
			return false
		}
	}
	return true
}

// askGuest fetches the github token from inside the guest over vsock to host CID 2.
func askGuest(t *testing.T, key, ip string) string {
	t.Helper()
	return sshVM(t, key, ip, fmt.Sprintf(`python3 -c 'import socket;s=socket.socket(socket.AF_VSOCK);s.connect((2,%d));s.sendall(b"{\"op\":\"token\",\"kind\":\"github\"}\n");print(s.recv(4096).decode().strip())'`, node.TokenPort))
}

// reserveNodeNATIPs keeps the test VM off the addresses of a local node's VMs on the shared bridge.
func reserveNodeNATIPs(t *testing.T, p *Provider) {
	t.Helper()
	used, _ := filepath.Glob("/var/lib/observatory-node/vms/*/nat-ip")
	for i, f := range used {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		dir := filepath.Join(p.VMsDir, fmt.Sprintf("reserved%d", i))
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "nat-ip"), b, 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func TestStartVMTokenService(t *testing.T) {
	p := testProvider(t)
	reserveNodeNATIPs(t, p)
	p.SetTokenLookup(githubLookup)
	key := filepath.Join(t.TempDir(), "id")
	if out, err := exec.Command("ssh-keygen", "-q", "-t", "ed25519", "-N", "", "-f", key).CombinedOutput(); err != nil {
		t.Fatalf("%v: %s", err, out)
	}
	pub, _ := os.ReadFile(key + ".pub")
	f := testVM("fctokens")
	f.Spec.MemoryMB = 512
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
	want := `{"ok":true,"token":"` + dummyToken + `","expiresAt":null}`
	if got := askGuest(t, key, ip); got != want {
		t.Fatalf("booted guest got %s", got)
	}
	dir := node.VMDir(p.VMsDir, f.Spec.ID)
	if err := waitAgent(ctx, dir, inst); err != nil {
		t.Fatal(err)
	}
	state := hibernate(t, inst, dir)
	if serveTokensRunning() {
		t.Fatal("token service still running after the VM was killed")
	}
	inst, err = p.Restore(context.Background(), f, dir, node.RestoreFrom{StateDir: state})
	if err != nil {
		t.Fatal(err)
	}
	defer inst.Kill()
	if got := askGuest(t, key, ip); got != want {
		t.Fatalf("restored guest got %s", got)
	}
	p.SetTokenLookup(nil)
	if got := askGuest(t, key, ip); got != `{"ok":false,"error":"unavailable"}` {
		t.Fatalf("without lookup the guest got %s", got)
	}

	if err := inst.Stop(); err != nil {
		t.Fatal(err)
	}
	<-inst.Done()
	if serveTokensRunning() {
		t.Fatal("token service still running after stop")
	}
	if _, err := os.Stat(jailBase(dir)); !os.IsNotExist(err) {
		t.Fatalf("jail left after stop: %v", err)
	}
}

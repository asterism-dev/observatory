package linuxvm

import (
	"bytes"
	"fmt"
	"net"
	"net/netip"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"github.com/asterism-dev/observatory/internal/egress"
	"github.com/asterism-dev/observatory/internal/node"
	"github.com/asterism-dev/observatory/internal/proto"
)

func needEgressHost(t *testing.T) {
	t.Helper()
	if os.Geteuid() != 0 {
		t.Skip("needs root")
	}
	if _, err := exec.LookPath("nft"); err != nil {
		t.Skip("needs nft")
	}
	if _, err := net.InterfaceByName(NATBridge); err != nil {
		t.Skip("needs bridge " + NATBridge)
	}
}

func egressVM(t *testing.T, vms, id, ip string) node.VMFile {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(vms, id), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(vms, id, natFile), []byte(ip+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return node.VMFile{Spec: proto.VMSpec{ID: id}}
}

func run(t *testing.T, name string, args ...string) {
	t.Helper()
	if out, err := exec.Command(name, args...).CombinedOutput(); err != nil {
		t.Fatalf("%s %v: %v: %s", name, args, err, out)
	}
}

// fakeVM is a netns attached to vmm0 through a veth named like the VM's NAT tap.
func fakeVM(t *testing.T, id, ip string) string {
	t.Helper()
	ns, tap := "egt-"+id, TapName(id, 0)
	exec.Command("ip", "netns", "del", ns).Run()
	exec.Command("ip", "link", "del", tap).Run()
	run(t, "ip", "netns", "add", ns)
	t.Cleanup(func() {
		exec.Command("ip", "link", "del", tap).Run()
		exec.Command("ip", "netns", "del", ns).Run()
	})
	run(t, "ip", "link", "add", tap, "type", "veth", "peer", "name", "eth0", "netns", ns)
	run(t, "ip", "link", "set", tap, "master", NATBridge, "up")
	run(t, "ip", "-n", ns, "addr", "add", ip+"/24", "dev", "eth0")
	run(t, "ip", "-n", ns, "link", "set", "eth0", "up")
	run(t, "ip", "-n", ns, "link", "set", "lo", "up")
	run(t, "ip", "-n", ns, "route", "add", "default", "via", NATGateway)
	return ns
}

// dialFrom connects from inside the netns; the socket is created on a thread that never leaves it.
func dialFrom(ns, addr string) error {
	errc := make(chan error, 1)
	go func() {
		runtime.LockOSThread() // never unlocked: the thread exits with the goroutine
		f, err := os.Open("/run/netns/" + ns)
		if err != nil {
			errc <- err
			return
		}
		defer f.Close()
		if err := unix.Setns(int(f.Fd()), unix.CLONE_NEWNET); err != nil {
			errc <- err
			return
		}
		c, err := net.DialTimeout("tcp4", addr, 2*time.Second)
		if err == nil {
			time.Sleep(200 * time.Millisecond)
			c.Close()
		}
		errc <- err
	}()
	return <-errc
}

func fakeProxy(t *testing.T, port int) <-chan netip.AddrPort {
	t.Helper()
	ln, err := net.Listen("tcp4", fmt.Sprintf("%s:%d", NATGateway, port))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	got := make(chan netip.AddrPort, 8)
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			dst, err := egress.OriginalDst(c)
			if err != nil {
				t.Errorf("OriginalDst: %v", err)
			}
			got <- dst
			c.Close()
		}
	}()
	return got
}

func TestEgressNetnsIsolation(t *testing.T) {
	needEgressHost(t)
	vms := t.TempDir()
	e, err := NewEgress(vms, "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { NewEgress(vms, "") })
	if err := nftApply(egressRules([]netip.Addr{netip.MustParseAddr("127.0.0.53")})); err != nil {
		t.Fatal(err)
	}
	a, b := egressVM(t, vms, "egta", "192.168.64.2"), egressVM(t, vms, "egtb", "192.168.64.3")
	nsA := fakeVM(t, "egta", "192.168.64.2")
	fakeVM(t, "egtb", "192.168.64.3")
	proxyA, proxyB := fakeProxy(t, 15002), fakeProxy(t, 15003)
	for _, f := range []node.VMFile{a, b, a} {
		if err := e.Redirect(f); err != nil {
			t.Fatalf("redirect %s: %v", f.Spec.ID, err)
		}
	}

	if err := dialFrom(nsA, "1.1.1.1:443"); err != nil {
		t.Fatalf("redirected dial: %v", err)
	}
	select {
	case dst := <-proxyA:
		if dst != netip.MustParseAddrPort("1.1.1.1:443") {
			t.Fatalf("OriginalDst = %s", dst)
		}
	case <-proxyB:
		t.Fatal("A's connection reached B's proxy")
	case <-time.After(2 * time.Second):
		t.Fatal("A's connection did not reach its proxy")
	}
	if err := dialFrom(nsA, "192.168.64.1:15003"); err == nil {
		t.Fatal("A reached B's proxy port directly")
	}

	if err := e.Unredirect(a); err != nil {
		t.Fatal(err)
	}
	if err := e.Unredirect(a); err != nil {
		t.Fatalf("second unredirect: %v", err)
	}
	dialFrom(nsA, "1.1.1.1:443")
	select {
	case <-proxyA:
		t.Fatal("A still redirected after Unredirect")
	case <-time.After(time.Second):
	}

	// One of the node's own addresses outside the forbidden prefixes, with a listener that would accept.
	own := ""
	addrs, _ := net.InterfaceAddrs()
	for _, a := range addrs {
		ip, ok := netip.AddrFromSlice(a.(*net.IPNet).IP)
		if ip = ip.Unmap(); ok && ip.Is4() && (egress.Policy{Deny: egress.Forbidden}).Allowed(ip) {
			ln, err := net.Listen("tcp4", ip.String()+":0")
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { ln.Close() })
			own = ln.Addr().String()
			break
		}
	}
	if own == "" {
		t.Fatal("node has no address outside the forbidden prefixes")
	}
	// A local resolver stays reachable on port 53 only.
	for _, port := range []string{"53", "54"} {
		ln, err := net.Listen("tcp4", "127.0.0.53:"+port)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { ln.Close() })
	}
	if out, err := proxyUIDConnect("127.0.0.53:53"); err != nil {
		t.Errorf("uid 71002 -> local resolver: %v: %s", err, out)
	}
	for _, addr := range []string{"10.198.0.1:443", "169.254.169.254:80", "127.0.0.1:22", "127.0.0.53:54", own} {
		if out, err := proxyUIDConnect(addr); err == nil || !strings.Contains(out, "Connection refused") {
			t.Errorf("uid 71002 -> %s: %v: %s", addr, err, out)
		}
	}
}

// onExitingThread runs fn on a thread that is destroyed afterwards; Go never destroys the main thread, so that one is held aside.
func onExitingThread(fn func()) {
	release := make(chan struct{})
	defer close(release)
	for {
		ran := make(chan bool)
		go func() {
			runtime.LockOSThread()
			if unix.Gettid() == os.Getpid() {
				ran <- false
				<-release
				return
			}
			fn()
			ran <- true
		}()
		if <-ran {
			return
		}
	}
}

func proxyUIDConnect(addr string) (string, error) {
	host, port, _ := net.SplitHostPort(addr)
	var stderr bytes.Buffer
	cmd := exec.Command("/bin/bash", "-c", fmt.Sprintf("exec 3<>/dev/tcp/%s/%s", host, port))
	cmd.Stderr = &stderr
	cmd.SysProcAttr = &syscall.SysProcAttr{Credential: &syscall.Credential{Uid: 71002, Gid: 71002}}
	err := cmd.Run()
	return stderr.String(), err
}

// Recreating the tables at node start drops a previous run's elements and leaves other tables alone.
func TestEgressRecreateDropsStale(t *testing.T) {
	needEgressHost(t)
	vms := t.TempDir()
	e, err := NewEgress(vms, "")
	if err != nil {
		t.Fatal(err)
	}
	if err := e.Redirect(egressVM(t, vms, "egtstale", "192.168.64.9")); err != nil {
		t.Fatal(err)
	}
	run(t, "nft", "add table inet egtother; add chain inet egtother c; add set inet egtother s { type ipv4_addr; }; add element inet egtother s { 1.2.3.4 }")
	t.Cleanup(func() { exec.Command("nft", "delete", "table", "inet", "egtother").Run() })
	if _, err := NewEgress(vms, ""); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"list", "map", "bridge", "obs_egress", "marks"}, {"list", "map", "inet", "obs_egress", "ports"}, {"list", "set", "inet", "obs_egress", "allowed"}} {
		out, err := exec.Command("nft", args...).CombinedOutput()
		if err != nil || strings.Contains(string(out), "15009") || strings.Contains(string(out), TapName("egtstale", 0)) || strings.Contains(string(out), "0x004f0009") {
			t.Errorf("%v after reload: %v\n%s", args, err, out)
		}
	}
	out, err := exec.Command("nft", "list", "table", "inet", "egtother").CombinedOutput()
	if err != nil || !strings.Contains(string(out), "1.2.3.4") {
		t.Errorf("other table disturbed: %v\n%s", err, out)
	}
}

func TestEgressSpawn(t *testing.T) {
	needEgressHost(t)
	dir, err := os.MkdirTemp("", "egspawn")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	// The proxy runs unprivileged, so it must be able to execute the binary.
	if err := os.Chmod(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	bin := filepath.Join(dir, "observatory-node")
	build := exec.Command("go", "build", "-o", bin, "github.com/asterism-dev/observatory/cmd/node")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build: %v: %s", err, out)
	}
	e := &Egress{VMsDir: dir, Binary: bin}
	f := egressVM(t, dir, "egtspawn", "192.168.64.2")
	// Spawned from a thread that then exits, as a VM's locked thread does; Pdeathsig must not fire.
	var p node.EgressProc
	onExitingThread(func() { p, err = e.Spawn(f) })
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(p.Kill)
	if err := p.Send(proto.Egress{}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-p.Ready():
	case <-p.Done():
		t.Fatal("exited before ready")
	case <-time.After(10 * time.Second):
		t.Fatal("not ready")
	}
	time.Sleep(500 * time.Millisecond)
	select {
	case <-p.Done():
		t.Fatal("proxy died with the thread that spawned it")
	default:
	}
	pid := p.(*egressProc).cmd.Process.Pid
	if env, err := os.ReadFile(fmt.Sprintf("/proc/%d/environ", pid)); err != nil || len(env) != 0 {
		t.Errorf("environ = %q, %v", env, err)
	}
	if cmdline, _ := os.ReadFile(fmt.Sprintf("/proc/%d/cmdline", pid)); !strings.Contains(string(cmdline), "-peer\x00192.168.64.2") {
		t.Errorf("cmdline = %q", cmdline)
	}
	status, err := os.ReadFile(fmt.Sprintf("/proc/%d/status", pid))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"Uid:\t71002\t71002\t71002\t71002", "Gid:\t71002\t71002\t71002\t71002", "CapEff:\t0000000000000000", "CapPrm:\t0000000000000000"} {
		if !strings.Contains(string(status), want) {
			t.Errorf("status lacks %q:\n%s", want, status)
		}
	}
	p.Kill()
	select {
	case <-p.Done():
	case <-time.After(10 * time.Second):
		t.Fatal("Done not closed after Kill")
	}
}

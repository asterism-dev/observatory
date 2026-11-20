package node

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/asterism-dev/observatory/internal/proto"
)

type testEnv struct {
	hub      *fakeHub
	agent    *Agent
	provider *fakeProvider
	stop     context.CancelFunc
	runDone  chan struct{}
}

func startEnv(t *testing.T, prep func(a *Agent, p *fakeProvider)) *testEnv {
	t.Helper()
	h := newFakeHub(t)
	p := newFakeProvider()
	a := &Agent{HubURL: h.url(), Credential: "tok", NodeID: "n1", VMsDir: t.TempDir(), CPUs: 4, MemoryMB: 8192, Provider: p}
	if prep != nil {
		prep(a, p)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { a.Run(ctx); close(done) }()
	t.Cleanup(func() {
		cancel()
		<-done
	})
	eventually(t, "node online", func() bool { return len(h.registers()) > 0 })
	return &testEnv{hub: h, agent: a, provider: p, stop: cancel, runDone: done}
}

func TestConnectSendsCredential(t *testing.T) {
	env := startEnv(t, nil)
	if got := env.hub.authHeaders(); got[0] != "Bearer tok" {
		t.Fatalf("Authorization = %q", got)
	}
}

func TestAgentRegistersProvider(t *testing.T) {
	env := startEnv(t, func(a *Agent, _ *fakeProvider) {
		a.Config = json.RawMessage(`{"provider":"fake"}`)
		a.Machine = &proto.Machine{OS: "darwin", Arch: "arm64", CPUs: 8}
	})
	n := env.hub.registers()[0]
	if n.Provider != "fake" || string(n.Config) != `{"provider":"fake"}` || n.Machine == nil || n.Machine.CPUs != 8 {
		t.Fatalf("node = %+v", n)
	}
}

func TestAgentRegistersHypervisor(t *testing.T) {
	env := startEnv(t, nil)
	if got := env.hub.registers()[0].Hypervisor; got == nil || *got != (proto.Hypervisor{Type: "firecracker", Version: "v1.18.0"}) {
		t.Fatalf("hypervisor = %+v", got)
	}
}

func TestAgentCreateReachesRunning(t *testing.T) {
	env := startEnv(t, nil)
	vm := env.hub.create("dev")
	eventually(t, "running", func() bool { v, _ := env.hub.vm(vm.ID); return v.State == proto.StateRunning })
	if v, _ := env.hub.vm(vm.ID); v.IP != "127.0.0.1" {
		t.Fatalf("ip = %q", v.IP)
	}
	files, _ := LoadVMs(env.agent.VMsDir)
	if len(files) != 1 || files[0].Spec.ID != vm.ID {
		t.Fatalf("vm.json = %+v", files)
	}
	if mac, err := net.ParseMAC(files[0].MAC); err != nil || mac[0]&0x02 == 0 || mac[0]&0x01 != 0 {
		t.Fatalf("mac %q not locally administered unicast (%v)", files[0].MAC, err)
	}
}

func TestAgentRestartsWhenGuestShutsDown(t *testing.T) {
	fastRestarts(t, 10*time.Millisecond)
	env := startEnv(t, nil)
	vm := env.hub.create("dev")
	eventually(t, "running", func() bool { v, _ := env.hub.vm(vm.ID); return v.State == proto.StateRunning })
	first := env.provider.instance("dev")
	first.guestShutdown()
	eventually(t, "restarted", func() bool {
		v, _ := env.hub.vm(vm.ID)
		return env.provider.instance("dev") != first && v.State == proto.StateRunning
	})
}

func TestAgentRemovedKeyGoneAfterRestart(t *testing.T) {
	fastRestarts(t, 10*time.Millisecond)
	env := startEnv(t, nil)
	vm := env.hub.create("dev")
	eventually(t, "running", func() bool { v, _ := env.hub.vm(vm.ID); return v.State == proto.StateRunning })
	env.hub.setBoot(vm.ID, &proto.Boot{})
	eventually(t, "boot applied", func() bool {
		env.agent.mu.Lock()
		defer env.agent.mu.Unlock()
		b := env.agent.vms[vm.ID].file.Boot
		return b != nil && len(b.SSHPublicKeys) == 0
	})
	first := env.provider.instance("dev")
	first.guestShutdown()
	eventually(t, "restarted", func() bool { return env.provider.instance("dev") != first })
	if b := env.provider.startedBoot("dev"); b == nil || len(b.SSHPublicKeys) != 0 {
		t.Fatalf("restart booted with %+v", b)
	}
}

func TestAgentPrepareFails(t *testing.T) {
	env := startEnv(t, func(_ *Agent, p *fakeProvider) { p.failPrepare["bad"] = true })
	vm := env.hub.create("bad")
	eventually(t, "error", func() bool { v, _ := env.hub.vm(vm.ID); return v.State == proto.StateError })
	if v, _ := env.hub.vm(vm.ID); !strings.Contains(v.Error, "prepare boom") {
		t.Fatalf("error = %q", v.Error)
	}
}

func TestAgentPrepareSaveFails(t *testing.T) {
	env := startEnv(t, func(_ *Agent, p *fakeProvider) {
		p.afterPrepare = func(dir string) {
			os.Chmod(dir, 0o555)
			t.Cleanup(func() { os.Chmod(dir, 0o755) })
		}
	})
	vm := env.hub.create("dev")
	eventually(t, "error", func() bool { v, _ := env.hub.vm(vm.ID); return v.State == proto.StateError })
	if v, _ := env.hub.vm(vm.ID); !strings.HasPrefix(v.Error, "prepare: ") {
		t.Fatalf("error = %q", v.Error)
	}
	if env.provider.instance("dev") != nil {
		t.Fatal("instance started although NeedsPrepare was not saved")
	}
}

func TestAgentWithChangedNodeIDKeepsVMs(t *testing.T) {
	env := startEnv(t, nil)
	vm := env.hub.create("dev")
	eventually(t, "running", func() bool { v, _ := env.hub.vm(vm.ID); return v.State == proto.StateRunning })
	env.stop()
	<-env.runDone

	p := newFakeProvider()
	a := &Agent{HubURL: env.agent.HubURL, Credential: "tok", NodeID: "renamed", VMsDir: env.agent.VMsDir, CPUs: 4, MemoryMB: 8192, Provider: p}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { a.Run(ctx); close(done) }()
	t.Cleanup(func() { cancel(); <-done })
	eventually(t, "booted from cache", func() bool { return p.instance("dev") != nil })
	time.Sleep(300 * time.Millisecond)
	if _, err := os.Stat(VMDir(a.VMsDir, vm.ID)); err != nil {
		t.Fatalf("vm dir: %v", err)
	}
	for _, n := range env.hub.registers() {
		if n.NodeID == "renamed" {
			t.Fatalf("renamed node registered: %+v", n)
		}
	}
}

func TestAgentDelete(t *testing.T) {
	env := startEnv(t, nil)
	vm := env.hub.create("dev")
	eventually(t, "running", func() bool { v, _ := env.hub.vm(vm.ID); return v.State == proto.StateRunning })
	env.hub.delete(vm.ID)
	eventually(t, "dir removed", func() bool { _, err := os.Stat(VMDir(env.agent.VMsDir, vm.ID)); return os.IsNotExist(err) })
	if !env.provider.instance("dev").killed.Load() {
		t.Fatal("instance not killed")
	}
}

func TestAgentDeletesVMsUnknownToHub(t *testing.T) {
	env := startEnv(t, func(a *Agent, _ *fakeProvider) {
		SaveVM(a.VMsDir, cachedVM("g1", "good"))
		SaveVM(a.VMsDir, cachedVM("s1", "sleepy"))
	})
	for _, id := range []string{"g1", "s1"} {
		eventually(t, id+" dir removed", func() bool { _, err := os.Stat(VMDir(env.agent.VMsDir, id)); return os.IsNotExist(err) })
	}
	if len(env.hub.vms()) != 0 {
		t.Fatalf("hub adopted VMs: %+v", env.hub.vms())
	}
	if len(env.provider.prepared) != 0 {
		t.Fatalf("Prepare called on restore: %v", env.provider.prepared)
	}
}

func TestAgentBootsFromCacheWithoutHub(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	unreachable := "ws://" + ln.Addr().String() + "/api/nodes/connect"
	ln.Close()
	p := newFakeProvider()
	a := &Agent{HubURL: unreachable, Credential: "tok", NodeID: "n1", VMsDir: t.TempDir(), CPUs: 4, MemoryMB: 8192, Provider: p}
	stopped := cachedVM("s1", "sleepy")
	stopped.Power = proto.PowerStopped
	SaveVM(a.VMsDir, cachedVM("g1", "good"))
	SaveVM(a.VMsDir, stopped)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { a.Run(ctx); close(done) }()
	t.Cleanup(func() { cancel(); <-done })
	eventually(t, "good started", func() bool { return p.instance("good") != nil })
	time.Sleep(100 * time.Millisecond)
	if p.instance("sleepy") != nil {
		t.Fatal("stopped VM started")
	}
	for _, id := range []string{"g1", "s1"} {
		if _, err := os.Stat(VMDir(a.VMsDir, id)); err != nil {
			t.Fatalf("vm dir %s: %v", id, err)
		}
	}
}

func TestAgentStoppedNotOverwrittenByLateIP(t *testing.T) {
	env := startEnv(t, func(_ *Agent, p *fakeProvider) { p.staleIP["dev"] = true })
	vm := env.hub.create("dev")
	eventually(t, "started", func() bool { return env.provider.instance("dev") != nil })
	env.provider.instance("dev").guestShutdown()
	eventually(t, "stopped", func() bool { v, _ := env.hub.vm(vm.ID); return v.State == proto.StateStopped })
	time.Sleep(200 * time.Millisecond)
	if v, _ := env.hub.vm(vm.ID); v.State != proto.StateStopped {
		t.Fatalf("state = %s, want stopped", v.State)
	}
}

func TestStopAllHibernatesRunningVMs(t *testing.T) {
	freeSpace(t, 1<<40)
	env := startEnv(t, nil)
	vms := []hubVM{env.hub.create("one"), env.hub.create("two")}
	for _, vm := range vms {
		eventually(t, "running", func() bool { v, _ := env.hub.vm(vm.ID); return v.State == proto.StateRunning })
	}
	env.stop()
	<-env.runDone
	for _, vm := range vms {
		dir := VMDir(env.agent.VMsDir, vm.ID)
		if !env.provider.hasEvent("savestate:" + filepath.Join(dir, "state.tmp")) {
			t.Fatalf("%s not saved: %v", vm.Spec.Name, env.provider.gotEvents())
		}
		if !loadVM(t, env.agent, vm.ID).Hibernated {
			t.Fatalf("%s: vm.json not hibernated", vm.Spec.Name)
		}
		if _, err := os.Stat(filepath.Join(dir, StateDir, MemFile)); err != nil {
			t.Fatalf("%s: %v", vm.Spec.Name, err)
		}
	}
	if env.provider.hasEvent("stop-guest") {
		t.Fatalf("guest stopped: %v", env.provider.gotEvents())
	}
}

func TestStopAllBudgetKillsRest(t *testing.T) {
	freeSpace(t, 1<<40)
	old := stopBudget
	stopBudget = 50 * time.Millisecond
	t.Cleanup(func() { stopBudget = old })
	a, p := newReconcileAgent(t)
	a.reconcile(context.Background(), 0, []proto.DesiredVM{dv("a", "a", proto.PowerRunning, 1)})
	waitState(t, a, "a", proto.StateRunning, 1)
	block := make(chan struct{})
	defer close(block)
	p.mu.Lock()
	p.saveBlock = block
	p.mu.Unlock()
	a.stopAll()
	if !p.instance("a").killed.Load() {
		t.Fatalf("not killed: %v", p.gotEvents())
	}
	if loadVM(t, a, "a").Hibernated {
		t.Fatal("vm.json hibernated")
	}
	if _, err := os.Stat(filepath.Join(VMDir(a.VMsDir, "a"), "state.tmp")); !os.IsNotExist(err) {
		t.Fatalf("state.tmp left: %v", err)
	}
}

func TestRestartResumesHibernated(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	unreachable := "ws://" + ln.Addr().String() + "/api/nodes/connect"
	ln.Close()
	p := newFakeProvider()
	a := &Agent{HubURL: unreachable, Credential: "tok", NodeID: "n1", VMsDir: t.TempDir(), CPUs: 4, MemoryMB: 8192, Provider: p}
	state := saveHibernated(t, a, cachedVM("g1", "good"))
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { a.Run(ctx); close(done) }()
	t.Cleanup(func() { cancel(); <-done })
	eventually(t, "restored", func() bool { return p.hasEvent("restore:" + state) })
	p.mu.Lock()
	starts := p.starts["good"]
	p.mu.Unlock()
	if starts != 0 {
		t.Fatalf("cold started %d times", starts)
	}
}

func TestAgentStartsChassisOnUnderlay(t *testing.T) {
	env := startEnv(t, func(a *Agent, _ *fakeProvider) {
		a.ChassisKey, a.ChassisPrivateKey, a.OVNKeyFile = "pub-key", "priv-key", "/vms/ovn-key.pem"
	})
	eventually(t, "chassis started", func() bool { return len(env.provider.gotChassis()) > 0 })
	if n := len(env.provider.gotChassis()); n != 1 {
		t.Fatalf("StartChassis called %d times", n)
	}
	c := env.provider.gotChassis()[0]
	u := env.hub.underlay
	if c.Name != "n1" || c.PrivateKey != "priv-key" || c.Address != u.Address || c.HubKey != u.HubKey || c.SBRemote != u.SBRemote ||
		c.Endpoint != u.Endpoint || c.CACert != u.CACert || c.Cert != u.Cert || c.KeyFile != "/vms/ovn-key.pem" || c.MTU != 1420 {
		t.Fatalf("chassis = %+v", c)
	}
}

func TestRegisterCarriesChassis(t *testing.T) {
	env := startEnv(t, func(a *Agent, _ *fakeProvider) {
		a.ChassisKey, a.ChassisCSR = "pub-key", "CSR PEM"
	})
	if r := env.hub.registers()[0]; r.ChassisKey != "pub-key" || r.ChassisCSR != "CSR PEM" {
		t.Fatalf("register = %+v", r)
	}
}

func TestAgentStopsChassisAfterGuests(t *testing.T) {
	env := startEnv(t, nil)
	vm := env.hub.create("dev")
	eventually(t, "running", func() bool { v, _ := env.hub.vm(vm.ID); return v.State == proto.StateRunning })
	env.stop()
	<-env.runDone
	env.provider.mu.Lock()
	defer env.provider.mu.Unlock()
	if got := strings.Join(env.provider.events, ","); !strings.HasSuffix(got, ",kill-guest,stop-chassis") {
		t.Fatalf("events = %s", got)
	}
}

func TestAgentSerializesChassisStarts(t *testing.T) {
	env := startEnv(t, nil)
	env.provider.block = make(chan struct{})
	go env.agent.startChassis(context.Background(), proto.Underlay{Address: "10.198.0.11/24"})
	eventually(t, "first call", func() bool { return len(env.provider.gotChassis()) == 1 })
	go env.agent.startChassis(context.Background(), proto.Underlay{Address: "10.198.0.12/24"})
	time.Sleep(100 * time.Millisecond)
	close(env.provider.block)
	eventually(t, "second call", func() bool { return len(env.provider.gotChassis()) == 2 })
	if env.provider.maxInflight.Load() != 1 {
		t.Fatalf("StartChassis calls overlapped")
	}
	if got := env.provider.gotChassis()[1].Address; got != "10.198.0.12/24" {
		t.Fatalf("last call carries %q, want newest config", got)
	}
}

func TestAgentStopChassisWaitsForInFlightStart(t *testing.T) {
	env := startEnv(t, nil)
	env.provider.block = make(chan struct{})
	go env.agent.startChassis(context.Background(), proto.Underlay{Address: "10.198.0.11/24"})
	eventually(t, "start in flight", func() bool { return len(env.provider.gotChassis()) == 1 })
	env.stop()
	time.Sleep(100 * time.Millisecond)
	env.provider.mu.Lock()
	early := strings.Contains(strings.Join(env.provider.events, ","), "stop-chassis")
	env.provider.mu.Unlock()
	if early {
		t.Fatal("StopChassis ran while StartChassis was in flight")
	}
	close(env.provider.block)
	<-env.runDone
	env.provider.mu.Lock()
	defer env.provider.mu.Unlock()
	if got := strings.Join(env.provider.events, ","); got != "start-begin,start-end,stop-chassis" {
		t.Fatalf("events = %s", got)
	}
	env.agent.startChassis(context.Background(), proto.Underlay{Address: "10.198.0.13/24"})
	if len(env.provider.chassis) != 1 {
		t.Fatal("chassis started after shutdown")
	}
}

func TestAgentStatusCarriesDisks(t *testing.T) {
	env := startEnv(t, nil)
	vm := env.hub.create("dev")
	eventually(t, "running with disks", func() bool {
		v, _ := env.hub.vm(vm.ID)
		return v.State == proto.StateRunning && len(v.Disks) == 1 && v.Disks[0].Device == "vda"
	})
}

func TestAgentOmitsDisksOnProviderError(t *testing.T) {
	env := startEnv(t, func(_ *Agent, p *fakeProvider) { p.failDisks = true })
	vm := env.hub.create("dev")
	eventually(t, "running", func() bool { v, _ := env.hub.vm(vm.ID); return v.State == proto.StateRunning })
	if v, _ := env.hub.vm(vm.ID); len(v.Disks) != 0 {
		t.Fatalf("disks = %+v", v.Disks)
	}
}

func TestAgentRefreshesDisksWhileRunning(t *testing.T) {
	env := startEnv(t, func(a *Agent, _ *fakeProvider) { a.diskRefresh = 20 * time.Millisecond })
	vm := env.hub.create("dev")
	eventually(t, "running", func() bool { v, _ := env.hub.vm(vm.ID); return v.State == proto.StateRunning })
	env.provider.mu.Lock()
	env.provider.bigRoot = true
	env.provider.mu.Unlock()
	eventually(t, "refreshed disks", func() bool {
		v, _ := env.hub.vm(vm.ID)
		return len(v.Disks) == 1 && v.Disks[0].AllocatedBytes == 42 && v.State == proto.StateRunning
	})
}

func TestAgentShutdownAndStart(t *testing.T) {
	env := startEnv(t, nil)
	vm := env.hub.create("dev")
	eventually(t, "running", func() bool { v, _ := env.hub.vm(vm.ID); return v.State == proto.StateRunning })
	first := env.provider.instance("dev")
	env.hub.setPower(vm.ID, proto.PowerStopped, proto.StopGraceful)
	eventually(t, "stopped", func() bool { v, _ := env.hub.vm(vm.ID); return v.State == proto.StateStopped })
	if !env.provider.hasEvent("stop-guest") || env.provider.hasEvent("kill-guest") {
		t.Fatalf("events = %v", env.provider.events)
	}
	env.hub.setPower(vm.ID, proto.PowerRunning, "")
	eventually(t, "running again", func() bool {
		v, _ := env.hub.vm(vm.ID)
		return v.State == proto.StateRunning && env.provider.instance("dev") != first
	})
}

func TestAgentHardStop(t *testing.T) {
	env := startEnv(t, nil)
	vm := env.hub.create("dev")
	eventually(t, "running", func() bool { v, _ := env.hub.vm(vm.ID); return v.State == proto.StateRunning })
	env.hub.setPower(vm.ID, proto.PowerStopped, proto.StopHard)
	eventually(t, "stopped", func() bool { v, _ := env.hub.vm(vm.ID); return v.State == proto.StateStopped })
	if !env.provider.hasEvent("kill-guest") {
		t.Fatalf("events = %v", env.provider.events)
	}
}

func TestAgentRebuildAfterDirMissing(t *testing.T) {
	env := startEnv(t, nil)
	vm := env.hub.create("dev")
	eventually(t, "running", func() bool { v, _ := env.hub.vm(vm.ID); return v.State == proto.StateRunning })
	env.stop()
	<-env.runDone
	if err := os.RemoveAll(VMDir(env.agent.VMsDir, vm.ID)); err != nil {
		t.Fatal(err)
	}
	old := env.agent
	restarted := &Agent{HubURL: old.HubURL, Credential: old.Credential, NodeID: old.NodeID, VMsDir: old.VMsDir, CPUs: old.CPUs, MemoryMB: old.MemoryMB, Provider: env.provider}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { restarted.Run(ctx); close(done) }()
	t.Cleanup(func() { cancel(); <-done })
	eventually(t, "dir missing", func() bool {
		v, _ := env.hub.vm(vm.ID)
		return v.State == proto.StateError && v.Error == proto.ErrVMDirMissing
	})
	env.hub.setPower(vm.ID, proto.PowerRunning, "")
	eventually(t, "running again", func() bool { v, _ := env.hub.vm(vm.ID); return v.State == proto.StateRunning })
	if n := len(env.provider.prepared); n != 2 {
		t.Fatalf("prepared = %v", env.provider.prepared)
	}
}

func TestStopAllHibernatesInParallel(t *testing.T) {
	freeSpace(t, 1<<40)
	a, p := newReconcileAgent(t)
	a.reconcile(context.Background(), 0, []proto.DesiredVM{dv("a", "a", proto.PowerRunning, 1), dv("b", "b", proto.PowerRunning, 1)})
	waitState(t, a, "a", proto.StateRunning, 1)
	waitState(t, a, "b", proto.StateRunning, 1)
	block := make(chan struct{})
	p.mu.Lock()
	p.saveBlock = block
	p.mu.Unlock()
	done := make(chan struct{})
	go func() { a.stopAll(); close(done) }()
	eventually(t, "both saving at once", func() bool {
		return p.hasEvent("savestate:"+filepath.Join(VMDir(a.VMsDir, "a"), "state.tmp")) &&
			p.hasEvent("savestate:"+filepath.Join(VMDir(a.VMsDir, "b"), "state.tmp"))
	})
	close(block)
	<-done
	for _, id := range []string{"a", "b"} {
		if !loadVM(t, a, id).Hibernated {
			t.Fatalf("%s not hibernated", id)
		}
	}
}

func TestStopAllStopsWhatDoesNotFit(t *testing.T) {
	// Room for one 1 GiB VM's state (RAM + 1 GiB), not two.
	freeSpace(t, 3<<30)
	a, p := newReconcileAgent(t)
	a.reconcile(context.Background(), 0, []proto.DesiredVM{dv("a", "a", proto.PowerRunning, 1), dv("b", "b", proto.PowerRunning, 1)})
	waitState(t, a, "a", proto.StateRunning, 1)
	waitState(t, a, "b", proto.StateRunning, 1)
	a.stopAll()
	hibernated := 0
	for _, id := range []string{"a", "b"} {
		if loadVM(t, a, id).Hibernated {
			hibernated++
		}
	}
	if hibernated != 1 || countEvent(p, "stop-guest") != 1 {
		t.Fatalf("hibernated=%d events=%v", hibernated, p.gotEvents())
	}
}

func TestStopAllNeverResumesCommittedState(t *testing.T) {
	freeSpace(t, 1<<40)
	a, p := newReconcileAgent(t)
	a.reconcile(context.Background(), 0, []proto.DesiredVM{dv("a", "a", proto.PowerRunning, 1)})
	waitState(t, a, "a", proto.StateRunning, 1)
	p.mu.Lock()
	p.failKill = errors.New("kill boom")
	p.mu.Unlock()
	a.stopAll()
	p.mu.Lock()
	p.failKill = nil
	p.mu.Unlock()
	if !loadVM(t, a, "a").Hibernated {
		t.Fatal("vm.json not hibernated")
	}
	if ev := p.gotEvents(); slices.Contains(ev, "stop-guest") || slices.Contains(ev, "resume") || countEvent(p, "kill-failed") != 2 {
		t.Fatalf("events = %v", ev)
	}
}

func TestRestoreOfPausedVMStaysPaused(t *testing.T) {
	a, p := newReconcileAgent(t)
	var mu sync.Mutex
	var states []proto.VMState
	a.send = func(m proto.Message) error {
		if m.Type == "status" {
			mu.Lock()
			states = append(states, m.Status.State)
			mu.Unlock()
		}
		return nil
	}
	f := cachedVM("a", "a")
	f.Power = proto.PowerPaused
	saveHibernated(t, a, f)
	if err := a.restoreAll(context.Background()); err != nil {
		t.Fatal(err)
	}
	waitState(t, a, "a", proto.StatePaused, 1)
	if !p.hasEvent("restore-paused") || p.hasEvent("pause") || p.hasEvent("resume") {
		t.Fatalf("events = %v", p.gotEvents())
	}
	mu.Lock()
	defer mu.Unlock()
	if slices.Contains(states, proto.StateRunning) {
		t.Fatalf("states = %v", states)
	}
}

const testImage = "ghcr.io/asterism-dev/observatory-guest:v1@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

func startImageEnv(t *testing.T, initial proto.ImageStatus) (*testEnv, *imageProvider) {
	t.Helper()
	var ip *imageProvider
	env := startEnv(t, func(a *Agent, p *fakeProvider) {
		ip = &imageProvider{fakeProvider: p, initial: initial}
		a.Provider = ip
	})
	return env, ip
}

func waitPreparingImage(t *testing.T, env *testEnv, id, want string) {
	t.Helper()
	eventually(t, "preparing guest image", func() bool {
		v, _ := env.hub.vm(id)
		return v.State == proto.StateStarting && strings.Contains(v.Error, want)
	})
}

func TestAgentWaitsForGuestImage(t *testing.T) {
	env, ip := startImageEnv(t, proto.ImageStatus{Version: testImage, State: proto.ImagePulling})
	vm := env.hub.create("dev")
	waitPreparingImage(t, env, vm.ID, PreparingImage)
	if v, _ := env.hub.vm(vm.ID); v.Error != PreparingImage || len(v.Disks) != 0 {
		t.Fatalf("error = %q, disks before the root disk exists = %+v", v.Error, v.Disks)
	}
	if got := env.provider.gotPrepared(); len(got) != 0 {
		t.Fatalf("prepared before the image was ready: %v", got)
	}
	cfg := proto.ImageConfig{Cmd: []string{"sleep", "1"}}
	ip.report(t, proto.ImageStatus{Version: testImage, State: proto.ImageReady, Config: cfg})
	eventually(t, "running", func() bool { v, _ := env.hub.vm(vm.ID); return v.State == proto.StateRunning })
	if img, _ := env.provider.gotPreparedImage("dev"); img != testImage {
		t.Fatalf("Prepare got image %q", img)
	}
	if f := loadVM(t, env.agent, vm.ID); f.Image != testImage || !reflect.DeepEqual(f.ImageConfig, cfg) {
		t.Fatalf("vm.json image = %q, config %+v", f.Image, f.ImageConfig)
	}
	if v, _ := env.hub.vm(vm.ID); v.Image != testImage {
		t.Fatalf("status image = %q", v.Image)
	}
}

// Start falls back to cloning the VM's image when a template restore fails, so it must see the image Prepare used.
func TestAgentStartGetsImage(t *testing.T) {
	env, _ := startImageEnv(t, proto.ImageStatus{Version: testImage, State: proto.ImageReady})
	vm := env.hub.create("dev")
	eventually(t, "running", func() bool { v, _ := env.hub.vm(vm.ID); return v.State == proto.StateRunning })
	env.provider.mu.Lock()
	img := env.provider.startedImage["dev"]
	env.provider.mu.Unlock()
	if img != testImage {
		t.Fatalf("Start got image %q", img)
	}
}

func TestAgentWaitsForFailedPull(t *testing.T) {
	retry := time.Date(2026, 11, 18, 12, 30, 0, 0, time.UTC)
	env, ip := startImageEnv(t, proto.ImageStatus{Version: testImage, State: proto.ImageFailed, Error: "apt boom", RetryAt: &retry})
	vm := env.hub.create("dev")
	waitPreparingImage(t, env, vm.ID, "pull failed")
	if v, _ := env.hub.vm(vm.ID); !strings.Contains(v.Error, "apt boom") || !strings.Contains(v.Error, "2026-11-18T12:30:00Z") {
		t.Fatalf("error = %q", v.Error)
	}
	time.Sleep(50 * time.Millisecond)
	if got := env.provider.gotPrepared(); len(got) != 0 {
		t.Fatalf("prepared with a failed image: %v", got)
	}
	ip.report(t, proto.ImageStatus{Version: testImage, State: proto.ImageReady})
	eventually(t, "running", func() bool { v, _ := env.hub.vm(vm.ID); return v.State == proto.StateRunning })
}

func TestAgentPullsUserImage(t *testing.T) {
	env, _ := startImageEnv(t, proto.ImageStatus{Version: testImage, State: proto.ImageReady})
	userImage := &proto.ImageRef{Repo: "hub.example/i/img1", Digest: "sha256:" + strings.Repeat("b", 64), PullToken: "bad"}
	vm := env.hub.createWith("dev", func(v *hubVM) { v.Spec.Image = userImage })
	eventually(t, "pull error", func() bool {
		v, _ := env.hub.vm(vm.ID)
		return v.State == proto.StateError && v.Error == "image pull failed: UNAUTHORIZED"
	})
	if got := env.provider.gotPrepared(); len(got) != 0 {
		t.Fatalf("prepared without the image: %v", got)
	}
	fresh := *userImage
	fresh.PullToken = "t1"
	env.hub.mu.Lock()
	env.hub.vmsByID[vm.ID].Spec.Image = &fresh
	env.hub.mu.Unlock()
	env.hub.push()
	eventually(t, "running", func() bool { v, _ := env.hub.vm(vm.ID); return v.State == proto.StateRunning })
	want := userImage.Repo + "@" + userImage.Digest
	if img, _ := env.provider.gotPreparedImage("dev"); img != want {
		t.Fatalf("Prepare got image %q", img)
	}
	if f := loadVM(t, env.agent, vm.ID); f.Image != want || !reflect.DeepEqual(f.ImageConfig, proto.ImageConfig{Cmd: []string{"app"}}) {
		t.Fatalf("vm.json image = %q, config %+v", f.Image, f.ImageConfig)
	}
}

func TestAgentGoneUserImage(t *testing.T) {
	env, _ := startImageEnv(t, proto.ImageStatus{Version: testImage, State: proto.ImageReady})
	digest := "sha256:" + strings.Repeat("b", 64)
	vm := env.hub.createWith("dev", func(v *hubVM) { v.Spec.Image = &proto.ImageRef{Digest: digest, Gone: true} })
	eventually(t, "gone error", func() bool {
		v, _ := env.hub.vm(vm.ID)
		return v.State == proto.StateError && v.Error == "image no longer available"
	})
	pulled := slices.ContainsFunc(env.provider.gotEvents(), func(e string) bool { return strings.HasPrefix(e, "pull:") })
	if pulled || len(env.provider.gotPrepared()) != 0 {
		t.Fatalf("events = %v", env.provider.gotEvents())
	}
	env.hub.mu.Lock()
	env.hub.vmsByID[vm.ID].Spec.Image = &proto.ImageRef{Repo: "hub.example/i/img1", Digest: digest, PullToken: "t1"}
	env.hub.mu.Unlock()
	env.hub.push()
	eventually(t, "running", func() bool { v, _ := env.hub.vm(vm.ID); return v.State == proto.StateRunning })
}

func TestPullTokenNotPersisted(t *testing.T) {
	env, _ := startImageEnv(t, proto.ImageStatus{Version: testImage, State: proto.ImageReady})
	vm := env.hub.createWith("dev", func(v *hubVM) {
		v.Spec.Image = &proto.ImageRef{Repo: "hub.example/i/img1", Digest: "sha256:" + strings.Repeat("b", 64), PullToken: "t1"}
	})
	eventually(t, "running", func() bool { v, _ := env.hub.vm(vm.ID); return v.State == proto.StateRunning })
	b, err := os.ReadFile(filepath.Join(VMDir(env.agent.VMsDir, vm.ID), "vm.json"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), "pullToken") || !strings.Contains(string(b), "hub.example/i/img1") {
		t.Fatalf("vm.json = %s", b)
	}
	env.agent.mu.Lock()
	tok := env.agent.vms[vm.ID].file.Spec.Image.PullToken
	env.agent.mu.Unlock()
	if tok != "t1" {
		t.Fatalf("token in memory = %q", tok)
	}
}

func TestAgentReportsImage(t *testing.T) {
	env, ip := startImageEnv(t, proto.ImageStatus{Version: testImage, State: proto.ImagePulling})
	ready := proto.ImageStatus{Version: testImage, State: proto.ImageReady}
	ip.report(t, ready)
	isReady := func(st proto.ImageStatus) bool { return st.State == proto.ImageReady }
	eventually(t, "image message", func() bool { return slices.ContainsFunc(env.hub.gotImages(), isReady) })
	time.Sleep(50 * time.Millisecond)
	if n := len(slices.DeleteFunc(env.hub.gotImages(), func(st proto.ImageStatus) bool { return !isReady(st) })); n != 1 {
		t.Fatalf("%d ready image messages", n)
	}
	env.agent.mu.Lock()
	reg := env.agent.registerMsg()
	env.agent.mu.Unlock()
	if reg.Image == nil || !reflect.DeepEqual(*reg.Image, ready) {
		t.Fatalf("register image = %+v", reg.Image)
	}
}

func TestAgentWithoutGuestImages(t *testing.T) {
	env := startEnv(t, nil)
	vm := env.hub.create("dev")
	eventually(t, "running", func() bool { v, _ := env.hub.vm(vm.ID); return v.State == proto.StateRunning })
	if img, ok := env.provider.gotPreparedImage("dev"); !ok || img != "" {
		t.Fatalf("Prepare got image %q (called %v)", img, ok)
	}
	if v, _ := env.hub.vm(vm.ID); v.Image != "" {
		t.Fatalf("status image = %q", v.Image)
	}
	if reg := env.hub.registers()[0]; reg.Image != nil {
		t.Fatalf("register image = %+v", reg.Image)
	}
}

func TestAgentDeleteWhileWaitingForImage(t *testing.T) {
	env, ip := startImageEnv(t, proto.ImageStatus{Version: testImage, State: proto.ImagePulling})
	vm := env.hub.create("dev")
	waitPreparingImage(t, env, vm.ID, PreparingImage)
	env.agent.mu.Lock()
	e := env.agent.vms[vm.ID]
	env.agent.mu.Unlock()
	env.hub.delete(vm.ID)
	select {
	case <-e.booted:
	case <-time.After(5 * time.Second):
		t.Fatal("wait for the image did not end")
	}
	eventually(t, "removed", func() bool {
		env.agent.mu.Lock()
		defer env.agent.mu.Unlock()
		_, ok := env.agent.vms[vm.ID]
		return !ok && len(env.agent.dying) == 0 && !exists(VMDir(env.agent.VMsDir, vm.ID))
	})
	ip.report(t, proto.ImageStatus{Version: testImage, State: proto.ImageReady})
	if got := env.provider.gotPrepared(); len(got) != 0 {
		t.Fatalf("deleted VM prepared: %v", got)
	}
}

func TestAgentStopWhileWaitingForImage(t *testing.T) {
	env, ip := startImageEnv(t, proto.ImageStatus{Version: testImage, State: proto.ImagePulling})
	vm := env.hub.create("dev")
	waitPreparingImage(t, env, vm.ID, PreparingImage)
	env.hub.setPower(vm.ID, proto.PowerStopped, proto.StopGraceful)
	eventually(t, "stopped", func() bool { v, _ := env.hub.vm(vm.ID); return v.State == proto.StateStopped })
	env.hub.setPower(vm.ID, proto.PowerRunning, "")
	waitPreparingImage(t, env, vm.ID, PreparingImage)
	if got := env.provider.gotPrepared(); len(got) != 0 {
		t.Fatalf("prepared before the image was ready: %v", got)
	}
	ip.report(t, proto.ImageStatus{Version: testImage, State: proto.ImageReady})
	eventually(t, "running", func() bool { v, _ := env.hub.vm(vm.ID); return v.State == proto.StateRunning })
	if got := env.provider.gotPrepared(); len(got) != 1 {
		t.Fatalf("prepared = %v", got)
	}
}

func TestAgentSameGenerationKeepsWaitingForImage(t *testing.T) {
	env, ip := startImageEnv(t, proto.ImageStatus{Version: testImage, State: proto.ImagePulling})
	vm := env.hub.create("dev")
	waitPreparingImage(t, env, vm.ID, PreparingImage)
	env.agent.mu.Lock()
	e := env.agent.vms[vm.ID]
	d := proto.DesiredVM{Spec: e.file.Spec, Power: proto.PowerRunning, Generation: e.file.Generation}
	env.agent.mu.Unlock()
	// A restore that is due queues at the same generation, which applyPending then skips.
	env.agent.queue(context.Background(), e, d)
	time.Sleep(50 * time.Millisecond)
	if st, _ := env.agent.vmStatus(vm.ID); st.State != proto.StateStarting || st.Error != PreparingImage {
		t.Fatalf("status = %+v", st)
	}
	ip.report(t, proto.ImageStatus{Version: testImage, State: proto.ImageReady})
	eventually(t, "running", func() bool { v, _ := env.hub.vm(vm.ID); return v.State == proto.StateRunning })
}

// A VM made for the cloud-init guest neither boots nor gets a new root disk on the new runtime.
func TestPrepareRejectsLegacyVM(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	unreachable := "ws://" + ln.Addr().String() + "/api/nodes/connect"
	ln.Close()
	p := newFakeProvider()
	a := &Agent{HubURL: unreachable, Credential: "tok", NodeID: "n1", VMsDir: t.TempDir(), CPUs: 4, MemoryMB: 8192,
		Provider: &imageProvider{fakeProvider: p, initial: proto.ImageStatus{Version: testImage, State: proto.ImageReady}}}
	stock, built, unprepared, current := cachedVM("l1", "stock"), cachedVM("l2", "built"), cachedVM("l3", "unprepared"), cachedVM("n1", "current")
	built.Image = "2026.11.1-aaaaaaaa"
	unprepared.Image, unprepared.NeedsPrepare = "2026.11.1-aaaaaaaa", true
	current.Image = testImage
	for _, f := range []VMFile{stock, built, unprepared, current} {
		SaveVM(a.VMsDir, f)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { a.Run(ctx); close(done) }()
	t.Cleanup(func() { cancel(); <-done })
	eventually(t, "current started", func() bool { return p.instance("current") != nil })
	for _, id := range []string{"l1", "l2", "l3"} {
		eventually(t, id+" refused", func() bool {
			st, _ := a.vmStatus(id)
			return st.State == proto.StateError && strings.HasSuffix(st.Error, ErrLegacyRuntime.Error())
		})
	}
	if got := p.gotPrepared(); len(got) != 0 {
		t.Fatalf("prepared = %v", got)
	}
	for _, name := range []string{"stock", "built", "unprepared"} {
		if p.instance(name) != nil {
			t.Fatalf("%s started", name)
		}
	}
}

func TestNodeHandlesProcessesRequest(t *testing.T) {
	env := startEnv(t, nil)
	vm := env.hub.create("dev")
	eventually(t, "running", func() bool { v, _ := env.hub.vm(vm.ID); return v.State == proto.StateRunning })
	env.hub.send(proto.Message{Type: "processes", ReqID: "r1", VM: vm.ID})
	env.hub.send(proto.Message{Type: "processes", ReqID: "r2", VM: "nope"})
	eventually(t, "replies", func() bool { return len(env.hub.processReplies()) == 2 })
	for _, r := range env.hub.processReplies() {
		switch r.ReqID {
		case "r1":
			if r.Error != "" || r.VM != vm.ID || len(r.Processes) != 1 || r.Processes[0].Command != "/sbin/init" {
				t.Fatalf("r1 = %+v", r)
			}
		case "r2":
			if r.Error != proto.ErrVMNotRunning || r.Processes != nil {
				t.Fatalf("r2 = %+v", r)
			}
		default:
			t.Fatalf("reply %+v", r)
		}
	}
}

func TestNodeBoundsGuestProcesses(t *testing.T) {
	env := startEnv(t, nil)
	vm := env.hub.create("dev")
	eventually(t, "running", func() bool { v, _ := env.hub.vm(vm.ID); return v.State == proto.StateRunning })
	ps := make([]proto.GuestProcess, 600)
	for i := range ps {
		ps[i] = proto.GuestProcess{PID: i, Command: strings.Repeat("c", 4000), User: strings.Repeat("u", 100), State: strings.Repeat("s", 20)}
	}
	env.provider.mu.Lock()
	env.provider.processes = ps
	env.provider.mu.Unlock()
	env.hub.send(proto.Message{Type: "processes", ReqID: "big", VM: vm.ID})
	eventually(t, "reply", func() bool { return len(env.hub.processReplies()) == 1 })
	r := env.hub.processReplies()[0]
	if len(r.Processes) != 500 {
		t.Fatalf("%d entries", len(r.Processes))
	}
	if p := r.Processes[0]; len(p.Command) != 512 || len(p.User) != 64 || len(p.State) != 8 {
		t.Fatalf("entry sizes %d/%d/%d", len(p.Command), len(p.User), len(p.State))
	}

	env.provider.mu.Lock()
	env.provider.processesErr = errors.New(strings.Repeat("e", 1000))
	env.provider.mu.Unlock()
	env.hub.send(proto.Message{Type: "processes", ReqID: "err", VM: vm.ID})
	eventually(t, "error reply", func() bool { return len(env.hub.processReplies()) == 2 })
	if r := env.hub.processReplies()[1]; len(r.Error) != 256 || r.Processes != nil {
		t.Fatalf("error reply: %d bytes, %d entries", len(r.Error), len(r.Processes))
	}
}

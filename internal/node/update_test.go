package node

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/asterism-dev/observatory/internal/proto"
)

type fakeUpdater struct {
	mu       sync.Mutex
	starts   []string
	startErr error
	waitErr  error
	block    chan struct{} // Wait blocks until closed, if set
}

func (u *fakeUpdater) Start(_ context.Context, hubBase string) error {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.starts = append(u.starts, hubBase)
	return u.startErr
}

func (u *fakeUpdater) Wait(ctx context.Context) error {
	if u.block != nil {
		select {
		case <-u.block:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return u.waitErr
}

func (u *fakeUpdater) gotStarts() []string {
	u.mu.Lock()
	defer u.mu.Unlock()
	return append([]string(nil), u.starts...)
}

func TestHubBase(t *testing.T) {
	for in, want := range map[string]string{
		"ws://10.0.0.1:8080/api/nodes/connect":    "http://10.0.0.1:8080",
		"wss://hub.example.com/api/nodes/connect": "https://hub.example.com",
	} {
		if got := hubBase(in); got != want {
			t.Errorf("hubBase(%q) = %q, want %q", in, got, want)
		}
	}
}

func startUpdateEnv(t *testing.T, u *fakeUpdater) *testEnv {
	t.Helper()
	return startEnv(t, func(a *Agent, _ *fakeProvider) {
		if u != nil {
			a.Updater = u
		}
	})
}

func waitUpdateError(t *testing.T, env *testEnv, want string) {
	t.Helper()
	eventually(t, "update report", func() bool {
		r := env.hub.gotUpdates()
		return len(r) == 1 && strings.Contains(r[0].Error, want)
	})
}

func TestUpdateRunsUpdater(t *testing.T) {
	u := &fakeUpdater{block: make(chan struct{})}
	defer close(u.block)
	env := startUpdateEnv(t, u)
	env.hub.sendUpdate()
	eventually(t, "started", func() bool { return len(u.gotStarts()) == 1 })
	if got, want := u.gotStarts()[0], hubBase(env.hub.url()); got != want {
		t.Fatalf("hub base = %q, want %q", got, want)
	}
}

func TestUpdateReportsStartFailure(t *testing.T) {
	env := startUpdateEnv(t, &fakeUpdater{startErr: errors.New("boom")})
	env.hub.sendUpdate()
	waitUpdateError(t, env, "boom")
}

func TestUpdateReportsUnitFailure(t *testing.T) {
	env := startUpdateEnv(t, &fakeUpdater{waitErr: errors.New("install.sh --update failed")})
	env.hub.sendUpdate()
	waitUpdateError(t, env, "install.sh --update failed")
}

func TestUpdateIgnoresSecondRequest(t *testing.T) {
	u := &fakeUpdater{block: make(chan struct{})}
	defer close(u.block)
	env := startUpdateEnv(t, u)
	env.hub.sendUpdate()
	eventually(t, "started", func() bool { return len(u.gotStarts()) == 1 })
	env.hub.sendUpdate()
	time.Sleep(100 * time.Millisecond)
	if n := len(u.gotStarts()); n != 1 {
		t.Fatalf("Start called %d times", n)
	}
}

func TestUpdateWithoutUpdater(t *testing.T) {
	env := startUpdateEnv(t, nil)
	env.hub.sendUpdate()
	waitUpdateError(t, env, "updates not supported on this node")
}

func TestStopAllShutsDownWithMarker(t *testing.T) {
	freeSpace(t, 1<<40)
	marker := filepath.Join(t.TempDir(), "shutdown-vms")
	env := startEnv(t, func(a *Agent, _ *fakeProvider) { a.ShutdownMarker = marker })
	vm := env.hub.create("one")
	eventually(t, "running", func() bool { v, _ := env.hub.vm(vm.ID); return v.State == proto.StateRunning })
	if err := os.WriteFile(marker, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	env.stop()
	<-env.runDone
	if env.provider.hasEvent("savestate:"+filepath.Join(VMDir(env.agent.VMsDir, vm.ID), "state.tmp")) || !env.provider.hasEvent("stop-guest") {
		t.Fatalf("events = %v", env.provider.gotEvents())
	}
	if loadVM(t, env.agent, vm.ID).Hibernated {
		t.Fatal("vm.json hibernated")
	}
}

func TestRunRemovesShutdownMarker(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "shutdown-vms")
	if err := os.WriteFile(marker, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	startEnv(t, func(a *Agent, _ *fakeProvider) { a.ShutdownMarker = marker })
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("marker still there: %v", err)
	}
}

func TestRestoreAfterShutdownKeepsPower(t *testing.T) {
	freeSpace(t, 1<<40)
	marker := filepath.Join(t.TempDir(), "shutdown-vms")
	env := startEnv(t, func(a *Agent, _ *fakeProvider) { a.ShutdownMarker = marker })
	run, paused, hib := env.hub.create("run"), env.hub.create("paused"), env.hub.create("hib")
	for _, vm := range []hubVM{run, paused, hib} {
		eventually(t, "running", func() bool { v, _ := env.hub.vm(vm.ID); return v.State == proto.StateRunning })
	}
	env.hub.setPower(paused.ID, proto.PowerPaused, "")
	env.hub.setPower(hib.ID, proto.PowerHibernated, "")
	eventually(t, "paused", func() bool { v, _ := env.hub.vm(paused.ID); return v.State == proto.StatePaused })
	eventually(t, "hibernated", func() bool { v, _ := env.hub.vm(hib.ID); return v.State == proto.StateHibernated })
	if err := os.WriteFile(marker, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	env.stop()
	<-env.runDone

	// A hypervisor change may leave a hibernated VM without usable memory; it then rests stopped.
	f := loadVM(t, env.agent, hib.ID)
	f.Hibernated = false
	if err := SaveVM(env.agent.VMsDir, f); err != nil {
		t.Fatal(err)
	}
	p := newFakeProvider()
	a := &Agent{HubURL: env.hub.url(), Credential: "tok", NodeID: "n1", VMsDir: env.agent.VMsDir, CPUs: 4, MemoryMB: 8192, Provider: p, ShutdownMarker: marker}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { a.Run(ctx); close(done) }()
	t.Cleanup(func() { cancel(); <-done })

	eventually(t, "node back", func() bool { return len(env.hub.registers()) == 2 })
	for id, want := range map[string]proto.VMState{run.ID: proto.StateRunning, paused.ID: proto.StatePaused, hib.ID: proto.StateStopped} {
		eventually(t, string(want), func() bool { st, _ := a.vmStatus(id); return st.State == want })
	}
	if p.hasEvent("restore-paused") || slices.ContainsFunc(p.gotEvents(), func(e string) bool { return strings.HasPrefix(e, "restore:") }) {
		t.Fatalf("memory resumed: %v", p.gotEvents())
	}
}

func TestUpdateFailureRemovesShutdownMarker(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "shutdown-vms")
	env := startEnv(t, func(a *Agent, _ *fakeProvider) {
		a.Updater = &fakeUpdater{waitErr: errors.New("install.sh --update failed")}
		a.ShutdownMarker = marker
	})
	if err := os.WriteFile(marker, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	env.hub.sendUpdate()
	waitUpdateError(t, env, "install.sh --update failed")
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("marker still there: %v", err)
	}
}

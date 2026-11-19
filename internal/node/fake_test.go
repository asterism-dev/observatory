package node

import (
	"context"
	"errors"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/asterism-dev/observatory/internal/chassisconf"
	"github.com/asterism-dev/observatory/internal/proto"
)

type fakeInstance struct {
	once    sync.Once
	done    chan struct{}
	stopped atomic.Bool
	killed  atomic.Bool
	p       *fakeProvider
	staleIP bool // simulates finding the previous boot's lease only after the VM already died
	hang    bool // Stop and Kill do not end the instance

	ticks, tickStep atomic.Uint64 // CPUTicks advances by tickStep per call
}

func (i *fakeInstance) WaitIP(context.Context) (string, error) {
	if i.staleIP {
		<-i.done
	}
	return "127.0.0.1", nil
}
func (i *fakeInstance) Done() <-chan struct{} { return i.done }
func (i *fakeInstance) guestShutdown()        { i.once.Do(func() { close(i.done) }) }

func (i *fakeInstance) Stop() error {
	if i.hang {
		i.p.record("stop-guest")
		return nil
	}
	i.once.Do(func() { i.stopped.Store(true); close(i.done) })
	if i.p != nil {
		i.p.record("stop-guest")
	}
	return nil
}

func (i *fakeInstance) Kill() error {
	i.p.mu.Lock()
	failKill := i.p.failKill
	i.p.mu.Unlock()
	if failKill != nil {
		i.p.record("kill-failed")
		return failKill
	}
	if i.hang {
		i.p.record("kill-guest")
		return nil
	}
	// Recorded before done closes: done wakes the agent, whose teardown events must come after.
	if i.p != nil {
		i.p.record("kill-guest")
	}
	i.once.Do(func() { i.killed.Store(true); close(i.done) })
	return nil
}

func (i *fakeInstance) Pause() error  { i.p.record("pause"); return nil }
func (i *fakeInstance) Resume() error { i.p.record("resume"); return nil }
func (i *fakeInstance) Flush() error  { i.p.record("flush"); return nil }

func (i *fakeInstance) CPUTicks() (uint64, error) { return i.ticks.Add(i.tickStep.Load()), nil }
func (i *fakeInstance) Memory() (int, int, error) { return 1024, 256, nil }

func (i *fakeInstance) ReloadSeed() error { i.p.record("reload-seed"); return nil }

func (i *fakeInstance) GuestResume(context.Context) error {
	i.p.mu.Lock()
	i.p.guestResumes = append(i.p.guestResumes, time.Now())
	i.p.mu.Unlock()
	return nil
}

func (i *fakeInstance) GuestTrust(_ context.Context, cas map[string]string) error {
	i.p.record("guest-trust")
	i.p.mu.Lock()
	block := i.p.trustBlock
	i.p.trustBlock = nil
	i.p.mu.Unlock()
	if block != nil {
		<-block
	}
	i.p.mu.Lock()
	i.p.trusts = append(i.p.trusts, cas)
	i.p.mu.Unlock()
	return nil
}

func (i *fakeInstance) SaveState(dir string) error {
	i.p.record("savestate:" + dir)
	i.p.mu.Lock()
	block, fail := i.p.saveBlock, i.p.failSaveState
	i.p.mu.Unlock()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	if block != nil {
		// Killing Firecracker breaks its snapshot call.
		select {
		case <-block:
		case <-i.done:
			return errors.New("killed while saving")
		}
	}
	for _, n := range []string{StateFile, MemFile} {
		if err := os.WriteFile(filepath.Join(dir, n), []byte(n), 0o600); err != nil {
			return err
		}
	}
	return fail
}

type fakeProvider struct {
	mu            sync.Mutex
	prepared      []string
	preparedImage map[string]string // f.Image of the latest Prepare per VM
	startedImage  map[string]string // f.Image of the latest Start per VM
	started       map[string]*fakeInstance
	failPrepare   map[string]bool
	failStart     map[string]bool
	staleIP       map[string]bool
	chassis       []chassisconf.Config
	block         chan struct{} // when set, StartChassis waits for it to close
	inflight      atomic.Int32
	maxInflight   atomic.Int32
	events        []string
	failDisks     bool
	failReseed    bool
	starts        map[string]int
	boots         map[string]*proto.Boot // Boot of the latest Start per VM
	bigRoot       bool                   // makes Disks report a different allocation, as if the guest wrote data
	afterPrepare  func(dir string)
	beforeStart   func(dir string)
	failRestore   error
	beforeRestore func(dir string)
	saveBlock     chan struct{}
	restoreBlock  chan struct{}
	failSaveState error
	failKill      error
	guestResumes  []time.Time
	restores      map[string]int
	idle, wake    chan struct{} // a send makes a waiting WaitIdle or WaitWake return nil
	wakeTeardown  chan struct{} // when set, a cancelled WaitWake removes its taps only once it is closed
	trusts        []map[string]string
	trustBlock    chan struct{} // when set, the next GuestTrust waits for it to close
}

func (p *fakeProvider) Disks(f VMFile, _ string) ([]proto.Disk, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.failDisks {
		return nil, errors.New("disks boom")
	}
	d := proto.Disk{Name: "root", File: "root.img", Device: "vda", SizeBytes: int64(f.Spec.DiskGB) << 30}
	if p.bigRoot {
		d.AllocatedBytes = 42
	}
	return []proto.Disk{d}, nil
}

func (p *fakeProvider) record(e string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.events = append(p.events, e)
}

func (p *fakeProvider) StartChassis(_ context.Context, c chassisconf.Config) error {
	n := p.inflight.Add(1)
	defer p.inflight.Add(-1)
	if n > p.maxInflight.Load() {
		p.maxInflight.Store(n)
	}
	p.mu.Lock()
	p.chassis = append(p.chassis, c)
	block := p.block
	p.mu.Unlock()
	p.record("start-begin")
	if block != nil {
		<-block
	}
	p.record("start-end")
	return nil
}

func (p *fakeProvider) StopChassis() error {
	p.record("stop-chassis")
	return nil
}

func (p *fakeProvider) gotChassis() []chassisconf.Config {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]chassisconf.Config(nil), p.chassis...)
}

func newFakeProvider() *fakeProvider {
	return &fakeProvider{idle: make(chan struct{}), wake: make(chan struct{}), started: map[string]*fakeInstance{}, starts: map[string]int{}, failPrepare: map[string]bool{}, failStart: map[string]bool{}, staleIP: map[string]bool{}}
}

func (p *fakeProvider) Name() string { return "fake" }

func (p *fakeProvider) Prepare(_ context.Context, f VMFile, dir string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.prepared = append(p.prepared, f.Spec.Name)
	if p.preparedImage == nil {
		p.preparedImage = map[string]string{}
	}
	p.preparedImage[f.Spec.Name] = f.Image
	p.events = append(p.events, "prepare:"+f.Spec.Name)
	if p.failPrepare[f.Spec.Name] {
		return errors.New("prepare boom")
	}
	if p.afterPrepare != nil {
		p.afterPrepare(dir)
	}
	return nil
}

// imageProvider is a fakeProvider with a guest image whose state the test reports.
type imageProvider struct {
	*fakeProvider
	initial proto.ImageStatus
	sink    func(proto.ImageStatus) // guarded by fakeProvider.mu
}

func (p *imageProvider) RunGuestImages(ctx context.Context, report func(proto.ImageStatus)) {
	p.mu.Lock()
	p.sink = report
	p.mu.Unlock()
	report(p.initial)
	<-ctx.Done()
}

func (p *imageProvider) report(t *testing.T, st proto.ImageStatus) {
	t.Helper()
	var sink func(proto.ImageStatus)
	eventually(t, "RunGuestImages", func() bool { p.mu.Lock(); defer p.mu.Unlock(); sink = p.sink; return sink != nil })
	sink(st)
}

func (p *fakeProvider) gotPreparedImage(name string) (string, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	img, ok := p.preparedImage[name]
	return img, ok
}

func (p *fakeProvider) Reseed(_ context.Context, f VMFile, _ string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.events = append(p.events, "reseed:"+f.Spec.Name)
	if f.Boot != nil {
		p.events = append(p.events, "reseed-keys:"+strings.Join(f.Boot.SSHPublicKeys, ","))
		if len(f.Boot.EgressCAs) > 0 {
			p.events = append(p.events, "reseed-cas:"+strings.Join(slices.Sorted(maps.Keys(f.Boot.EgressCAs)), ","))
		}
	}
	if p.failReseed {
		return errors.New("reseed boom")
	}
	return nil
}

func (p *fakeProvider) Start(_ context.Context, f VMFile, dir string) (Instance, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.beforeStart != nil {
		p.beforeStart(dir)
	}
	if p.failStart[f.Spec.Name] {
		return nil, errors.New("start boom")
	}
	p.starts[f.Spec.Name]++
	p.events = append(p.events, "start:"+f.Spec.Name)
	if p.boots == nil {
		p.boots = map[string]*proto.Boot{}
	}
	p.boots[f.Spec.Name] = f.Boot
	if p.startedImage == nil {
		p.startedImage = map[string]string{}
	}
	p.startedImage[f.Spec.Name] = f.Image
	inst := &fakeInstance{p: p, done: make(chan struct{}), staleIP: p.staleIP[f.Spec.Name]}
	p.started[f.Spec.Name] = inst
	return inst, nil
}

func (p *fakeProvider) Restore(_ context.Context, f VMFile, dir string, from RestoreFrom) (Instance, error) {
	p.record("restore:" + from.StateDir)
	if from.Paused {
		p.record("restore-paused")
	}
	if from.Takeoff {
		p.record("restore-takeoff")
	}
	p.mu.Lock()
	block := p.restoreBlock
	p.mu.Unlock()
	if block != nil {
		<-block
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.beforeRestore != nil {
		p.beforeRestore(dir)
	}
	if p.failRestore != nil {
		return nil, p.failRestore
	}
	if p.restores == nil {
		p.restores = map[string]int{}
	}
	p.restores[f.Spec.Name]++
	if !from.Shared {
		os.RemoveAll(from.StateDir)
	}
	inst := &fakeInstance{p: p, done: make(chan struct{})}
	p.started[f.Spec.Name] = inst
	return inst, nil
}

func (p *fakeProvider) WaitIdle(ctx context.Context, _ VMFile, _ time.Duration) error {
	return p.wait(ctx, p.idle, "wait-idle")
}

func (p *fakeProvider) WaitWake(ctx context.Context, _ VMFile) error {
	err := p.wait(ctx, p.wake, "wait-wake")
	p.mu.Lock()
	block := p.wakeTeardown
	p.mu.Unlock()
	if err != nil && block != nil {
		p.record("wake-teardown-begin")
		<-block
		p.record("wake-teardown-end")
	}
	return err
}

func (p *fakeProvider) wait(ctx context.Context, ch chan struct{}, name string) error {
	p.record(name)
	select {
	case <-ch:
		return nil
	case <-ctx.Done():
		p.record(name + "-cancelled")
		return ctx.Err()
	}
}

func (p *fakeProvider) startedBoot(name string) *proto.Boot {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.boots[name]
}

func (p *fakeProvider) instance(name string) *fakeInstance {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.started[name]
}

func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timeout waiting for %s", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func (p *fakeProvider) hasEvent(e string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return slices.Contains(p.events, e)
}

func cachedVM(id, name string) VMFile {
	return VMFile{Spec: proto.VMSpec{ID: id, Name: name, CPUs: 1, MemoryMB: 1024, DiskGB: 10},
		MAC: "02:00:00:00:00:01", Power: proto.PowerRunning, Generation: 1}
}

// fakeEgress records its calls in the provider's event log, so they order against the VM's.
type fakeEgress struct {
	p        *fakeProvider
	mu       sync.Mutex
	procs    []*fakeProc
	sent     []proto.Egress
	failSend error
	noReady  bool
}

type fakeProc struct {
	h           *fakeEgress
	ready, done chan struct{}
	once        sync.Once
}

func (h *fakeEgress) Spawn(VMFile) (EgressProc, error) {
	h.p.record("spawn")
	h.mu.Lock()
	defer h.mu.Unlock()
	pr := &fakeProc{h: h, ready: make(chan struct{}), done: make(chan struct{})}
	if !h.noReady {
		close(pr.ready)
	}
	h.procs = append(h.procs, pr)
	return pr, nil
}

func (h *fakeEgress) Redirect(VMFile) error   { h.p.record("redirect"); return nil }
func (h *fakeEgress) Unredirect(VMFile) error { h.p.record("unredirect"); return nil }

func (h *fakeEgress) last() *fakeProc {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.procs[len(h.procs)-1]
}

func (h *fakeEgress) gotSent() []proto.Egress {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]proto.Egress(nil), h.sent...)
}

func (pr *fakeProc) Send(t proto.Egress) error {
	pr.h.p.record("send")
	pr.h.mu.Lock()
	defer pr.h.mu.Unlock()
	pr.h.sent = append(pr.h.sent, t)
	return pr.h.failSend
}

func (pr *fakeProc) Ready() <-chan struct{} { return pr.ready }
func (pr *fakeProc) Done() <-chan struct{}  { return pr.done }
func (pr *fakeProc) Kill()                  { pr.h.p.record("kill"); pr.crash() }
func (pr *fakeProc) crash()                 { pr.once.Do(func() { close(pr.done) }) }

func (p *fakeProvider) Hypervisor() proto.Hypervisor {
	return proto.Hypervisor{Type: "firecracker", Version: "v1.18.0"}
}

package node

import (
	"context"
	"errors"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

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
	if i.hang {
		i.p.record("kill-guest")
		return nil
	}
	i.once.Do(func() { i.killed.Store(true); close(i.done) })
	if i.p != nil {
		i.p.record("kill-guest")
	}
	return nil
}

type fakeProvider struct {
	mu           sync.Mutex
	prepared     []string
	started      map[string]*fakeInstance
	failPrepare  map[string]bool
	failStart    map[string]bool
	staleIP      map[string]bool
	chassis      []Chassis
	block        chan struct{} // when set, StartChassis waits for it to close
	inflight     atomic.Int32
	maxInflight  atomic.Int32
	events       []string
	failDisks    bool
	failReseed   bool
	starts       map[string]int
	boots        map[string]*proto.Boot // Boot of the latest Start per VM
	bigRoot      bool                   // makes Disks report a different allocation, as if the guest wrote data
	afterPrepare func(dir string)
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

func (p *fakeProvider) StartChassis(_ context.Context, c Chassis) error {
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

func (p *fakeProvider) gotChassis() []Chassis {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]Chassis(nil), p.chassis...)
}

func newFakeProvider() *fakeProvider {
	return &fakeProvider{started: map[string]*fakeInstance{}, starts: map[string]int{}, failPrepare: map[string]bool{}, failStart: map[string]bool{}, staleIP: map[string]bool{}}
}

func (p *fakeProvider) Name() string { return "fake" }

func (p *fakeProvider) Prepare(_ context.Context, f VMFile, dir string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.prepared = append(p.prepared, f.Spec.Name)
	p.events = append(p.events, "prepare:"+f.Spec.Name)
	if p.failPrepare[f.Spec.Name] {
		return errors.New("prepare boom")
	}
	if p.afterPrepare != nil {
		p.afterPrepare(dir)
	}
	return nil
}

func (p *fakeProvider) Reseed(_ context.Context, f VMFile, _ string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.events = append(p.events, "reseed:"+f.Spec.Name)
	if p.failReseed {
		return errors.New("reseed boom")
	}
	return nil
}

func (p *fakeProvider) Start(f VMFile, _ string) (Instance, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.failStart[f.Spec.Name] {
		return nil, errors.New("start boom")
	}
	p.starts[f.Spec.Name]++
	if p.boots == nil {
		p.boots = map[string]*proto.Boot{}
	}
	p.boots[f.Spec.Name] = f.Boot
	inst := &fakeInstance{p: p, done: make(chan struct{}), staleIP: p.staleIP[f.Spec.Name]}
	p.started[f.Spec.Name] = inst
	return inst, nil
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

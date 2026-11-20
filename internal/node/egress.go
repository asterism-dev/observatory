package node

import (
	"context"
	"log/slog"
	"maps"
	"reflect"
	"sync"
	"time"

	"github.com/asterism-dev/observatory/internal/proto"
)

// EgressHost runs a VM's egress proxy process and redirects the VM's HTTPS to it.
type EgressHost interface {
	Spawn(f VMFile) (EgressProc, error)
	Redirect(f VMFile) error
	Unredirect(f VMFile) error
}

type EgressProc interface {
	Send(proto.Egress) error
	Ready() <-chan struct{}
	Done() <-chan struct{}
	Kill()
	// Usage carries the proxy's Claude usage reports, ids only; the proxy throttles them.
	Usage() <-chan proto.UsageObservation
}

var testHookBeforeEgressUp func()

var (
	egressReadyTimeout = 10 * time.Second
	guestTrustTimeout  = 30 * time.Second
)

// egressSup supervises one VM's proxy; its fields other than the channels are guarded by Agent.egressMu.
type egressSup struct {
	f       VMFile // f.Egress is never nil
	halted  bool
	prev    *egressSup // a halted predecessor, whose redirect must be gone before this one's
	changed chan struct{}
	ready   chan struct{} // closed once the first redirect is in place
	once    sync.Once
	stop    context.CancelFunc
	done    chan struct{}
}

// egressUp starts e's proxy and waits, at most egressReadyTimeout, for the redirect; each call is paired with one egressDown once the VM process started with it ends.
func (a *Agent) egressUp(e *vmEntry) {
	// Under both locks, a push either lands in e.file first or its egressSync sees this process counted.
	a.mu.Lock()
	f := e.file
	a.egressMu.Lock()
	if a.egressUsers == nil {
		a.egressUsers = map[string]int{}
	}
	a.egressUsers[f.Spec.ID]++
	s := a.setEgressLocked(f)
	a.egressMu.Unlock()
	a.mu.Unlock()
	if s == nil {
		return
	}
	select {
	case <-s.ready:
	case <-time.After(egressReadyTimeout):
		slog.Warn("egress proxy not ready, starting without it", "vm", f.Spec.ID, "timeout", egressReadyTimeout)
	}
}

// egressDown stops the VM's proxy once no process started with egressUp is left; the count keeps a replaced instance exiting late from taking down its successor's proxy.
func (a *Agent) egressDown(id string) {
	a.egressMu.Lock()
	var s *egressSup
	if n := a.egressUsers[id] - 1; n > 0 {
		a.egressUsers[id] = n
	} else {
		delete(a.egressUsers, id)
		s = a.haltEgressLocked(id)
	}
	a.egressMu.Unlock()
	s.wait()
}

// egressSync hands a running VM's proxy the table and CAs reconcile just stored in e.file; old is e.file before.
func (a *Agent) egressSync(e *vmEntry, old VMFile) {
	a.mu.Lock()
	f, inst, paused := e.file, e.inst, e.paused
	id := f.Spec.ID
	var halted *egressSup
	a.egressMu.Lock()
	if a.egressUsers[id] > 0 {
		if f.Egress != nil {
			a.setEgressLocked(f)
		} else {
			halted = a.haltEgressLocked(id)
		}
	}
	a.egressMu.Unlock()
	a.mu.Unlock()
	halted.wait()
	cas := func(b *proto.Boot) map[string]string {
		if b == nil {
			return nil
		}
		return b.EgressCAs
	}
	// A paused guest cannot answer; its resume re-reads the seed, which carries the CAs.
	if !alive(inst) || paused || maps.Equal(cas(old.Boot), cas(f.Boot)) {
		return
	}
	go func() {
		// The guest drops CAs missing from a call, so calls run one at a time and each sends the newest set.
		e.trustMu.Lock()
		defer e.trustMu.Unlock()
		a.mu.Lock()
		latest := cas(e.file.Boot)
		a.mu.Unlock()
		ctx, cancel := context.WithTimeout(context.Background(), guestTrustTimeout)
		defer cancel()
		if err := inst.GuestTrust(ctx, latest); err != nil {
			slog.Error("egress ca trust failed", "vm", id, "err", err)
		}
	}()
}

// setEgressLocked starts f's proxy or passes it f's table; nil when f has no table or the node runs no proxies. a.egressMu must be held.
func (a *Agent) setEgressLocked(f VMFile) *egressSup {
	if f.Egress == nil {
		return nil
	}
	if a.Egress == nil {
		a.egressWarn.Do(func() { slog.Warn("vm has host-bound secrets, but this node runs no egress proxies", "vm", f.Spec.ID) })
		return nil
	}
	id := f.Spec.ID
	s := a.egress[id]
	if s != nil && !s.halted {
		if !reflect.DeepEqual(s.f.Egress, f.Egress) {
			select {
			case s.changed <- struct{}{}:
			default:
			}
		}
		s.f = f
		return s
	}
	ctx, stop := context.WithCancel(context.Background())
	next := &egressSup{f: f, prev: s, changed: make(chan struct{}, 1), ready: make(chan struct{}), stop: stop, done: make(chan struct{})}
	if a.egress == nil {
		a.egress = map[string]*egressSup{}
	}
	a.egress[id] = next
	go a.superviseEgress(ctx, next)
	return next
}

// haltEgressLocked stops the VM's proxy and returns it for wait; a.egressMu must be held.
func (a *Agent) haltEgressLocked(id string) *egressSup {
	s := a.egress[id]
	if s == nil || s.halted {
		return nil
	}
	s.halted = true
	s.stop()
	return s
}

// haltAllEgress stops every proxy and its redirect, so none outlives the node process.
func (a *Agent) haltAllEgress() {
	a.egressMu.Lock()
	var halted []*egressSup
	// Includes proxies an exited VM's egressDown is still waiting for.
	for id, s := range a.egress {
		a.haltEgressLocked(id)
		halted = append(halted, s)
	}
	a.egressMu.Unlock()
	for _, s := range halted {
		s.wait()
	}
}

func (s *egressSup) wait() {
	if s != nil {
		<-s.done
	}
}

func (a *Agent) egressFile(s *egressSup) VMFile {
	a.egressMu.Lock()
	defer a.egressMu.Unlock()
	return s.f
}

// superviseEgress keeps s's proxy running until it is halted, restarting it with backoff.
func (a *Agent) superviseEgress(ctx context.Context, s *egressSup) {
	defer func() {
		a.egressMu.Lock()
		if a.egress[s.f.Spec.ID] == s {
			delete(a.egress, s.f.Spec.ID)
		}
		a.egressMu.Unlock()
		close(s.done)
	}()
	s.prev.wait()
	for failures := 0; ctx.Err() == nil; failures++ {
		ranAt := time.Now()
		a.runEgress(ctx, s)
		if ctx.Err() != nil {
			return
		}
		if time.Since(ranAt) >= quickRun {
			failures = 0
		}
		select {
		case <-ctx.Done():
		case <-time.After(restartBackoff[min(failures, len(restartBackoff)-1)]):
		}
	}
}

// runEgress runs one proxy process until it exits or ctx ends; the VM is redirected to it only while it is ready.
func (a *Agent) runEgress(ctx context.Context, s *egressSup) {
	f := a.egressFile(s)
	id := f.Spec.ID
	proc, err := a.Egress.Spawn(f)
	if err != nil {
		slog.Error("egress proxy failed", "vm", id, "err", err)
		return
	}
	defer func() {
		proc.Kill()
		<-proc.Done()
	}()
	go a.forwardUsage(proc)
	if err := proc.Send(*f.Egress); err != nil {
		slog.Error("egress proxy config failed", "vm", id, "err", err)
		return
	}
	select {
	case <-proc.Ready():
	case <-proc.Done():
		slog.Error("egress proxy exited before it was ready", "vm", id)
		return
	case <-ctx.Done():
		return
	}
	if err := a.Egress.Redirect(f); err != nil {
		slog.Error("egress redirect failed", "vm", id, "err", err)
		return
	}
	defer func() {
		if err := a.Egress.Unredirect(f); err != nil {
			slog.Error("egress unredirect failed", "vm", id, "err", err)
		}
	}()
	s.once.Do(func() { close(s.ready) })
	for {
		select {
		case <-proc.Done():
			slog.Warn("egress proxy exited", "vm", id)
			return
		case <-ctx.Done():
			return
		case <-s.changed:
			if err := proc.Send(*a.egressFile(s).Egress); err != nil {
				slog.Error("egress proxy config failed", "vm", id, "err", err)
				return
			}
		}
	}
}

// forwardUsage sends proc's usage reports to the hub, batching those already queued, until proc exits; reports while disconnected are lost, the hub's polling catches up.
func (a *Agent) forwardUsage(proc EgressProc) {
	for {
		var batch []proto.UsageObservation
		select {
		case <-proc.Done():
			return
		case o := <-proc.Usage():
			batch = append(batch, o)
		}
		for n := len(proc.Usage()); n > 0; n-- {
			batch = append(batch, <-proc.Usage())
		}
		a.mu.Lock()
		send := a.send
		a.mu.Unlock()
		if send == nil {
			continue
		}
		if err := send(proto.Message{Type: "usage", Usage: batch}); err != nil {
			slog.Warn("usage report failed", "err", err)
		}
	}
}

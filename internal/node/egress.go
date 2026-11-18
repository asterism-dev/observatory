package node

import (
	"context"
	"log"
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
}

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

// egressUp starts f's proxy and waits, at most egressReadyTimeout, for the redirect; each call is paired with one egressDown once the VM process started with it ends.
func (a *Agent) egressUp(f VMFile) {
	id := f.Spec.ID
	a.egressMu.Lock()
	if a.egressUsers == nil {
		a.egressUsers = map[string]int{}
	}
	// Counted per process: a replaced instance exiting late must not take down its successor's proxy.
	a.egressUsers[id]++
	s := a.setEgressLocked(f)
	a.egressMu.Unlock()
	if s == nil {
		return
	}
	select {
	case <-s.ready:
	case <-time.After(egressReadyTimeout):
		log.Printf("vm %s: egress proxy not ready after %s, starting without it", id, egressReadyTimeout)
	}
}

// egressDown stops the VM's proxy once no process started with egressUp is left.
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
	a.mu.Unlock()
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
		ctx, cancel := context.WithTimeout(context.Background(), guestTrustTimeout)
		defer cancel()
		if err := inst.GuestTrust(ctx, cas(f.Boot)); err != nil {
			log.Printf("vm %s: egress ca trust: %v", id, err)
		}
	}()
}

// setEgressLocked starts f's proxy or passes it f's table; nil when f has no table or the node runs no proxies. a.egressMu must be held.
func (a *Agent) setEgressLocked(f VMFile) *egressSup {
	if f.Egress == nil {
		return nil
	}
	if a.Egress == nil {
		a.egressWarn.Do(func() { log.Printf("vm %s: has host-bound secrets, but this node runs no egress proxies", f.Spec.ID) })
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
		log.Printf("vm %s: egress proxy: %v", id, err)
		return
	}
	defer func() {
		proc.Kill()
		<-proc.Done()
	}()
	if err := proc.Send(*f.Egress); err != nil {
		log.Printf("vm %s: egress proxy config: %v", id, err)
		return
	}
	select {
	case <-proc.Ready():
	case <-proc.Done():
		log.Printf("vm %s: egress proxy exited before it was ready", id)
		return
	case <-ctx.Done():
		return
	}
	if err := a.Egress.Redirect(f); err != nil {
		log.Printf("vm %s: egress redirect: %v", id, err)
		return
	}
	defer func() {
		if err := a.Egress.Unredirect(f); err != nil {
			log.Printf("vm %s: egress unredirect: %v", id, err)
		}
	}()
	s.once.Do(func() { close(s.ready) })
	for {
		select {
		case <-proc.Done():
			log.Printf("vm %s: egress proxy exited", id)
			return
		case <-ctx.Done():
			return
		case <-s.changed:
			if err := proc.Send(*a.egressFile(s).Egress); err != nil {
				log.Printf("vm %s: egress proxy config: %v", id, err)
			}
		}
	}
}

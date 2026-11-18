package node

import (
	"context"
	"log"
	"time"

	"github.com/asterism-dev/observatory/internal/proto"
)

func suspended(f VMFile) bool { return f.Suspended && f.Hibernated }

// watchIdleLocked (re)starts e's idle watch while e runs with auto-suspend on; a.mu must be held.
func (a *Agent) watchIdleLocked(e *vmEntry) {
	if e.idleStop != nil {
		e.idleStop()
		e.idleStop = nil
	}
	running := e.status.State == proto.StateRunning || e.status.State == proto.StatePaused
	if e.ctx == nil || e.file.AutoSuspendMinutes <= 0 || !running || !alive(e.inst) {
		return
	}
	ctx, stop := context.WithCancel(e.ctx)
	e.idleStop = stop
	go a.suspendLoop(ctx, e)
}

// suspendLoop hibernates e as suspended once its taps were idle for its auto-suspend minutes, then waits for traffic to resume it.
func (a *Agent) suspendLoop(ctx context.Context, e *vmEntry) {
	a.mu.Lock()
	f := e.file
	a.mu.Unlock()
	for {
		if err := a.Provider.WaitIdle(ctx, f, time.Duration(f.AutoSuspendMinutes)*time.Minute); err != nil {
			if ctx.Err() == nil {
				log.Printf("vm %s: idle watch: %v", f.Spec.ID, err)
			}
			return
		}
		if a.suspend(ctx, e) {
			break
		}
	}
	a.waitWake(e)
}

// suspend hibernates e as suspended; it skips (the next idle period retries) while another operation runs or e must not sleep now.
func (a *Agent) suspend(ctx context.Context, e *vmEntry) bool {
	if !e.op.TryLock() {
		return false
	}
	defer e.op.Unlock()
	a.mu.Lock()
	ok := ctx.Err() == nil && !a.stopping && !e.paused && e.file.Power == proto.PowerRunning && alive(e.inst) && a.vms[e.file.Spec.ID] == e
	if ok {
		e.file.Suspended = true
	}
	a.mu.Unlock()
	if !ok {
		return false
	}
	err := a.hibernate(e)
	if err == nil {
		return true
	}
	log.Printf("vm %s: auto-suspend: %v", e.file.Spec.ID, err)
	a.mu.Lock()
	e.file.Suspended = e.file.Hibernated
	a.mu.Unlock()
	return false
}

// waitWake resumes suspended e on traffic or a wake request; a desired change, delete or node stop ends e.ctx and leaves e suspended.
func (a *Agent) waitWake(e *vmEntry) {
	for i := 0; ; i++ {
		ctx, wake := context.WithCancel(e.ctx)
		a.mu.Lock()
		e.wake = wake
		f, woken := e.file, e.woken
		a.mu.Unlock()
		var err error
		if !woken {
			err = a.Provider.WaitWake(ctx, f)
		}
		woken = ctx.Err() != nil
		wake()
		if e.ctx.Err() != nil {
			return
		}
		if err == nil || woken {
			break
		}
		log.Printf("vm %s: wait for traffic: %v", f.Spec.ID, err)
		a.reportEntry(e, proto.VMStatus{ID: f.Spec.ID, State: proto.StateSuspended, Error: "wait for traffic: " + err.Error()})
		select {
		case <-e.ctx.Done():
			return
		case <-time.After(restartBackoff[min(i, len(restartBackoff)-1)]):
		}
	}
	e.op.Lock()
	defer e.op.Unlock()
	a.mu.Lock()
	f := e.file
	ok := e.ctx.Err() == nil && !a.stopping && a.vms[f.Spec.ID] == e && suspended(f)
	a.mu.Unlock()
	if !ok {
		return
	}
	f.Suspended = false
	e.cancel()
	a.startVM(e.root, f, false)
}

// syncSuspend applies d's auto-suspend minutes and wake requests to e; neither comes with a new generation.
func (a *Agent) syncSuspend(e *vmEntry, d proto.DesiredVM) {
	a.mu.Lock()
	minutes := d.AutoSuspendMinutes != e.file.AutoSuspendMinutes
	if !minutes && d.WakeSeq == e.file.WakeSeq {
		a.mu.Unlock()
		return
	}
	if d.WakeSeq > e.file.WakeSeq && suspended(e.file) {
		e.woken = true
		if e.wake != nil {
			e.wake()
		}
	}
	e.file.AutoSuspendMinutes, e.file.WakeSeq = d.AutoSuspendMinutes, d.WakeSeq
	if minutes {
		a.watchIdleLocked(e)
	}
	f := e.file
	a.mu.Unlock()
	select {
	case <-e.booted:
	default:
		return
	}
	// A running operation saves e.file itself; an older copy saved here could land after it.
	if e.op.TryLock() {
		if err := SaveVM(a.VMsDir, f); err != nil {
			log.Printf("vm %s: save: %v", f.Spec.ID, err)
		}
		e.op.Unlock()
	}
}

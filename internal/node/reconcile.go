package node

import (
	"context"
	"log"
	"math"
	"slices"
	"sync"
	"time"

	"github.com/asterism-dev/observatory/internal/proto"
)

// reconcile converges the node's VMs to desired, the hub's full list for this node.
// seq is the list's Message.Seq, echoed to the hub once the list is applied.
func (a *Agent) reconcile(ctx context.Context, seq uint64, desired []proto.DesiredVM) {
	a.reconcileMu.Lock()
	defer a.reconcileMu.Unlock()

	want := make(map[string]bool, len(desired))
	for _, d := range desired {
		want[d.Spec.ID] = true
	}
	a.mu.Lock()
	var gone []*vmEntry
	for id, e := range a.vms {
		if !want[id] {
			gone = append(gone, e)
			delete(a.vms, id)
			if a.dying == nil {
				a.dying = map[*vmEntry]bool{}
			}
			a.dying[e] = true
		}
	}
	deferred := a.deferred
	a.deferred = nil
	a.mu.Unlock()
	var wg sync.WaitGroup
	for _, e := range gone {
		a.gcSnapshots(e.file.Spec.ID, nil)
		if a.slowDestroy(e) {
			// It waits on Firecracker, the guest or storage; the disks stay in use until it is done.
			go func() { a.destroy(ctx, e); a.sendDisks() }()
			continue
		}
		wg.Add(1)
		go func() { defer wg.Done(); a.destroy(ctx, e) }()
	}
	// Removals finish first so their resources are free before anything new is prepared.
	wg.Wait()
	a.resumeDeferred(ctx, deferred, desired)

	for _, d := range desired {
		a.mu.Lock()
		e, ok := a.vms[d.Spec.ID]
		var gen uint64
		var due bool
		var old VMFile
		if ok {
			gen, due, old = e.file.Generation, restoreDue(e.file, d), e.file
			// Key and variable changes keep the generation; the new Boot applies at the next (re)start.
			if !e.placeholder && d.Generation <= gen {
				e.file.Boot = d.Boot
			}
			// The proxy table and a fresh pull token apply at once, whatever the generation.
			if !e.placeholder {
				e.file.Egress = d.Egress
				if e.file.Spec.Image != nil && d.Spec.Image != nil {
					e.file.Spec.Image = d.Spec.Image
					a.wakeImageWaitersLocked()
				}
				// A hub that failed to read the integrations leaves the last known tokens in place.
				if !d.IntegrationsUnresolved || old.Boot == nil || old.IntegrationsUnresolved {
					e.file.Integrations, e.file.IntegrationsUnresolved = d.Integrations, d.IntegrationsUnresolved
				}
			}
		}
		a.mu.Unlock()
		if ok && !e.placeholder {
			a.egressSync(e, old)
			a.syncSuspend(e, d)
		}
		switch {
		case !ok || e.placeholder && !d.Provisioned && d.Generation > gen:
			a.createDesired(ctx, d)
		case d.Generation > gen || due:
			a.updateDesired(ctx, e, d)
		}
		if ok && !e.placeholder {
			a.syncSnapshots(d)
		}
	}

	a.mu.Lock()
	a.appliedSeq = seq
	a.mu.Unlock()
	a.sendDisks()
}

func (a *Agent) sendDisks() {
	a.mu.Lock()
	m, send := proto.Message{Type: "disks", DisksInUse: a.disksInUse(), AppliedSeq: a.appliedSeq}, a.send
	a.mu.Unlock()
	if send != nil {
		if err := send(m); err != nil {
			log.Printf("disks in use: %v", err)
		}
	}
}

// resumeDeferred boots the restored VMs with disks that the hub still wants and deletes the rest.
func (a *Agent) resumeDeferred(ctx context.Context, deferred []VMFile, desired []proto.DesiredVM) {
	byID := make(map[string]proto.DesiredVM, len(desired))
	for _, d := range desired {
		byID[d.Spec.ID] = d
	}
	for _, f := range deferred {
		d, ok := byID[f.Spec.ID]
		gen := d.Generation
		f.Boot, f.Egress, f.Integrations, f.IntegrationsUnresolved = d.Boot, d.Egress, d.Integrations, d.IntegrationsUnresolved
		switch {
		case !ok && f.Hibernated:
			booted := make(chan struct{})
			close(booted)
			e := &vmEntry{file: f, cancel: func() {}, booted: booted}
			a.mu.Lock()
			if a.dying == nil {
				a.dying = map[*vmEntry]bool{}
			}
			a.dying[e] = true
			a.mu.Unlock()
			go func() { a.destroy(ctx, e); a.sendDisks() }()
		case !ok:
			if err := DeleteVMDir(a.VMsDir, f.Spec.ID); err != nil {
				log.Printf("vm %s: delete dir: %v", f.Spec.ID, err)
			}
		case gen > f.Generation:
			// The attachments may have changed while the node was down; updateDesired applies them before booting.
			booted := make(chan struct{})
			close(booted)
			a.mu.Lock()
			a.vms[f.Spec.ID] = &vmEntry{file: f, status: proto.VMStatus{ID: f.Spec.ID, State: restingState(f)}, cancel: func() {}, booted: booted}
			a.mu.Unlock()
		default:
			a.startVM(ctx, f, f.NeedsPrepare)
		}
	}
}

// slowDestroy reports whether destroying e waits on a boot, a running operation or a resume to shut down.
func (a *Agent) slowDestroy(e *vmEntry) bool {
	select {
	case <-e.booted:
	default:
		return true
	}
	if !e.op.TryLock() {
		return true
	}
	e.op.Unlock()
	a.mu.Lock()
	defer a.mu.Unlock()
	return e.file.Hibernated
}

func (a *Agent) destroy(ctx context.Context, e *vmEntry) {
	id := e.file.Spec.ID
	e.cancel()
	<-e.booted
	e.op.Lock()
	defer e.op.Unlock()
	gone := func() {
		a.mu.Lock()
		delete(a.dying, e)
		a.mu.Unlock()
	}
	a.mu.Lock()
	hibernated := e.file.Hibernated
	a.mu.Unlock()
	// A hibernated VM has no live instance; with disks it must still shut down cleanly, so it stays until storage is back.
	if hibernated {
		if err := a.endHibernationWait(ctx, e, true, math.MaxUint64); ctx.Err() != nil {
			// The node is stopping; the VM dir and its state stay, and so do its disks.
			return
		} else if err != nil {
			log.Printf("vm %s: giving up on its memory state: %v", id, err)
		}
	}
	if inst := a.instance(e); inst != nil {
		e.stopRequested.Store(true)
		// Disks outlive the VM, so the guest must flush them; Stop powers off hard if the guest does not shut down.
		if len(e.file.Spec.Attachments) > 0 {
			if err := inst.Stop(); err != nil {
				log.Printf("vm %s: stop: %v", id, err)
				if err := inst.Kill(); err != nil {
					log.Printf("vm %s: kill: %v", id, err)
				}
			}
		} else if err := inst.Kill(); err != nil {
			log.Printf("vm %s: kill: %v", id, err)
		}
		select {
		case <-inst.Done():
			gone()
		case <-time.After(killWait):
			log.Printf("vm %s: still running %s after power-off; its disks stay in use until it exits", id, killWait)
			go func() {
				<-inst.Done()
				gone()
				a.sendDisks()
			}()
		}
	} else {
		gone()
	}
	if err := DeleteVMDir(a.VMsDir, id); err != nil {
		log.Printf("vm %s: delete dir: %v", id, err)
	}
}

func (a *Agent) createDesired(ctx context.Context, d proto.DesiredVM) {
	if d.Provisioned {
		a.placeholder(d, proto.ErrVMDirMissing)
		return
	}
	if d.Fork != nil {
		f, err := a.forkLocal(d)
		f.Egress, f.Integrations, f.IntegrationsUnresolved = d.Egress, d.Integrations, d.IntegrationsUnresolved
		if err != nil {
			a.placeholder(d, err.Error())
			return
		}
		a.startVM(ctx, f, false)
		return
	}
	f := VMFile{Spec: d.Spec, MAC: RandomMAC(), Power: d.Power, StopMode: d.StopMode, Generation: d.Generation, NeedsPrepare: true, Boot: d.Boot,
		Egress: d.Egress, Integrations: d.Integrations, IntegrationsUnresolved: d.IntegrationsUnresolved, AutoSuspendMinutes: d.AutoSuspendMinutes, WakeSeq: d.WakeSeq}
	if err := SaveVM(a.VMsDir, f); err != nil {
		a.placeholder(d, err.Error())
		return
	}
	a.startVM(ctx, f, true)
}

// placeholder records an unbootable VM so its error reaches the hub.
func (a *Agent) placeholder(d proto.DesiredVM, msg string) {
	booted := make(chan struct{})
	close(booted)
	e := &vmEntry{
		file:   VMFile{Spec: d.Spec, Power: d.Power, StopMode: d.StopMode, Generation: d.Generation},
		cancel: func() {}, booted: booted, placeholder: true,
	}
	a.mu.Lock()
	a.vms[d.Spec.ID] = e
	a.mu.Unlock()
	a.report(proto.VMStatus{ID: d.Spec.ID, State: proto.StateError, Error: msg})
}

func (a *Agent) updateDesired(ctx context.Context, e *vmEntry, d proto.DesiredVM) {
	if e.placeholder {
		a.mu.Lock()
		e.file.Spec, e.file.Power, e.file.StopMode, e.file.Generation, e.file.Boot = d.Spec, d.Power, d.StopMode, d.Generation, d.Boot
		st := e.status
		a.mu.Unlock()
		a.report(st)
		return
	}
	if !a.acquire(ctx, e, d) {
		return
	}
	op := &e.op
	unlock := true
	defer func() {
		if unlock {
			op.Unlock()
		}
	}()
	a.mu.Lock()
	f := e.file
	a.mu.Unlock()
	// The VM stays as the restore left it until the hub has restored its disks too.
	if d.Restore != nil && (restoreDue(f, d) || !d.Restore.Ready) {
		if restoreDue(f, d) {
			unlock = false
			go func() {
				defer op.Unlock()
				if err := a.restoreLocal(e, *d.Restore); err != nil {
					log.Printf("vm %s: restore: %v", f.Spec.ID, err)
					a.mu.Lock()
					st := e.status
					a.mu.Unlock()
					st.Error = "restore: " + err.Error()
					a.reportEntry(e, st)
				}
			}()
		}
		return
	}
	// A new Boot takes effect at the next start, which reseeds; it does not restart the VM.
	f.Power, f.StopMode, f.Generation, f.Boot = d.Power, d.StopMode, d.Generation, d.Boot
	// A desired change wakes a suspended VM, or leaves it hibernated, stopped or paused as asked.
	f.Suspended = false
	var reseedErr error
	halted := !slices.Equal(f.Spec.NICs, d.Spec.NICs) || !slices.Equal(f.Spec.Attachments, d.Spec.Attachments)
	graceful := d.Power != proto.PowerStopped || d.StopMode != proto.StopHard
	if f.Hibernated && len(f.Spec.Attachments) > 0 && graceful && (halted || d.Power == proto.PowerStopped) {
		// Resuming to shut down waits on storage and the guest; d is applied again once the state is gone.
		unlock = false
		// A restart waiting for storage must not resume or cold-boot the VM before d applies.
		e.stopRequested.Store(true)
		e.cancel()
		go func() {
			defer op.Unlock()
			if err := a.endHibernationWait(ctx, e, true, d.Generation); err == nil {
				a.queue(ctx, e, d)
			}
		}()
		return
	}
	if halted {
		e = a.halt(ctx, e, graceful)
		f.Hibernated = e.file.Hibernated
		oldNICs, oldAtts := f.Spec.NICs, f.Spec.Attachments
		f.Spec.NICs, f.Spec.Attachments = d.Spec.NICs, d.Spec.Attachments
		if !f.NeedsPrepare {
			// Keeping the old values on failure leaves a difference, so the next generation retries the reseed.
			if reseedErr = a.Provider.Reseed(ctx, f, VMDir(a.VMsDir, f.Spec.ID)); reseedErr != nil {
				f.Spec.NICs, f.Spec.Attachments = oldNICs, oldAtts
			}
		}
	}
	if err := SaveVM(a.VMsDir, f); err != nil {
		log.Printf("vm %s: save: %v", f.Spec.ID, err)
		if halted {
			a.report(proto.VMStatus{ID: f.Spec.ID, State: proto.StateError, Error: "save: " + err.Error()})
		}
		return
	}
	a.mu.Lock()
	old := e.file
	e.file = f
	inst, st := e.inst, e.status
	a.mu.Unlock()
	if reseedErr != nil {
		a.report(proto.VMStatus{ID: f.Spec.ID, State: proto.StateError, Error: "reseed: " + reseedErr.Error()})
		return
	}
	live := inst != nil
	if live {
		select {
		case <-inst.Done():
			live = false
		default:
		}
	}
	switch {
	case f.Power == proto.PowerHibernated && live:
		// Writing the memory out takes long; reconcile goes on while the hibernate keeps e.op.
		unlock = false
		go func() {
			defer op.Unlock()
			if err := a.hibernate(e); err != nil {
				a.mu.Lock()
				st := e.status
				a.mu.Unlock()
				st.Error = "hibernate: " + err.Error()
				a.reportEntry(e, st)
			}
		}()
	case f.Power == proto.PowerHibernated:
		e.cancel()
		a.report(proto.VMStatus{ID: f.Spec.ID, State: restingState(f)})
	case f.Power != proto.PowerStopped && !live:
		e.cancel()
		a.startVM(ctx, f, f.NeedsPrepare)
	case f.Power == proto.PowerStopped && live:
		e.stopRequested.Store(true)
		stop := inst.Stop
		if f.StopMode == proto.StopHard {
			stop = inst.Kill
		}
		if err := stop(); err != nil {
			log.Printf("vm %s: stop: %v", f.Spec.ID, err)
		}
		a.report(proto.VMStatus{ID: f.Spec.ID, State: proto.StateStopped})
	case f.Power == proto.PowerStopped && f.Hibernated:
		e.cancel()
		if err := a.endHibernation(ctx, e, graceful); err != nil {
			log.Printf("vm %s: end hibernation: %v", f.Spec.ID, err)
		}
		f.Hibernated = false
		if err := SaveVM(a.VMsDir, f); err != nil {
			log.Printf("vm %s: save: %v", f.Spec.ID, err)
		}
		a.mu.Lock()
		e.file = f
		a.mu.Unlock()
		a.report(proto.VMStatus{ID: f.Spec.ID, State: proto.StateStopped})
	case f.Power == proto.PowerStopped:
		e.cancel() // ends a pending restart backoff
		if st.State == proto.StateHibernated {
			// The hibernation was ended (resumed and shut down) before this update was replayed.
			st = proto.VMStatus{ID: f.Spec.ID, State: proto.StateStopped}
		}
		a.report(st)
	default:
		a.egressSync(e, old)
		a.syncPause(e, inst)
	}
}

// acquire takes e.op to apply d now, or queues d while e boots or runs another operation.
func (a *Agent) acquire(ctx context.Context, e *vmEntry, d proto.DesiredVM) bool {
	select {
	case <-e.booted:
		if e.op.TryLock() {
			return true
		}
	default:
	}
	a.queue(ctx, e, d)
	return false
}

// halt stops e's instance and swaps in an idle stopped entry, so e's boot goroutine neither restarts it nor reports over later updates.
func (a *Agent) halt(ctx context.Context, e *vmEntry, graceful bool) *vmEntry {
	e.stopRequested.Store(true)
	if inst := a.instance(e); inst != nil {
		select {
		case <-inst.Done():
		default:
			if err := inst.Stop(); err != nil {
				log.Printf("vm %s: stop: %v", e.file.Spec.ID, err)
				if err := inst.Kill(); err != nil {
					log.Printf("vm %s: kill: %v", e.file.Spec.ID, err)
				}
			}
			<-inst.Done()
		}
	}
	a.mu.Lock()
	hibernated := e.file.Hibernated
	a.mu.Unlock()
	// Attached disks change, so the memory state cannot be resumed against them; a VM with disks was resumed and shut down before.
	if hibernated {
		if err := a.endHibernation(ctx, e, graceful); err != nil {
			log.Printf("vm %s: end hibernation: %v", e.file.Spec.ID, err)
		}
	}
	a.mu.Lock()
	f := e.file
	a.mu.Unlock()
	// Cancelling only after exit keeps a pending WaitIP from reporting the cancellation as an error.
	e.cancel()
	booted := make(chan struct{})
	close(booted)
	a.mu.Lock()
	defer a.mu.Unlock()
	idle := &vmEntry{file: f, status: proto.VMStatus{ID: e.file.Spec.ID, State: proto.StateStopped}, cancel: func() {}, booted: booted}
	a.vms[idle.file.Spec.ID] = idle
	return idle
}

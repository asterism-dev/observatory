package node

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"github.com/coder/websocket"
	"github.com/hashicorp/yamux"

	"github.com/asterism-dev/observatory/internal/chassisconf"
	"github.com/asterism-dev/observatory/internal/proto"
)

const (
	reconnectDelay = 2 * time.Second
	ipTimeout      = 120 * time.Second

	underlayMTU = 1420

	defaultDiskRefresh = 60 * time.Second
)

var (
	restartBackoff = []time.Duration{1 * time.Second, 2 * time.Second, 5 * time.Second, 15 * time.Second, 30 * time.Second}
	quickRun       = 60 * time.Second
	killWait       = 30 * time.Second
	// stopBudget leaves systemd's TimeoutStopSec=300 room to kill what is still saving.
	stopBudget = 240 * time.Second

	guestResumeTimeout = 30 * time.Second
)

type Agent struct {
	HubURL, Credential, NodeID, VMsDir string
	CPUs, MemoryMB                     int
	Provider                           Provider
	Config                             json.RawMessage
	Machine                            *proto.Machine
	ChassisKey                         string // public, sent in Register
	ChassisPrivateKey                  string
	ChassisCSR                         string // OVN client cert request, sent in Register
	OVNKeyFile                         string
	Version                            string

	diskRefresh time.Duration // zero means defaultDiskRefresh

	chassisMu       sync.Mutex
	chassisPending  *chassisconf.Config // newest config not yet applied; older ones are superseded
	chassisRunning  bool
	chassisStopping bool
	chassisWG       sync.WaitGroup

	reconcileMu sync.Mutex

	mu       sync.Mutex
	vms      map[string]*vmEntry
	send     func(proto.Message) error // nil while disconnected
	stopping bool
	// deferred holds restored VMs with disks; they boot only once the hub's first desired list still wants them.
	deferred   []VMFile
	appliedSeq uint64
	// dying holds removed entries whose instance may still run; their disks stay in use until it exits.
	dying    map[*vmEntry]bool
	snapJobs map[snapKey]*snapshotJob
}

type vmEntry struct {
	file   VMFile
	status proto.VMStatus
	inst   Instance
	cancel context.CancelFunc
	booted chan struct{} // closed once boot has finished starting (or failed)
	// placeholder entries stand in for provisioned VMs whose directory is gone; they never boot.
	placeholder   bool
	stopRequested atomic.Bool // set before node-initiated Stop/Kill so the exit is not treated as a crash
	// op serializes power operations on the VM; a hibernate holds it from its own goroutine.
	op     sync.Mutex
	paused bool // guarded by Agent.mu
	// pending is the newest desired that arrived while op was taken; a waiting goroutine applies it.
	pending *proto.DesiredVM
	waiting bool
	// ctx is the boot context, which cancel ends; root is the agent's, for a resumed successor.
	ctx, root context.Context
	// idleStop ends the idle watch; wake ends a suspended VM's WaitWake to resume it, as does woken set before it waits. Guarded by Agent.mu.
	idleStop, wake context.CancelFunc
	woken          bool
	wakeDone       chan struct{} // closed once a cancelled WaitWake has removed its taps
}

func (a *Agent) Run(ctx context.Context) error {
	a.vms = map[string]*vmEntry{}
	if err := a.restoreAll(ctx); err != nil {
		return err
	}
	for {
		if err := a.connect(ctx); err != nil && ctx.Err() == nil {
			log.Printf("hub connection: %v", err)
		}
		select {
		case <-ctx.Done():
			a.stopAll()
			return ctx.Err()
		case <-time.After(reconnectDelay):
		}
	}
}

func (a *Agent) restoreAll(ctx context.Context) error {
	files, err := LoadVMs(a.VMsDir)
	if err != nil {
		return err
	}
	for _, f := range files {
		if len(f.Spec.Attachments) > 0 {
			a.mu.Lock()
			a.deferred = append(a.deferred, f)
			a.mu.Unlock()
			continue
		}
		a.startVM(ctx, f, f.NeedsPrepare)
	}
	return nil
}

func (a *Agent) connect(ctx context.Context) error {
	c, _, err := websocket.Dial(ctx, a.HubURL, &websocket.DialOptions{
		HTTPHeader: http.Header{"Authorization": {"Bearer " + a.Credential}},
	})
	if err != nil {
		return err
	}
	cfg := yamux.DefaultConfig()
	cfg.LogOutput = io.Discard
	sess, err := yamux.Client(websocket.NetConn(ctx, c, websocket.MessageBinary), cfg)
	if err != nil {
		return err
	}
	defer sess.Close()
	ctrl, err := sess.Open()
	if err != nil {
		return err
	}

	var encMu sync.Mutex
	enc := proto.NewEncoder(ctrl)
	send := func(m proto.Message) error {
		encMu.Lock()
		defer encMu.Unlock()
		return enc.Encode(m)
	}
	// Register and installing send happen under a.mu so no status update slips between them.
	a.mu.Lock()
	err = send(proto.Message{Type: "register", Register: a.registerMsg()})
	if err == nil {
		a.send = send
	}
	a.mu.Unlock()
	if err != nil {
		return err
	}
	log.Printf("connected to hub %s", a.HubURL)
	connCtx, stopRefresh := context.WithCancel(ctx)
	go a.refreshDisksLoop(connCtx)
	defer func() {
		stopRefresh()
		a.mu.Lock()
		a.send = nil
		a.mu.Unlock()
	}()

	dec := json.NewDecoder(ctrl)
	for {
		var m proto.Message
		if err := dec.Decode(&m); err != nil {
			return err
		}
		switch {
		case m.Type == "desired":
			a.reconcile(ctx, m.Seq, m.Desired)
		case m.Type == "underlay" && m.Underlay != nil:
			go a.startChassis(ctx, *m.Underlay)
		}
	}
}

func (a *Agent) startChassis(ctx context.Context, u proto.Underlay) {
	c := chassisconf.Config{
		Name: a.NodeID, PrivateKey: a.ChassisPrivateKey, Address: u.Address, HubKey: u.HubKey, Endpoint: u.Endpoint,
		SBRemote: u.SBRemote, CACert: u.CACert, Cert: u.Cert, KeyFile: a.OVNKeyFile, MTU: underlayMTU,
	}
	a.chassisMu.Lock()
	if a.chassisStopping {
		a.chassisMu.Unlock()
		return
	}
	a.chassisPending = &c
	if a.chassisRunning {
		a.chassisMu.Unlock()
		return
	}
	a.chassisRunning = true
	a.chassisWG.Add(1)
	defer a.chassisWG.Done()
	for a.chassisPending != nil {
		next := *a.chassisPending
		a.chassisPending = nil
		a.chassisMu.Unlock()
		if err := a.Provider.StartChassis(ctx, next); err != nil {
			log.Printf("chassis: %v", err)
		}
		a.chassisMu.Lock()
	}
	a.chassisRunning = false
	a.chassisMu.Unlock()
}

func (a *Agent) registerMsg() *proto.Register {
	reg := &proto.Register{NodeID: a.NodeID, ChassisKey: a.ChassisKey, ChassisCSR: a.ChassisCSR, Provider: a.Provider.Name(), CPUs: a.CPUs, MemoryMB: a.MemoryMB, Config: a.Config, Machine: a.Machine,
		DisksInUse: a.disksInUse(), AppliedSeq: a.appliedSeq, Version: a.Version}
	reg.Hostname, _ = os.Hostname()
	for _, e := range a.vms {
		st := e.status
		st.Disks = a.disks(e.file)
		reg.VMs = append(reg.VMs, st)
	}
	return reg
}

// disksInUse lists the disk IDs local VMs still use; a.mu must be held.
func (a *Agent) disksInUse() []string {
	var ids []string
	for _, e := range a.vms {
		for _, at := range e.file.Spec.Attachments {
			ids = append(ids, at.DiskID)
		}
	}
	for e := range a.dying {
		for _, at := range e.file.Spec.Attachments {
			ids = append(ids, at.DiskID)
		}
	}
	for _, f := range a.deferred {
		for _, at := range f.Spec.Attachments {
			ids = append(ids, at.DiskID)
		}
	}
	slices.Sort(ids)
	return slices.Compact(ids)
}

// storageError is a boot refused because storage is not reachable yet, or a hibernated VM's vm.json cannot be saved; boot retries it without giving up.
type storageError struct{ error }

// checkStorage dials each NBD server f attaches, so a VM whose disks are unreachable fails instead of booting.
// Its error is the full status message.
func checkStorage(f VMFile) error {
	if err := dialStorage(f); err != nil {
		return storageError{err}
	}
	return nil
}

func dialStorage(f VMFile) error {
	dialed := map[string]bool{}
	for _, at := range f.Spec.Attachments {
		if at.URL == "" {
			return fmt.Errorf("storage not ready: disk %s has no NBD URL yet", at.DiskID)
		}
		u, err := url.Parse(at.URL)
		if err != nil {
			return fmt.Errorf("storage unreachable: %w", err)
		}
		if dialed[u.Host] {
			continue
		}
		dialed[u.Host] = true
		c, err := net.DialTimeout("tcp", u.Host, 3*time.Second)
		if err != nil {
			return fmt.Errorf("storage unreachable: %w", err)
		}
		c.Close()
	}
	return nil
}

func (a *Agent) startVM(ctx context.Context, f VMFile, prepare bool) {
	state := proto.StateStarting
	if f.Power == proto.PowerStopped || f.Power == proto.PowerHibernated || suspended(f) {
		state = restingState(f)
	}
	bootCtx, cancel := context.WithCancel(ctx)
	e := &vmEntry{file: f, status: proto.VMStatus{ID: f.Spec.ID, State: state}, booted: make(chan struct{}), ctx: bootCtx, root: ctx}
	// Whoever cancels goes on to start the VM, whose taps the cancelled WaitWake must not remove after the fact.
	e.cancel = func() {
		cancel()
		a.mu.Lock()
		done := e.wakeDone
		a.mu.Unlock()
		if done != nil {
			<-done
		}
	}
	a.mu.Lock()
	a.vms[f.Spec.ID] = e
	a.mu.Unlock()
	a.report(e.status)
	go a.boot(bootCtx, e, f, prepare)
}

// boot prepares (if asked) and starts f; a stopped f is only prepared. Exited instances are restarted while f should run.
func (a *Agent) boot(ctx context.Context, e *vmEntry, f VMFile, prepare bool) {
	backoff, quick := restartBackoff, quickRun
	id, dir := f.Spec.ID, VMDir(a.VMsDir, f.Spec.ID)
	report := func(st proto.VMStatus) { a.reportEntry(e, st) }
	fail := func(step string, err error) {
		close(e.booted)
		report(proto.VMStatus{ID: id, State: proto.StateError, Error: fmt.Sprintf("%s: %v", step, err)})
	}
	if prepare {
		if err := a.prepare(ctx, e, f); err != nil {
			fail("prepare", err)
			return
		}
	}
	if f.Power == proto.PowerStopped || f.Power == proto.PowerHibernated {
		close(e.booted)
		report(proto.VMStatus{ID: id, State: restingState(f)})
		return
	}
	if suspended(f) {
		close(e.booted)
		report(proto.VMStatus{ID: id, State: restingState(f)})
		a.waitWake(e)
		return
	}
	var inst Instance
	var note string
	// A new export listens only after the storage server's next poll, so an unreachable storage is retried like an exit.
	reason := checkStorage(f)
	if reason == nil {
		// A same-generation push (new keys or variables) may have arrived while preparing.
		a.mu.Lock()
		f.Boot = e.file.Boot
		a.mu.Unlock()
		var err error
		if inst, note, err = a.start(ctx, e, f, dir); err != nil {
			if !errors.As(err, new(storageError)) {
				fail("start", err)
				return
			}
			reason = err
		} else {
			a.mu.Lock()
			e.inst = inst
			a.mu.Unlock()
		}
	}
	close(e.booted)
	if reason != nil {
		report(proto.VMStatus{ID: id, State: proto.StateError, Error: reason.Error()})
	}

	failures := 0
	for {
		if inst != nil {
			ranAt := time.Now()
			a.watch(ctx, e, id, inst, note)
			note = ""
			ran := time.Since(ranAt)
			if ran >= quick {
				failures = 0
			}
			reason = fmt.Errorf("exited after %s", ran.Round(time.Second))
		}
		a.mu.Lock()
		ok := a.restartable(ctx, e)
		a.mu.Unlock()
		if !ok {
			return
		}
		for inst = nil; inst == nil; {
			if failures == len(backoff) {
				if errors.As(reason, new(storageError)) {
					failures--
					continue
				}
				report(proto.VMStatus{ID: id, State: proto.StateError, Error: "restarting failed repeatedly: " + reason.Error()})
				return
			}
			select {
			case <-ctx.Done():
				return
			case <-time.After(backoff[failures]):
			}
			// Counted up front; a run longer than quick resets it.
			failures++
			var err error
			inst, note, err = a.restart(ctx, e)
			if err != nil {
				reason = err
			} else if inst == nil {
				return
			}
		}
		report(proto.VMStatus{ID: id, State: proto.StateStarting})
	}
}

func (a *Agent) prepare(ctx context.Context, e *vmEntry, f VMFile) error {
	if err := a.Provider.Prepare(ctx, f, VMDir(a.VMsDir, f.Spec.ID)); err != nil {
		return err
	}
	a.mu.Lock()
	e.file.NeedsPrepare = false
	saved := e.file
	a.mu.Unlock()
	// Booting without the cleared flag on disk would re-prepare (wipe the disks) on the next restart.
	return SaveVM(a.VMsDir, saved)
}

// start boots f from its memory state if it has one; a state that fails to load is discarded for a cold boot, whose status carries note.
func (a *Agent) start(ctx context.Context, e *vmEntry, f VMFile, dir string) (inst Instance, note string, err error) {
	if f.Hibernated {
		// Cleared first: once resumed, the guest writes to its disks, and a crash must then cold-boot.
		if err := a.clearHibernated(e); err != nil {
			return nil, "", storageError{fmt.Errorf("save: %w", err)}
		}
		// A paused guest cannot take off; it is paused once it has.
		paused := f.Power == proto.PowerPaused && !f.Takeoff
		inst, err = a.Provider.Restore(ctx, f, dir, RestoreFrom{StateDir: filepath.Join(dir, StateDir), Paused: paused, Takeoff: f.Takeoff})
		if err == nil {
			a.mu.Lock()
			e.paused = paused
			a.mu.Unlock()
			return inst, "", nil
		}
		if ctx.Err() != nil {
			// Stopping or deleted: no cold boot to replace it.
			return nil, "", err
		}
		log.Printf("vm %s: restore: %v", f.Spec.ID, err)
		note = "hibernation discarded: " + err.Error()
	}
	inst, err = a.coldStart(f, dir)
	if err == nil {
		a.mu.Lock()
		e.paused = false
		a.mu.Unlock()
	}
	return inst, note, err
}

// coldStart boots f from its disks; a memory state left behind must never be resumed against them later.
func (a *Agent) coldStart(f VMFile, dir string) (Instance, error) {
	if err := os.RemoveAll(filepath.Join(dir, StateDir)); err != nil {
		return nil, err
	}
	return a.Provider.Start(f, dir)
}

func (a *Agent) clearHibernated(e *vmEntry) error {
	a.mu.Lock()
	f := e.file
	a.mu.Unlock()
	f.Hibernated, f.Takeoff, f.Suspended = false, false, false
	if err := SaveVM(a.VMsDir, f); err != nil {
		return err
	}
	a.mu.Lock()
	e.file.Hibernated, e.file.Takeoff, e.file.Suspended = false, false, false
	a.mu.Unlock()
	return nil
}

func restingState(f VMFile) proto.VMState {
	if suspended(f) {
		return proto.StateSuspended
	}
	if f.Hibernated {
		return proto.StateHibernated
	}
	return proto.StateStopped
}

// hibernate writes e's memory to <dir>/state and powers it off; the caller holds e.op.
func (a *Agent) hibernate(e *vmEntry) error {
	a.mu.Lock()
	f, inst, wasPaused := e.file, e.inst, e.paused
	a.mu.Unlock()
	if err := CheckSpace(a.VMsDir, int64(f.Spec.MemoryMB)<<20+1<<30); err != nil {
		return err
	}
	dir := VMDir(a.VMsDir, f.Spec.ID)
	tmp, state := filepath.Join(dir, "state.tmp"), filepath.Join(dir, StateDir)
	err := os.RemoveAll(tmp)
	if err == nil {
		err = inst.SaveState(tmp)
	}
	if err == nil {
		err = os.RemoveAll(state)
	}
	if err == nil {
		err = os.Rename(tmp, state)
	}
	if err == nil {
		err = syncDir(dir)
	}
	if err == nil {
		a.mu.Lock()
		e.file.Hibernated = true
		f = e.file
		a.mu.Unlock()
		if err = SaveVM(a.VMsDir, f); err != nil {
			a.mu.Lock()
			e.file.Hibernated = false
			a.mu.Unlock()
		}
	}
	if err != nil {
		os.RemoveAll(tmp)
		os.RemoveAll(state)
		// After a kill (node stop budget) there is nothing left to resume.
		if !wasPaused && alive(inst) {
			if err := inst.Resume(); err != nil {
				log.Printf("vm %s: resume after failed hibernate: %v", f.Spec.ID, err)
			}
		}
		return err
	}
	e.stopRequested.Store(true)
	return inst.Kill()
}

// endHibernation drops e's memory state, gracefully resuming and shutting down a VM with disks first; it fails, keeping the state, while that resume is impossible.
func (a *Agent) endHibernation(ctx context.Context, e *vmEntry, graceful bool) error {
	a.mu.Lock()
	f := e.file
	a.mu.Unlock()
	dir := VMDir(a.VMsDir, f.Spec.ID)
	state := filepath.Join(dir, StateDir)
	if !graceful || len(f.Spec.Attachments) == 0 {
		if err := a.clearHibernated(e); err != nil {
			log.Printf("vm %s: save: %v", f.Spec.ID, err)
		}
		return os.RemoveAll(state)
	}
	if err := checkStorage(f); err != nil {
		return err
	}
	if err := a.clearHibernated(e); err != nil {
		return fmt.Errorf("save: %w", err)
	}
	if inst, err := a.Provider.Restore(ctx, f, dir, RestoreFrom{StateDir: state}); err != nil {
		log.Printf("vm %s: resume to shut down: %v", f.Spec.ID, err)
	} else {
		if err := inst.Stop(); err != nil {
			log.Printf("vm %s: stop: %v", f.Spec.ID, err)
			inst.Kill()
		}
		<-inst.Done()
	}
	if err := os.RemoveAll(state); err != nil {
		log.Printf("vm %s: remove state: %v", f.Spec.ID, err)
	}
	return nil
}

var errNewerDesired = errors.New("superseded by a newer desired state")

// endHibernationWait retries endHibernation on the restart backoff (unbounded only while storage is unreachable) until ctx ends or a generation above gen is queued.
func (a *Agent) endHibernationWait(ctx context.Context, e *vmEntry, graceful bool, gen uint64) error {
	for i, failures := 0, 0; ; i++ {
		err := a.endHibernation(ctx, e, graceful)
		if err == nil {
			return nil
		}
		log.Printf("vm %s: end hibernation: %v", e.file.Spec.ID, err)
		a.reportEntry(e, proto.VMStatus{ID: e.file.Spec.ID, State: proto.StateHibernated, Error: err.Error()})
		if !errors.As(err, new(storageError)) {
			if failures++; failures > len(restartBackoff) {
				return err
			}
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(restartBackoff[min(i, len(restartBackoff)-1)]):
		}
		a.mu.Lock()
		newer := e.pending != nil && e.pending.Generation > gen
		a.mu.Unlock()
		if newer {
			return errNewerDesired
		}
	}
}

// queue keeps d, unless a newer desired is already queued, to be applied once e's boot and running operation end.
func (a *Agent) queue(ctx context.Context, e *vmEntry, d proto.DesiredVM) {
	a.mu.Lock()
	if e.pending == nil || d.Generation >= e.pending.Generation {
		e.pending = &d
	}
	start := !e.waiting
	e.waiting = true
	a.mu.Unlock()
	if start {
		go a.applyPending(ctx, e)
	}
}

func (a *Agent) applyPending(ctx context.Context, e *vmEntry) {
	<-e.booted
	e.op.Lock()
	e.op.Unlock()
	a.reconcileMu.Lock()
	defer a.reconcileMu.Unlock()
	a.mu.Lock()
	d := e.pending
	e.pending, e.waiting = nil, false
	cur, ok := a.vms[e.file.Spec.ID]
	apply := ok && d != nil && !cur.placeholder && d.Generation > cur.file.Generation
	a.mu.Unlock()
	if apply && ctx.Err() == nil {
		a.updateDesired(ctx, cur, *d)
		a.sendDisks() // the replay may have dropped attachments, which the hub waits for

	}
}

// syncPause pauses or resumes inst to match e's desired power and reports e's status; the caller holds e.op.
func (a *Agent) syncPause(e *vmEntry, inst Instance) {
	a.mu.Lock()
	want, st, id := e.file.Power == proto.PowerPaused, e.status, e.file.Spec.ID
	change := want != e.paused
	a.mu.Unlock()
	if change {
		pause := inst.Resume
		if want {
			pause = inst.Pause
		} else {
			a.reloadSeed(e, inst)
		}
		if err := pause(); err != nil {
			st.Error = err.Error()
		} else {
			a.mu.Lock()
			e.paused = want
			a.mu.Unlock()
			st.Error = ""
			if !want {
				go a.guestResume(id, inst)
			}
			switch {
			case want:
				st.State = proto.StatePaused
			case st.IP != "":
				st.State = proto.StateRunning
			default:
				st.State = proto.StateStarting
			}
		}
	}
	a.reportEntry(e, st)
}

// reloadSeed brings keys and variables changed while paused into the seed the guest agent re-reads on resume; the VM resumes with the old seed on failure.
func (a *Agent) reloadSeed(e *vmEntry, inst Instance) {
	a.mu.Lock()
	f := e.file
	a.mu.Unlock()
	err := a.Provider.Reseed(context.Background(), f, VMDir(a.VMsDir, f.Spec.ID))
	if err == nil {
		err = inst.ReloadSeed()
	}
	if err != nil {
		log.Printf("vm %s: reload seed: %v", f.Spec.ID, err)
	}
}

// guestResume lets the guest catch up on its clock and seed after a pause; it keeps running without that.
func (a *Agent) guestResume(id string, inst Instance) {
	ctx, cancel := context.WithTimeout(context.Background(), guestResumeTimeout)
	defer cancel()
	if err := inst.GuestResume(ctx); err != nil {
		log.Printf("vm %s: guest agent resume: %v", id, err)
	}
}

// watch reports inst's progress until it exits, reporting stopped (or hibernated) last.
func (a *Agent) watch(ctx context.Context, e *vmEntry, id string, inst Instance, note string) {
	report := func(st proto.VMStatus) { a.reportEntry(e, st) }
	ipCtx, cancel := context.WithTimeout(ctx, ipTimeout)
	go func() {
		select {
		case <-inst.Done():
			cancel()
		case <-ipCtx.Done():
		}
	}()
	ip, err := inst.WaitIP(ipCtx)
	cancel()
	// Reporting stopped last from this goroutine keeps a late IP result from overwriting it.
	select {
	case <-inst.Done():
	default:
		if err != nil {
			report(proto.VMStatus{ID: id, State: proto.StateError, Error: "waiting for IP: " + err.Error()})
		} else {
			e.op.Lock()
			a.mu.Lock()
			paused, pause := e.paused, e.file.Power == proto.PowerPaused && !e.paused
			a.mu.Unlock()
			state := proto.StateRunning
			if paused {
				state = proto.StatePaused
			}
			report(proto.VMStatus{ID: id, State: state, IP: ip, Error: note})
			if pause {
				a.syncPause(e, inst)
			}
			a.mu.Lock()
			a.watchIdleLocked(e)
			a.mu.Unlock()
			e.op.Unlock()
		}
	}
	<-inst.Done()
	a.mu.Lock()
	f := e.file
	if e.idleStop != nil {
		e.idleStop()
		e.idleStop = nil
	}
	a.mu.Unlock()
	report(proto.VMStatus{ID: id, State: restingState(f)})
}

// hibernateFits picks the live entries whose memory states fit on paths.vms together; the others are stopped gracefully.
func (a *Agent) hibernateFits(entries map[string]*vmEntry) map[*vmEntry]bool {
	free, err := statfsFree(a.VMsDir)
	if err != nil {
		log.Printf("free space: %v", err)
		return nil
	}
	fits := map[*vmEntry]bool{}
	for id, e := range entries {
		if !alive(a.instance(e)) {
			continue
		}
		a.mu.Lock()
		need := int64(e.file.Spec.MemoryMB)<<20 + 1<<30
		a.mu.Unlock()
		if need > free {
			log.Printf("vm %s: not enough space on node to hibernate, stopping it", id)
			continue
		}
		free -= need
		fits[e] = true
	}
	return fits
}

func alive(inst Instance) bool {
	if inst == nil {
		return false
	}
	select {
	case <-inst.Done():
		return false
	default:
		return true
	}
}

func (a *Agent) instance(e *vmEntry) Instance {
	a.mu.Lock()
	defer a.mu.Unlock()
	return e.inst
}

// restartable reports whether e's exited instance should be started again; a.mu must be held.
func (a *Agent) restartable(ctx context.Context, e *vmEntry) bool {
	return ctx.Err() == nil && !a.stopping && !e.stopRequested.Load() && e.file.Power != proto.PowerStopped
}

// restart starts e again, from its memory state if it is hibernated; a nil Instance without error means it should no longer run.
func (a *Agent) restart(ctx context.Context, e *vmEntry) (Instance, string, error) {
	// Holding e.op keeps reconcile from starting a second instance while this one comes up, without stalling other VMs.
	e.op.Lock()
	defer e.op.Unlock()
	a.mu.Lock()
	f, ok := e.file, a.restartable(ctx, e)
	a.mu.Unlock()
	if !ok {
		return nil, "", nil
	}
	if f.NeedsPrepare {
		if err := a.prepare(ctx, e, f); err != nil {
			return nil, "", fmt.Errorf("prepare: %w", err)
		}
	}
	if err := checkStorage(f); err != nil {
		return nil, "", err
	}
	inst, note, err := a.start(ctx, e, f, VMDir(a.VMsDir, f.Spec.ID))
	if err != nil {
		return nil, "", fmt.Errorf("start: %w", err)
	}
	a.mu.Lock()
	ok = a.restartable(ctx, e)
	e.inst = inst
	a.mu.Unlock()
	if !ok {
		// A restored guest holds unwritten data, and one with disks has writes in flight; it must shut down, not be cut off.
		end := inst.Kill
		if f.Hibernated || len(f.Spec.Attachments) > 0 {
			e.stopRequested.Store(true)
			end = inst.Stop
		}
		if err := end(); err != nil {
			log.Printf("vm %s: stop: %v", f.Spec.ID, err)
			inst.Kill()
		}
		<-inst.Done()
		return nil, "", nil
	}
	return inst, note, nil
}

// stopAll hibernates live guests in parallel, so a node restart keeps their memory, and kills those not done within stopBudget.
func (a *Agent) stopAll() {
	a.mu.Lock()
	a.stopping = true
	entries := make(map[string]*vmEntry, len(a.vms))
	for id, e := range a.vms {
		entries[id] = e
	}
	a.mu.Unlock()
	a.releaseSnapshots()
	fits := a.hibernateFits(entries)
	var expired atomic.Bool
	kill := func(id string, e *vmEntry, inst Instance) {
		e.stopRequested.Store(true)
		if err := inst.Kill(); err != nil {
			log.Printf("vm %s: kill: %v", id, err)
		}
	}
	var wg sync.WaitGroup
	for id, e := range entries {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-e.booted
			// Waits for a restart or other operation in flight, so its instance is the one hibernated.
			e.op.Lock()
			defer e.op.Unlock()
			inst := a.instance(e)
			if !alive(inst) {
				return
			}
			if expired.Load() {
				kill(id, e, inst)
				return
			}
			if fits[e] {
				err := a.hibernate(e)
				if err == nil {
					return
				}
				log.Printf("vm %s: hibernate: %v", id, err)
				a.mu.Lock()
				saved := e.file.Hibernated
				a.mu.Unlock()
				if saved {
					// The state is committed; resuming the guest now would let its disks move on from that memory.
					if alive(inst) {
						kill(id, e, inst)
					}
					return
				}
			}
			if !expired.Load() {
				if err := inst.Stop(); err != nil {
					log.Printf("vm %s: stop: %v", id, err)
				}
			}
		}()
	}
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(stopBudget):
		expired.Store(true)
		var kills sync.WaitGroup
		for id, e := range entries {
			if inst := a.instance(e); alive(inst) {
				kills.Add(1)
				go func() { defer kills.Done(); kill(id, e, inst) }()
			}
		}
		kills.Wait()
		<-done
	}
	a.chassisMu.Lock()
	a.chassisStopping = true
	a.chassisMu.Unlock()
	a.chassisWG.Wait()
	if err := a.Provider.StopChassis(); err != nil {
		log.Printf("stop chassis: %v", err)
	}
}

// report records the status and forwards it to the hub; updates for deleted VMs are dropped.
func (a *Agent) report(st proto.VMStatus) { a.reportEntry(nil, st) }

// reportEntry is report that, with want set, also drops updates once want was replaced by a restart.
func (a *Agent) reportEntry(want *vmEntry, st proto.VMStatus) {
	a.mu.Lock()
	e, ok := a.vms[st.ID]
	ok = ok && (want == nil || e == want)
	var f VMFile
	if ok {
		f = e.file
	}
	a.mu.Unlock()
	if ok {
		st.Disks = a.disks(f)
		st.Snapshots = a.snapshotInfos(f.Spec.ID)
		st.RestoredSeq = f.RestoredSeq
	}
	a.mu.Lock()
	e, ok = a.vms[st.ID]
	ok = ok && (want == nil || e == want)
	if ok {
		st.Generation = e.file.Generation
		e.status = st
	}
	send := a.send
	a.mu.Unlock()
	if ok && send != nil {
		if err := send(proto.Message{Type: "status", Status: &st}); err != nil {
			log.Printf("vm %s: status: %v", st.ID, err)
		}
	}
}

func (a *Agent) disks(f VMFile) []proto.Disk {
	disks, err := a.Provider.Disks(f, VMDir(a.VMsDir, f.Spec.ID))
	if err != nil {
		log.Printf("vm %s: disks: %v", f.Spec.ID, err)
		return nil
	}
	return disks
}

func (a *Agent) refreshDisksLoop(ctx context.Context) {
	interval := a.diskRefresh
	if interval == 0 {
		interval = defaultDiskRefresh
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			a.refreshDisks()
		}
	}
}

// refreshDisks re-sends each running VM's stored status with fresh disks, so a newer state is never overwritten.
func (a *Agent) refreshDisks() {
	a.mu.Lock()
	var running []VMFile
	for _, e := range a.vms {
		if e.status.State == proto.StateRunning {
			running = append(running, e.file)
		}
	}
	a.mu.Unlock()
	for _, f := range running {
		disks := a.disks(f)
		a.mu.Lock()
		e, ok := a.vms[f.Spec.ID]
		if !ok || e.status.State != proto.StateRunning {
			a.mu.Unlock()
			continue
		}
		e.status.Disks = disks
		st, send := e.status, a.send
		a.mu.Unlock()
		if send != nil {
			if err := send(proto.Message{Type: "status", Status: &st}); err != nil {
				log.Printf("vm %s: status: %v", f.Spec.ID, err)
			}
		}
	}
}

func RandomMAC() string {
	b := make([]byte, 6)
	rand.Read(b)
	b[0] = b[0]&^0x01 | 0x02 // unicast, locally administered
	return net.HardwareAddr(b).String()
}

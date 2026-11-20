package registry

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"
)

// GC states: pushes are open, builds wait for running ones to end, nobody may push, garbage-collect runs.
const (
	GCOpen       = "open"
	GCDraining   = "draining"
	GCClosed     = "closed"
	GCCollecting = "collecting"
)

// ErrGCRunning is returned by Trigger while a collection is under way.
var ErrGCRunning = errors.New("registry garbage collection is already running")

// GCRun is the outcome of one collection.
type GCRun struct {
	StartedAt time.Time `json:"startedAt"`
	EndedAt   time.Time `json:"endedAt"`
	Summary   string    `json:"summary,omitempty"`
	Error     string    `json:"error,omitempty"`
}

// GCStatus is what admins see; Schedule is "HH:MM" in the hub's time zone or empty when only manual runs happen.
type GCStatus struct {
	State    string     `json:"state"`
	Schedule string     `json:"schedule"`
	Next     *time.Time `json:"next,omitempty"`
	Last     *GCRun     `json:"last,omitempty"`
}

// GC runs the registry's garbage-collect from the hub; closing push tokens and waiting out issued ones replaces stopping the registry.
type GC struct {
	// Command runs the collection, e.g. registry garbage-collect <config>.
	Command []string
	// Schedule is "HH:MM", or empty for manual runs only.
	Schedule string
	// RunningBuilds counts builds that may still push; collection waits for them.
	RunningBuilds func(context.Context) (int, error)
	// Reopened runs after every collection, to start the builds that waited.
	Reopened func()
	// TokenWait outlasts every push token issued before closing.
	TokenWait, Poll, DrainLimit time.Duration
	Now                         func() time.Time

	mu      sync.Mutex
	state   string
	last    *GCRun
	trigger chan struct{}
}

func NewGC(command []string, schedule string, runningBuilds func(context.Context) (int, error), reopened func()) *GC {
	return &GC{Command: command, Schedule: schedule, RunningBuilds: runningBuilds, Reopened: reopened,
		TokenWait: TokenTTL + 2*time.Minute, Poll: 10 * time.Second, DrainLimit: 65 * time.Minute, Now: time.Now,
		state: GCOpen, trigger: make(chan struct{}, 1)}
}

// ParseSchedule accepts "HH:MM".
func ParseSchedule(s string) error {
	_, err := time.Parse("15:04", s)
	if err != nil || len(s) != 5 {
		return fmt.Errorf("%q is not a time of day like 04:00", s)
	}
	return nil
}

// PushAllowed says whether a push token may be issued; pushes are safe until the registry closes.
func (g *GC) PushAllowed() bool {
	if g == nil {
		return true
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.state == GCOpen || g.state == GCDraining
}

// BuildsMayStart is false from the moment a collection is pending until it ends.
func (g *GC) BuildsMayStart() bool {
	if g == nil {
		return true
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.state == GCOpen
}

func (g *GC) Status() GCStatus {
	g.mu.Lock()
	defer g.mu.Unlock()
	st := GCStatus{State: g.state, Schedule: g.Schedule, Last: g.last}
	if g.Schedule != "" {
		next := g.next(g.Now())
		st.Next = &next
	}
	return st
}

// Trigger starts a collection now; ErrGCRunning when one is under way.
func (g *GC) Trigger() error {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.state != GCOpen {
		return ErrGCRunning
	}
	select {
	case g.trigger <- struct{}{}:
		g.state = GCDraining
		return nil
	default:
		return ErrGCRunning
	}
}

// Run collects at the scheduled time every day and whenever Trigger is called.
func (g *GC) Run(ctx context.Context) {
	for {
		var timer <-chan time.Time
		var t *time.Timer
		if g.Schedule != "" {
			now := g.Now()
			t = time.NewTimer(g.next(now).Sub(now))
			timer = t.C
		}
		select {
		case <-ctx.Done():
		case <-timer:
		case <-g.trigger:
		}
		if t != nil {
			t.Stop()
		}
		if ctx.Err() != nil {
			return
		}
		select {
		case <-g.trigger: // a trigger that raced the schedule is served by this run
		default:
		}
		g.collect(ctx)
	}
}

func (g *GC) next(now time.Time) time.Time {
	at, _ := time.Parse("15:04", g.Schedule)
	next := time.Date(now.Year(), now.Month(), now.Day(), at.Hour(), at.Minute(), 0, 0, now.Location())
	if !next.After(now) {
		next = next.AddDate(0, 0, 1)
	}
	return next
}

func (g *GC) setState(s string) {
	g.mu.Lock()
	g.state = s
	g.mu.Unlock()
}

func (g *GC) collect(ctx context.Context) {
	run := GCRun{StartedAt: g.Now()}
	defer func() {
		run.EndedAt = g.Now()
		g.mu.Lock()
		g.state, g.last = GCOpen, &run
		g.mu.Unlock()
		if run.Error != "" {
			slog.Error("registry garbage collection", "err", run.Error)
		} else {
			slog.Info("registry garbage collection", "summary", run.Summary)
		}
		if g.Reopened != nil {
			g.Reopened()
		}
	}()
	g.setState(GCDraining)
	if err := g.drain(ctx); err != nil {
		run.Error = err.Error()
		return
	}
	g.setState(GCClosed)
	select {
	case <-ctx.Done():
		run.Error = ctx.Err().Error()
		return
	case <-time.After(g.TokenWait):
	}
	g.setState(GCCollecting)
	out := &summaryWriter{}
	cmd := exec.CommandContext(ctx, g.Command[0], g.Command[1:]...)
	cmd.Stdout, cmd.Stderr = out, out
	cmd.Env = append(os.Environ(), "OTEL_TRACES_EXPORTER=none")
	err := cmd.Run()
	run.Summary = out.summary()
	if err != nil {
		run.Error = err.Error()
	}
}

// drain waits for running builds to end; past DrainLimit the build timeout has ended them anyway.
func (g *GC) drain(ctx context.Context) error {
	deadline := g.Now().Add(g.DrainLimit)
	for {
		// Waiting before counting also covers a build the scheduler started just before draining began.
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(g.Poll):
		}
		n, err := g.RunningBuilds(ctx)
		if err != nil {
			return err
		}
		if n == 0 || !g.Now().Before(deadline) {
			return nil
		}
	}
}

// summaryWriter keeps garbage-collect's "N blobs marked, …" line, or else its last line, without holding the whole output.
type summaryWriter struct {
	partial, marked, last string
}

func (w *summaryWriter) Write(p []byte) (int, error) {
	lines := strings.Split(w.partial+string(p), "\n")
	w.partial = lines[len(lines)-1]
	if len(w.partial) > 4096 {
		w.partial = w.partial[len(w.partial)-4096:]
	}
	for _, l := range lines[:len(lines)-1] {
		w.line(l)
	}
	return len(p), nil
}

func (w *summaryWriter) line(l string) {
	if l = strings.TrimSpace(l); l == "" {
		return
	}
	w.last = l
	if strings.Contains(l, "blobs marked") {
		w.marked = l
	}
}

func (w *summaryWriter) summary() string {
	w.line(w.partial)
	if w.marked != "" {
		return w.marked
	}
	return w.last
}

package registry

import (
	"context"
	"sync/atomic"
	"testing"
	"time"
)

func TestGCDrainsClosesCollectsAndReopens(t *testing.T) {
	var running atomic.Int32
	running.Store(1)
	reopened := make(chan struct{}, 1)
	g := NewGC([]string{"sh", "-c", `echo "blob eligible for deletion: sha256:a"; printf "\n3 blobs marked, 2 blobs and 1 manifests eligible for deletion\n"; echo "Deleting blob: x" >&2`}, "",
		func(context.Context) (int, error) { return int(running.Load()), nil }, func() { reopened <- struct{}{} })
	g.TokenWait, g.Poll = 50*time.Millisecond, 5*time.Millisecond
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go g.Run(ctx)

	if err := g.Trigger(); err != nil {
		t.Fatal(err)
	}
	if st := g.Status().State; st != GCDraining {
		t.Fatalf("trigger left state %s", st)
	}
	if !g.PushAllowed() || g.BuildsMayStart() {
		t.Fatal("draining: pushes go on, new builds must not start")
	}
	if g.Trigger() == nil {
		t.Fatal("second trigger accepted")
	}
	running.Store(0)
	waitState(t, g, GCClosed)
	if g.PushAllowed() {
		t.Fatal("closed: nobody may push")
	}
	select {
	case <-reopened:
	case <-time.After(5 * time.Second):
		t.Fatal("never reopened")
	}
	st := g.Status()
	if st.State != GCOpen || st.Last == nil || st.Last.Error != "" || st.Last.Summary != "3 blobs marked, 2 blobs and 1 manifests eligible for deletion" {
		t.Fatalf("status %+v %+v", st, st.Last)
	}
	if !g.PushAllowed() || !g.BuildsMayStart() {
		t.Fatal("not reopened")
	}
}

func TestGCReportsCommandFailure(t *testing.T) {
	reopened := make(chan struct{}, 1)
	g := NewGC([]string{"sh", "-c", "echo boom; exit 3"}, "", func(context.Context) (int, error) { return 0, nil }, func() { reopened <- struct{}{} })
	g.TokenWait, g.Poll = 0, time.Millisecond
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go g.Run(ctx)
	_ = g.Trigger()
	<-reopened
	if last := g.Status().Last; last.Error == "" || last.Summary != "boom" {
		t.Fatalf("%+v", last)
	}
}

func TestGCNext(t *testing.T) {
	g := &GC{Schedule: "04:00"}
	for now, want := range map[string]string{
		"2026-11-20T03:59:00Z": "2026-11-20T04:00:00Z",
		"2026-11-20T04:00:00Z": "2026-11-21T04:00:00Z",
		"2026-11-20T23:00:00Z": "2026-11-21T04:00:00Z",
	} {
		n, _ := time.Parse(time.RFC3339, now)
		if got := g.next(n).Format(time.RFC3339); got != want {
			t.Errorf("next(%s) = %s, want %s", now, got, want)
		}
	}
	for _, bad := range []string{"4:00", "24:00", "04:60", "x", ""} {
		if ParseSchedule(bad) == nil {
			t.Errorf("schedule %q accepted", bad)
		}
	}
	if ParseSchedule("23:59") != nil {
		t.Error("23:59 rejected")
	}
}

func waitState(t *testing.T, g *GC, want string) {
	t.Helper()
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(time.Millisecond) {
		if g.Status().State == want {
			return
		}
	}
	t.Fatalf("state %q, want %q", g.Status().State, want)
}

func TestGCTriggerAfterRunStartsAnother(t *testing.T) {
	runs := make(chan struct{}, 2)
	g := NewGC([]string{"true"}, "", func(context.Context) (int, error) { return 0, nil }, func() { runs <- struct{}{} })
	g.TokenWait, g.Poll = 0, time.Millisecond
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go g.Run(ctx)
	for range 2 {
		if err := g.Trigger(); err != nil {
			t.Fatal(err)
		}
		select {
		case <-runs:
		case <-time.After(5 * time.Second):
			t.Fatalf("run did not happen, state %s", g.Status().State)
		}
		waitState(t, g, GCOpen)
	}
}

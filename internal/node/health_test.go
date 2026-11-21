package node

import (
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/asterism-dev/observatory/internal/proto"
)

type healthRun struct {
	mu          sync.Mutex
	emergencies atomic.Int32
	// seen[i] is the emergency count and mode when check i+1 started.
	seen []healthSeen
}

type healthSeen struct {
	emergencies int32
	inEmergency bool
}

// runHealth runs a monitor whose check answers script in order (repeating the last entry) until cond holds.
func runHealth(t *testing.T, script []error, cond func(*healthRun) bool) *healthRun {
	t.Helper()
	prev := healthInterval
	healthInterval = time.Millisecond
	t.Cleanup(func() { healthInterval = prev })
	r := &healthRun{}
	var h *healthMonitor
	h = &healthMonitor{
		check: func(context.Context) error {
			r.mu.Lock()
			defer r.mu.Unlock()
			r.seen = append(r.seen, healthSeen{r.emergencies.Load(), h.inEmergency()})
			return script[min(len(r.seen), len(script))-1]
		},
		emergency: func(context.Context) error { r.emergencies.Add(1); return nil },
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { h.run(ctx); close(done) }()
	eventually(t, "health script", func() bool { r.mu.Lock(); defer r.mu.Unlock(); return cond(r) })
	cancel()
	<-done
	return r
}

func TestHealthMonitorEntersEmergencyAfterThreeFailures(t *testing.T) {
	down := errors.New("down")
	r := runHealth(t, []error{down}, func(r *healthRun) bool { return len(r.seen) >= 5 })
	if r.seen[2].emergencies != 0 || r.seen[2].inEmergency {
		t.Fatalf("emergency after two failures: %+v", r.seen[2])
	}
	if r.seen[3].emergencies != 1 || !r.seen[3].inEmergency {
		t.Fatalf("no emergency after three failures: %+v", r.seen[3])
	}
	if r.seen[4].emergencies != 2 {
		t.Fatalf("emergency not repeated while failing: %+v", r.seen[4])
	}
}

func TestHealthMonitorLeavesEmergencyOnSuccess(t *testing.T) {
	down := errors.New("down")
	r := runHealth(t, []error{down, down, down, nil}, func(r *healthRun) bool { return len(r.seen) >= 6 })
	if !r.seen[3].inEmergency {
		t.Fatalf("not in emergency after three failures: %+v", r.seen[3])
	}
	if r.seen[4].inEmergency || r.seen[5].emergencies != 1 {
		t.Fatalf("still in emergency after a success: %+v", r.seen[4:])
	}
}

func TestHealthMonitorResetsFailuresOnSuccess(t *testing.T) {
	down := errors.New("down")
	r := runHealth(t, []error{down, down, nil, down, down, nil}, func(r *healthRun) bool { return len(r.seen) >= 7 })
	if n := r.emergencies.Load(); n != 0 {
		t.Fatalf("emergency without three consecutive failures: %d", n)
	}
}

func TestAgentHealthCheck(t *testing.T) {
	h := newFakeHub(t)
	a := &Agent{Credential: "tok", controlBase: h.url()}
	if err := a.checkHealth(context.Background()); err != nil {
		t.Fatalf("healthy hub: %v", err)
	}
	h.setHealthy(false)
	if err := a.checkHealth(context.Background()); err == nil {
		t.Fatal("unhealthy hub accepted")
	}
	if err := (&Agent{Credential: "wrong", controlBase: h.url()}).checkHealth(context.Background()); err == nil {
		t.Fatal("rejected credential accepted")
	}
}

func TestEmergencyPollRunsUpdate(t *testing.T) {
	h := newFakeHub(t)
	h.setEmergencyUpdate(true)
	u := &fakeUpdater{block: make(chan struct{})}
	defer close(u.block)
	a := &Agent{HubURL: h.url(), Credential: "tok", Version: "1.0.0", Updater: u}
	a.healthErr.Store("health: 503")
	if err := a.emergencyPoll(context.Background()); err != nil {
		t.Fatal(err)
	}
	eventually(t, "update started", func() bool { return len(u.gotStarts()) == 1 })
	if got := u.gotStarts()[0]; got != h.url() {
		t.Fatalf("hub base = %q, want %q", got, h.url())
	}
	r := h.gotEmergencies()
	if len(r) != 1 || r[0].Version != "1.0.0" || r[0].Reason != "health: 503" || r[0].UpdateError != "" {
		t.Fatalf("reports = %+v", r)
	}
}

func TestEmergencyPollReportsUpdateFailure(t *testing.T) {
	h := newFakeHub(t)
	h.setEmergencyUpdate(true)
	a := &Agent{HubURL: h.url(), Credential: "tok", Updater: &fakeUpdater{startErr: errors.New("boom")}}
	if err := a.emergencyPoll(context.Background()); err != nil {
		t.Fatal(err)
	}
	eventually(t, "update finished", func() bool { return !a.updating.Load() })
	h.setEmergencyUpdate(false)
	if err := a.emergencyPoll(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := a.emergencyPoll(context.Background()); err != nil {
		t.Fatal(err)
	}
	r := h.gotEmergencies()
	if len(r) != 3 || !strings.Contains(r[1].UpdateError, "boom") || r[2].UpdateError != "" {
		t.Fatalf("reports = %+v", r)
	}
}

func TestEmergencyPollRejected(t *testing.T) {
	h := newFakeHub(t)
	a := &Agent{HubURL: h.url(), Credential: "wrong"}
	if err := a.emergencyPoll(context.Background()); err == nil {
		t.Fatal("rejected poll succeeded")
	}
}

func TestControlURLDefaultsToUnderlayGateway(t *testing.T) {
	if got := (&Agent{}).controlURL("/api/nodes/connect"); got != "http://10.198.0.1:8091/api/nodes/connect" {
		t.Fatalf("control URL = %q", got)
	}
}

func TestAgentConnectsToUnderlayControlURL(t *testing.T) {
	env := startEnv(t, nil)
	if got := env.hub.connectPaths(); len(got) != 1 || got[0] != "/api/nodes/connect" {
		t.Fatalf("connect paths = %v", got)
	}
}

func TestControlCarriesLargeMessages(t *testing.T) {
	env := startEnv(t, nil)
	big := strings.Repeat("x", 40<<10)
	env.hub.send(proto.Message{Type: "processes", ReqID: "r1", VM: big})
	eventually(t, "reply", func() bool { return len(env.hub.processReplies()) == 1 })
	if r := env.hub.processReplies()[0]; r.VM != big {
		t.Fatalf("reply VM len = %d", len(r.VM))
	}
	if n := len(env.hub.registers()); n != 1 {
		t.Fatalf("reconnected: %d registers", n)
	}
}

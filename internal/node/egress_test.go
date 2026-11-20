package node

import (
	"bytes"
	"context"
	"errors"
	"log"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/asterism-dev/observatory/internal/proto"
)

const (
	egressKey  = "-----BEGIN EC PRIVATE KEY-----\nMHcCAQEEIBsecret\n-----END EC PRIVATE KEY-----\n"
	egressCAID = "0b6a2d1e-7c1f-4c55-9a43-2f0f3c5c1e01"
)

func newEgressAgent(t *testing.T) (*Agent, *fakeProvider, *fakeEgress) {
	t.Helper()
	a, p := newReconcileAgent(t)
	h := &fakeEgress{p: p}
	a.Egress = h
	return a, p, h
}

func egressTable(value string) *proto.Egress {
	return &proto.Egress{
		CAs:     []proto.EgressCA{{ID: egressCAID, CertPEM: "CERT1", KeyPEM: egressKey}},
		Secrets: []proto.HostBoundSecret{{ID: "s1", Name: "GH_TOKEN", Placeholder: "obsph_x", Value: value, Hosts: []string{"api.github.com"}}},
	}
}

func withEgress(d proto.DesiredVM, value string) proto.DesiredVM {
	d.Egress = egressTable(value)
	d.Boot = &proto.Boot{EgressCAs: map[string]string{egressCAID: "CERT1"}}
	return d
}

// inOrder reports whether want occurs in ev as a subsequence, matching by prefix.
func inOrder(ev []string, want ...string) bool {
	for _, e := range ev {
		if len(want) > 0 && strings.HasPrefix(e, want[0]) {
			want = want[1:]
		}
	}
	return len(want) == 0
}

func after(ev []string, e string) []string {
	if i := slices.Index(ev, e); i >= 0 {
		return ev[i+1:]
	}
	return nil
}

func TestEgressStartsBeforeVM(t *testing.T) {
	a, p, _ := newEgressAgent(t)
	a.reconcile(context.Background(), 0, []proto.DesiredVM{withEgress(dv("a", "a", proto.PowerRunning, 1), "tok-secret-value")})
	waitState(t, a, "a", proto.StateRunning, 1)
	if ev := p.gotEvents(); !inOrder(ev, "spawn", "send", "redirect", "start:a") {
		t.Fatalf("events = %v", ev)
	}
}

func TestEgressNoProxyWithoutSecrets(t *testing.T) {
	a, p, _ := newEgressAgent(t)
	a.reconcile(context.Background(), 0, []proto.DesiredVM{dv("a", "a", proto.PowerRunning, 1)})
	waitState(t, a, "a", proto.StateRunning, 1)
	if p.hasEvent("spawn") {
		t.Fatalf("events = %v", p.gotEvents())
	}
}

func TestEgressCrashRemovesRedirect(t *testing.T) {
	old := restartBackoff
	restartBackoff = []time.Duration{10 * time.Millisecond}
	t.Cleanup(func() { restartBackoff = old })
	a, p, h := newEgressAgent(t)
	a.reconcile(context.Background(), 0, []proto.DesiredVM{withEgress(dv("a", "a", proto.PowerRunning, 1), "tok-secret-value")})
	waitState(t, a, "a", proto.StateRunning, 1)
	h.last().crash()
	eventually(t, "respawned", func() bool { return countEvent(p, "redirect") == 2 })
	ev := after(p.gotEvents(), "redirect")
	if !inOrder(ev, "unredirect", "spawn", "send", "redirect") || slices.Index(ev, "unredirect") > slices.Index(ev, "spawn") {
		t.Fatalf("events after first redirect = %v", ev)
	}
	if countEvent(p, "start:a") != 1 {
		t.Fatalf("VM restarted: %v", p.gotEvents())
	}
}

func TestEgressValueChangeNoRestart(t *testing.T) {
	a, p, h := newEgressAgent(t)
	a.reconcile(context.Background(), 0, []proto.DesiredVM{withEgress(dv("a", "a", proto.PowerRunning, 1), "tok-secret-value")})
	waitState(t, a, "a", proto.StateRunning, 1)
	a.reconcile(context.Background(), 0, []proto.DesiredVM{withEgress(dv("a", "a", proto.PowerRunning, 1), "tok-new-value")})
	eventually(t, "new value sent", func() bool {
		s := h.gotSent()
		return len(s) > 0 && s[len(s)-1].Secrets[0].Value == "tok-new-value"
	})
	if countEvent(p, "spawn") != 1 || countEvent(p, "start:a") != 1 {
		t.Fatalf("events = %v", p.gotEvents())
	}
	// An unchanged push sends nothing.
	n := len(h.gotSent())
	a.reconcile(context.Background(), 0, []proto.DesiredVM{withEgress(dv("a", "a", proto.PowerRunning, 1), "tok-new-value")})
	time.Sleep(50 * time.Millisecond)
	if len(h.gotSent()) != n {
		t.Fatalf("unchanged table resent: %v", p.gotEvents())
	}
}

func TestEgressStopsWithVM(t *testing.T) {
	freeSpace(t, 1<<40)
	a, p, _ := newEgressAgent(t)
	d := func(power proto.Power, gen uint64) proto.DesiredVM {
		return withEgress(dv("a", "a", power, gen), "tok-secret-value")
	}
	a.reconcile(context.Background(), 0, []proto.DesiredVM{d(proto.PowerRunning, 1)})
	waitState(t, a, "a", proto.StateRunning, 1)
	a.reconcile(context.Background(), 0, []proto.DesiredVM{d(proto.PowerHibernated, 2)})
	waitState(t, a, "a", proto.StateHibernated, 2)
	eventually(t, "proxy down", func() bool { return inOrder(p.gotEvents(), "kill-guest", "unredirect", "kill") })

	a.reconcile(context.Background(), 0, []proto.DesiredVM{d(proto.PowerRunning, 3)})
	waitState(t, a, "a", proto.StateRunning, 3)
	if ev := after(p.gotEvents(), "kill"); !inOrder(ev, "spawn", "send", "redirect", "restore:") {
		t.Fatalf("events after hibernate = %v", ev)
	}

	a.reconcile(context.Background(), 0, nil)
	eventually(t, "proxy down after delete", func() bool { return countEvent(p, "unredirect") == 2 && countEvent(p, "kill") == 2 })
	if ev := p.gotEvents(); !inOrder(ev, "restore:", "kill-guest", "unredirect", "kill") {
		t.Fatalf("events = %v", ev)
	}
}

func TestEgressStopsWithSuspendAndWakes(t *testing.T) {
	a, p, _ := newEgressAgent(t)
	freeSpace(t, 1<<40)
	a.reconcile(context.Background(), 0, []proto.DesiredVM{withEgress(dvSuspend(proto.PowerRunning, 1, 15), "tok-secret-value")})
	waitState(t, a, "a", proto.StateRunning, 1)
	p.idle <- struct{}{}
	waitState(t, a, "a", proto.StateSuspended, 1)
	eventually(t, "proxy down", func() bool { return inOrder(p.gotEvents(), "kill-guest", "unredirect", "kill") })
	p.wake <- struct{}{}
	waitState(t, a, "a", proto.StateRunning, 1)
	if ev := after(p.gotEvents(), "kill"); !inOrder(ev, "spawn", "send", "redirect", "restore:") {
		t.Fatalf("events after suspend = %v", ev)
	}
}

func TestEgressPauseKeepsProxy(t *testing.T) {
	a, p, _ := newEgressAgent(t)
	a.reconcile(context.Background(), 0, []proto.DesiredVM{withEgress(dv("a", "a", proto.PowerRunning, 1), "tok-secret-value")})
	waitState(t, a, "a", proto.StateRunning, 1)
	a.reconcile(context.Background(), 0, []proto.DesiredVM{withEgress(dv("a", "a", proto.PowerPaused, 2), "tok-secret-value")})
	waitState(t, a, "a", proto.StatePaused, 2)
	if p.hasEvent("unredirect") || p.hasEvent("kill") {
		t.Fatalf("events = %v", p.gotEvents())
	}
}

func TestEgressUnbindStopsProxy(t *testing.T) {
	a, p, _ := newEgressAgent(t)
	a.reconcile(context.Background(), 0, []proto.DesiredVM{withEgress(dv("a", "a", proto.PowerRunning, 1), "tok-secret-value")})
	waitState(t, a, "a", proto.StateRunning, 1)
	a.reconcile(context.Background(), 0, []proto.DesiredVM{dv("a", "a", proto.PowerRunning, 1)})
	eventually(t, "proxy down", func() bool { return inOrder(p.gotEvents(), "unredirect", "kill") })
	if p.hasEvent("kill-guest") || p.hasEvent("stop-guest") {
		t.Fatalf("VM stopped: %v", p.gotEvents())
	}
	// Binding again starts a proxy for the running VM.
	a.reconcile(context.Background(), 0, []proto.DesiredVM{withEgress(dv("a", "a", proto.PowerRunning, 1), "tok-secret-value")})
	eventually(t, "proxy again", func() bool { return countEvent(p, "redirect") == 2 })
}

func TestEgressTrustOnCAChange(t *testing.T) {
	a, p, _ := newEgressAgent(t)
	d := withEgress(dv("a", "a", proto.PowerRunning, 1), "tok-secret-value")
	a.reconcile(context.Background(), 0, []proto.DesiredVM{d})
	waitState(t, a, "a", proto.StateRunning, 1)
	id2 := "0b6a2d1e-7c1f-4c55-9a43-2f0f3c5c1e02"
	d.Boot = &proto.Boot{EgressCAs: map[string]string{egressCAID: "CERT1", id2: "CERT2"}}
	a.reconcile(context.Background(), 0, []proto.DesiredVM{d})
	eventually(t, "trusted", func() bool { return p.hasEvent("guest-trust") })
	p.mu.Lock()
	got := p.trusts[0]
	p.mu.Unlock()
	if len(got) != 2 || got[id2] != "CERT2" || got[egressCAID] != "CERT1" {
		t.Fatalf("trusted %v", got)
	}
	a.reconcile(context.Background(), 0, []proto.DesiredVM{d})
	time.Sleep(50 * time.Millisecond)
	if n := countEvent(p, "guest-trust"); n != 1 {
		t.Fatalf("guest-trust called %d times", n)
	}
}

func TestEgressNeverPersisted(t *testing.T) {
	dir := t.TempDir()
	f := cachedVM("a", "a")
	f.Egress = egressTable("tok-secret-value")
	if err := SaveVM(dir, f); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(filepath.Join(dir, "a", "vm.json"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), "tok-secret-value") || strings.Contains(string(b), "PRIVATE KEY") {
		t.Fatalf("vm.json = %s", b)
	}
}

func TestEgressValuesNotLogged(t *testing.T) {
	var buf bytes.Buffer
	log.SetOutput(&buf)
	t.Cleanup(func() { log.SetOutput(os.Stderr) })
	old := restartBackoff
	restartBackoff = []time.Duration{10 * time.Millisecond}
	t.Cleanup(func() { restartBackoff = old })
	a, p, h := newEgressAgent(t)
	a.reconcile(context.Background(), 0, []proto.DesiredVM{withEgress(dv("a", "a", proto.PowerRunning, 1), "tok-secret-value")})
	waitState(t, a, "a", proto.StateRunning, 1)
	h.last().crash()
	eventually(t, "respawned", func() bool { return countEvent(p, "redirect") == 2 })
	h.mu.Lock()
	h.failSend = errors.New("broken pipe")
	h.mu.Unlock()
	a.reconcile(context.Background(), 0, []proto.DesiredVM{withEgress(dv("a", "a", proto.PowerRunning, 1), "tok-other-value")})
	eventually(t, "send failed", func() bool { return countEvent(p, "send") >= 3 })
	a.reconcile(context.Background(), 0, nil)
	eventually(t, "proxy down", func() bool { return countEvent(p, "unredirect") >= 2 })
	a.stopAll()
	if s := buf.String(); strings.Contains(s, "tok-") || strings.Contains(s, "PRIVATE KEY") || strings.Contains(s, "CERT1") {
		t.Fatalf("log = %s", s)
	}
	if !strings.Contains(buf.String(), "broken pipe") {
		t.Fatalf("send failure not logged: %s", buf.String())
	}
}

func TestEgressUpBootsWithoutReadyProxy(t *testing.T) {
	old := egressReadyTimeout
	egressReadyTimeout = 50 * time.Millisecond
	t.Cleanup(func() { egressReadyTimeout = old })
	a, p, h := newEgressAgent(t)
	h.noReady = true
	a.reconcile(context.Background(), 0, []proto.DesiredVM{withEgress(dv("a", "a", proto.PowerRunning, 1), "tok-secret-value")})
	waitState(t, a, "a", proto.StateRunning, 1)
	if p.hasEvent("redirect") {
		t.Fatalf("redirect to a proxy that is not ready: %v", p.gotEvents())
	}
}

// A replaced instance that exits late must not take its successor's proxy down.
func TestEgressDownCountsInstances(t *testing.T) {
	a, p, _ := newEgressAgent(t)
	f := cachedVM("a", "a")
	f.Egress = egressTable("tok-secret-value")
	e := &vmEntry{file: f}
	a.egressUp(e)
	a.egressUp(e)
	a.egressDown("a")
	if p.hasEvent("unredirect") || countEvent(p, "spawn") != 1 {
		t.Fatalf("events = %v", p.gotEvents())
	}
	a.egressDown("a")
	if !inOrder(p.gotEvents(), "redirect", "unredirect", "kill") {
		t.Fatalf("events = %v", p.gotEvents())
	}
}

// A push landing between boot copying the file and egressUp must reach the proxy.
func TestEgressPushBeforeUpReachesProxy(t *testing.T) {
	a, _, h := newEgressAgent(t)
	var once sync.Once
	testHookBeforeEgressUp = func() {
		once.Do(func() {
			a.reconcile(context.Background(), 0, []proto.DesiredVM{withEgress(dv("a", "a", proto.PowerRunning, 1), "tok-new-value")})
		})
	}
	t.Cleanup(func() { testHookBeforeEgressUp = nil })
	a.reconcile(context.Background(), 0, []proto.DesiredVM{withEgress(dv("a", "a", proto.PowerRunning, 1), "tok-secret-value")})
	waitState(t, a, "a", proto.StateRunning, 1)
	eventually(t, "new value sent", func() bool {
		s := h.gotSent()
		return len(s) > 0 && s[len(s)-1].Secrets[0].Value == "tok-new-value"
	})
}

func TestEgressTrustLatestWins(t *testing.T) {
	a, p, _ := newEgressAgent(t)
	d := withEgress(dv("a", "a", proto.PowerRunning, 1), "tok-secret-value")
	a.reconcile(context.Background(), 0, []proto.DesiredVM{d})
	waitState(t, a, "a", proto.StateRunning, 1)
	release := make(chan struct{})
	p.mu.Lock()
	p.trustBlock = release
	p.mu.Unlock()
	id2, id3 := "0b6a2d1e-7c1f-4c55-9a43-2f0f3c5c1e02", "0b6a2d1e-7c1f-4c55-9a43-2f0f3c5c1e03"
	d.Boot = &proto.Boot{EgressCAs: map[string]string{egressCAID: "CERT1", id2: "CERT2"}}
	a.reconcile(context.Background(), 0, []proto.DesiredVM{d})
	eventually(t, "first trust blocked", func() bool { return p.hasEvent("guest-trust") })
	d.Boot = &proto.Boot{EgressCAs: map[string]string{id2: "CERT2", id3: "CERT3"}}
	a.reconcile(context.Background(), 0, []proto.DesiredVM{d})
	time.Sleep(50 * time.Millisecond)
	close(release)
	eventually(t, "both trusted", func() bool {
		p.mu.Lock()
		defer p.mu.Unlock()
		return len(p.trusts) == 2
	})
	p.mu.Lock()
	last := p.trusts[1]
	p.mu.Unlock()
	if len(last) != 2 || last[id3] != "CERT3" {
		t.Fatalf("last trusted %v", last)
	}
}

func TestEgressStopAllRemovesRedirects(t *testing.T) {
	a, p, _ := newEgressAgent(t)
	a.reconcile(context.Background(), 0, []proto.DesiredVM{withEgress(dv("a", "a", proto.PowerRunning, 1), "tok-secret-value")})
	waitState(t, a, "a", proto.StateRunning, 1)
	a.stopAll()
	if ev := p.gotEvents(); !inOrder(ev, "redirect", "unredirect", "kill") {
		t.Fatalf("events after stopAll = %v", ev)
	}
}

func TestEgressSendFailureRespawns(t *testing.T) {
	old := restartBackoff
	restartBackoff = []time.Duration{10 * time.Millisecond}
	t.Cleanup(func() { restartBackoff = old })
	a, p, h := newEgressAgent(t)
	a.reconcile(context.Background(), 0, []proto.DesiredVM{withEgress(dv("a", "a", proto.PowerRunning, 1), "tok-secret-value")})
	waitState(t, a, "a", proto.StateRunning, 1)
	h.mu.Lock()
	h.failSend = errors.New("broken pipe")
	h.mu.Unlock()
	a.reconcile(context.Background(), 0, []proto.DesiredVM{withEgress(dv("a", "a", proto.PowerRunning, 1), "tok-new-value")})
	eventually(t, "respawned", func() bool { return countEvent(p, "spawn") >= 2 })
	if ev := after(p.gotEvents(), "redirect"); !inOrder(ev, "send", "unredirect", "kill", "spawn") {
		t.Fatalf("events = %v", ev)
	}
}

func TestNodeForwardsUsage(t *testing.T) {
	a, _, h := newEgressAgent(t)
	got := make(chan proto.Message, 4)
	a.mu.Lock()
	a.send = func(m proto.Message) error { got <- m; return nil }
	a.mu.Unlock()
	a.reconcile(context.Background(), 0, []proto.DesiredVM{withEgress(dv("a", "a", proto.PowerRunning, 1), "tok-secret-value")})
	waitState(t, a, "a", proto.StateRunning, 1)
	pct := 42.0
	obs := proto.UsageObservation{Integration: "i1", Account: "acc1", FiveHourPct: &pct, ObservedAt: time.Now().UTC()}
	h.last().usage <- obs
	for {
		select {
		case m := <-got:
			if m.Type != "usage" {
				continue
			}
			if len(m.Usage) != 1 || m.Usage[0].Account != "acc1" || m.Usage[0].Integration != "i1" || *m.Usage[0].FiveHourPct != 42 {
				t.Fatalf("usage = %+v", m.Usage)
			}
			return
		case <-time.After(5 * time.Second):
			t.Fatal("no usage message")
		}
	}
}

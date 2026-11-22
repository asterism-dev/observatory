package api

import (
	"bufio"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/asterism-dev/observatory/internal/store"
	"github.com/asterism-dev/observatory/internal/telemetry"
)

// stale makes e's policy not fresh until the returned flag is set.
func (e *env) stale() *atomic.Bool {
	var fresh atomic.Bool
	e.z.WithFreshness(fresh.Load)
	return &fresh
}

func TestUserRoutesUnavailableWhenStale(t *testing.T) {
	e := newEnv(t)
	alice, _ := e.user("alice")
	cli := e.cliSession(alice)
	fresh := e.stale()
	for _, c := range []struct{ method, path string }{
		{"POST", "/api/registry/credentials"},
		{"GET", "/api/orgs/" + e.personalOrg(alice).String() + "/limits"},
	} {
		if rec := e.do(c.method, c.path, cli); rec.Code != http.StatusServiceUnavailable || !strings.Contains(rec.Body.String(), "policy unavailable") {
			t.Fatalf("%s %s while stale: %d %s", c.method, c.path, rec.Code, rec.Body.String())
		}
	}
	fresh.Store(true)
	if rec := e.do("POST", "/api/registry/credentials", cli); rec.Code != http.StatusCreated {
		t.Fatalf("credential once fresh: %d %s", rec.Code, rec.Body.String())
	}
}

func TestRegistryTokenUnavailableWhenStale(t *testing.T) {
	e := newEnv(t)
	cred, scope := e.registrySetup()
	h := e.registryHandler("http://registry.invalid", nil)
	fresh := e.stale()
	if code, _ := grant(t, h, "observatory", cred, scope); code != http.StatusServiceUnavailable {
		t.Fatalf("token on a stale policy: %d", code)
	}
	fresh.Store(true)
	if code, got := grant(t, h, "observatory", cred, scope); code != http.StatusOK || len(got) != 1 {
		t.Fatalf("token once fresh: %d %v", code, got)
	}
}

func TestTelemetryTokenUnavailableWhenStale(t *testing.T) {
	e := newEnv(t)
	_, ownerTok := e.user("alice")
	org := e.createOrg(ownerTok, "Acme")
	now := time.Now()
	e.api.now = func() time.Time { return now }
	tok := e.createTelemetryToken(ownerTok, org, "dash")
	if _, status := e.grafana("Bearer " + tok.Token); status != http.StatusOK {
		t.Fatalf("fresh: %d", status)
	}
	fresh := e.stale()
	// Past the recheck interval too: a stale policy must neither serve from the cache nor revoke.
	now = now.Add(2 * telemetryTokenRecheck)
	for range 2 {
		if _, status := e.grafana("Bearer " + tok.Token); status != http.StatusServiceUnavailable {
			t.Fatalf("stale: %d", status)
		}
	}
	if _, err := e.store.TelemetryTokenBySecret(context.Background(), tok.Token); errors.Is(err, store.ErrNotFound) {
		t.Fatal("a stale policy revoked the token")
	}
	fresh.Store(true)
	if _, status := e.grafana("Bearer " + tok.Token); status != http.StatusOK {
		t.Fatalf("fresh again: %d", status)
	}
}

func TestLogTailEndsWhenStale(t *testing.T) {
	te := newTelemetryEnv(t, cpuSeries)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"_time":"2026-01-01T00:00:00Z","_msg":"tailed","severity_text":"WARN","source":"observatory-node"}` + "\n"))
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	}))
	t.Cleanup(upstream.Close)
	te.api.telemetry = &telemetry.Client{MetricsURL: upstream.URL, LogsURL: upstream.URL}
	te.api.policyRecheck = 20 * time.Millisecond
	srv := httptest.NewServer(te.h)
	t.Cleanup(srv.Close)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "GET", srv.URL+te.path("/logs/tail"), nil)
	req.Header.Set("Authorization", "Bearer "+te.ownerTok)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body := bufio.NewReader(resp.Body)
	if line, err := body.ReadString('\n'); err != nil || !strings.HasPrefix(line, "data: ") {
		t.Fatalf("%q %v", line, err)
	}
	te.stale()
	if _, err := io.Copy(io.Discard, body); err != nil {
		t.Fatalf("tail did not end cleanly: %v", err)
	}
	if ctx.Err() != nil {
		t.Fatal("tail outlived the stale policy")
	}
}

// staleAfterFirst makes e's policy fresh for exactly one freshness read, standing in for a policy that goes stale mid-decision.
func (e *env) staleAfterFirst() {
	var reads atomic.Int32
	e.z.WithFreshness(func() bool { return reads.Add(1) == 1 })
}

func TestGrafanaProxiesRefuseWhenStale(t *testing.T) {
	te := newTelemetryEnv(t, cpuSeries)
	var hits atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		_, _ = w.Write([]byte(`{"status":"success","data":{}}`))
	}))
	t.Cleanup(upstream.Close)
	te.api.telemetry = &telemetry.Client{MetricsURL: upstream.URL, LogsURL: upstream.URL}
	tok := te.createTelemetryToken(te.ownerTok, te.org, "dash")
	te.stale()
	for _, path := range []string{"/grafana/prometheus/api/v1/query?query=up", "/grafana/logs/select/logsql/query?query=*"} {
		for _, bearer := range []string{tok.Token, "not-a-real-token"} {
			req := httptest.NewRequest("GET", path, nil)
			req.Header.Set("Authorization", "Bearer "+bearer)
			rec := httptest.NewRecorder()
			te.h.ServeHTTP(rec, req)
			if rec.Code != http.StatusServiceUnavailable || strings.Contains(rec.Body.String(), "success") {
				t.Fatalf("%s with %q while stale: %d %s", path, bearer, rec.Code, rec.Body.String())
			}
		}
	}
	if n := hits.Load(); n != 0 {
		t.Fatalf("the telemetry store got %d requests while the policy was stale", n)
	}
}

func TestTelemetryTokenDecidesOnOneSnapshot(t *testing.T) {
	e := newEnv(t)
	_, ownerTok := e.user("alice")
	org := e.createOrg(ownerTok, "Acme")
	now := time.Now()
	e.api.now = func() time.Time { return now }
	tok := e.createTelemetryToken(ownerTok, org, "dash")
	now = now.Add(2 * telemetryTokenRecheck)
	e.staleAfterFirst()
	if _, status := e.grafana("Bearer " + tok.Token); status != http.StatusOK {
		t.Fatalf("recheck across a staleness flip: %d", status)
	}
	if _, err := e.store.TelemetryTokenBySecret(context.Background(), tok.Token); err != nil {
		t.Fatalf("token revoked across a staleness flip: %v", err)
	}
}

func TestBuilderOwnerCheckDefersWhenStale(t *testing.T) {
	e := newEnv(t).withBuilds()
	_, aliceTok := e.user("alice")
	org := e.createOrg(aliceTok, "Acme")
	e.node(org)
	img := e.recipeImage(aliceTok, org, "tools", `[]`)
	id := e.startBuild(aliceTok, org, img)
	// The round-level hold passes, so the owner check itself must see the stale policy and defer.
	e.sched.PolicyFresh = func() bool { return true }
	fresh := e.stale()
	e.tick()
	if b := e.build(id); b.State != store.BuildQueued {
		t.Fatalf("build on a stale owner check: %s %q", b.State, b.Error)
	}
	fresh.Store(true)
	e.tick()
	if b := e.build(id); b.State != store.BuildRunning {
		t.Fatalf("build once fresh: %s %q", b.State, b.Error)
	}
}

func TestAutoRebuildSkipsWhenStaleMidTick(t *testing.T) {
	e := newEnv(t).withBuilds().withAuto()
	_, aliceTok := e.user("alice")
	org := e.createOrg(aliceTok, "Acme")
	e.node(org)
	img := e.recipeImage(aliceTok, org, "basis", "")
	e.setAuto(aliceTok, org, img, `{"autoSchedule":"daily"}`)
	e.staleAfterFirst()
	if err := e.auto.Tick(context.Background()); err == nil {
		t.Fatal("a tick across a staleness flip reported no problem")
	}
	if bs := e.builds(img); len(bs) != 0 {
		t.Fatalf("a staleness flip recorded builds: %+v", bs)
	}
	e.z.WithFreshness(func() bool { return true })
	e.autoTick()
	if bs := e.builds(img); len(bs) != 1 || bs[0].State != store.BuildQueued {
		t.Fatalf("builds once fresh: %+v", bs)
	}
}

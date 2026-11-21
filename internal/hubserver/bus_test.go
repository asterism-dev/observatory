package hubserver

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/asterism-dev/observatory/internal/db/dbtest"
	"github.com/asterism-dev/observatory/internal/platform/bus"
	"github.com/asterism-dev/observatory/internal/store"
)

// forward relays every connection on ln to target, standing in for a bus that comes up late.
func forward(t *testing.T, ln net.Listener, target string) {
	t.Helper()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			up, err := net.Dial("tcp", target)
			if err != nil {
				c.Close()
				continue
			}
			go func() { _, _ = io.Copy(up, c); up.Close() }()
			go func() { _, _ = io.Copy(c, up); c.Close() }()
		}
	}()
}

func TestHubServesWithoutBusAndRelaysOnceItAppears(t *testing.T) {
	natsURL, natsToken, stopNATS, err := bus.Embedded(t.Context(), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer stopNATS()
	// A port nothing listens on yet; the bus becomes reachable there later.
	late, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	lateAddr := late.Addr().String()
	late.Close()

	pool := dbtest.New(t)
	env := testHubEnv(t, pool.Config().ConnString(), "nats://"+lateAddr, natsToken)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- Run(ctx, func(k string) string { return env[k] }, ln) }()
	defer func() {
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Error(err)
			}
		case <-time.After(20 * time.Second):
			t.Error("hub did not stop")
		}
	}()

	waitFor := func(what string, cond func() bool) {
		t.Helper()
		deadline := time.Now().Add(15 * time.Second)
		for !cond() {
			if time.Now().After(deadline) {
				t.Fatalf("timed out waiting for %s", what)
			}
			time.Sleep(50 * time.Millisecond)
		}
	}
	waitFor("the hub to serve without the bus", func() bool {
		resp, err := http.Get("http://" + ln.Addr().String() + "/api/auth/config")
		if err != nil {
			return false
		}
		resp.Body.Close()
		return resp.StatusCode == http.StatusOK
	})

	if err := store.New(pool).Audit(t.Context(), store.AuditEntry{ActorKind: store.ActorSystem, Action: store.ActionRegistryGC, Result: store.ResultAllowed}); err != nil {
		t.Fatal(err)
	}
	unsent := func() int {
		var n int
		if err := pool.QueryRow(t.Context(), "SELECT count(*) FROM outbox WHERE sent_at IS NULL").Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	time.Sleep(1500 * time.Millisecond)
	if n := unsent(); n != 1 {
		t.Fatalf("%d unsent outbox rows while the bus is down, want 1", n)
	}

	late, err = net.Listen("tcp", lateAddr)
	if err != nil {
		t.Fatal(err)
	}
	defer late.Close()
	forward(t, late, strings.TrimPrefix(natsURL, "nats://"))
	waitFor("the outbox row to be published", func() bool { return unsent() == 0 })
}

func TestRelayLogsAndRetriesWrongToken(t *testing.T) {
	url, token, stop, err := bus.Embedded(t.Context(), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer stop()
	var logs syncBuffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
	defer slog.SetDefault(prev)

	ctx, cancel := context.WithTimeout(t.Context(), 6*time.Second)
	defer cancel()
	wrong := token[:31] + "x"
	runBus(ctx, nil, nil, nil, nil, nil, nil, url, wrong)
	if n := strings.Count(logs.String(), "event bus unavailable"); n < 2 {
		t.Fatalf("logged %d retries, want >= 2:\n%s", n, logs.String())
	}
	if strings.Contains(logs.String(), wrong) {
		t.Fatal("token logged")
	}
}

type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

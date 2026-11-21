package bus_test

import (
	"context"
	"net"
	"strconv"
	"testing"
	"time"

	"github.com/nats-io/nats-server/v2/server"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"

	"github.com/asterism-dev/observatory/internal/platform/bus"
)

func TestEmbeddedStream(t *testing.T) {
	ctx := context.Background()
	url, token, stop, err := bus.Embedded(ctx, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer stop()
	js, closeConn, err := bus.Connect(ctx, url, token)
	if err != nil {
		t.Fatal(err)
	}
	defer closeConn()

	for range 2 {
		if err := bus.EnsureStream(ctx, js); err != nil {
			t.Fatal(err)
		}
	}
	s, err := js.Stream(ctx, bus.Stream)
	if err != nil {
		t.Fatal(err)
	}
	cfg := s.CachedInfo().Config
	if len(cfg.Subjects) != 1 || cfg.Subjects[0] != "events.>" || cfg.Storage != jetstream.FileStorage ||
		cfg.Retention != jetstream.LimitsPolicy || cfg.MaxAge != 7*24*time.Hour || cfg.Duplicates != 10*time.Minute {
		t.Fatalf("stream config = %+v", cfg)
	}
}

func TestUnauthenticatedRefused(t *testing.T) {
	ctx := context.Background()
	url, token, stop, err := bus.Embedded(ctx, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer stop()
	if len(token) < 32 {
		t.Fatalf("token length = %d, want >= 32", len(token))
	}
	if nc, err := nats.Connect(url); err == nil {
		nc.Close()
		t.Fatal("client without token connected")
	}
	short, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	if _, _, err := bus.Connect(short, url, token[:31]+"x"); err == nil {
		t.Fatal("client with wrong token connected")
	}
}

func TestShortTokenRejected(t *testing.T) {
	if _, _, err := bus.Connect(context.Background(), "nats://127.0.0.1:1", "short"); err == nil {
		t.Fatal("short token accepted")
	}
}

func TestConnectWaitsForNATS(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := l.Addr().(*net.TCPAddr).Port
	l.Close()
	const token = "0123456789abcdef0123456789abcdef"

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	type result struct {
		js    jetstream.JetStream
		close func()
		err   error
	}
	done := make(chan result, 1)
	go func() {
		js, closeConn, err := bus.Connect(ctx, "nats://127.0.0.1:"+strconv.Itoa(port), token)
		done <- result{js, closeConn, err}
	}()

	time.Sleep(1500 * time.Millisecond)
	select {
	case r := <-done:
		t.Fatalf("Connect returned before NATS started: %v", r.err)
	default:
	}
	s, err := server.NewServer(&server.Options{Host: "127.0.0.1", Port: port, JetStream: true, StoreDir: t.TempDir(), Authorization: token, NoSigs: true, NoLog: true})
	if err != nil {
		t.Fatal(err)
	}
	go s.Start()
	defer s.Shutdown()

	r := <-done
	if r.err != nil {
		t.Fatal(r.err)
	}
	defer r.close()
	if err := bus.EnsureStream(ctx, r.js); err != nil {
		t.Fatal(err)
	}
}

func TestConnectHonoursContext(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	if _, _, err := bus.Connect(ctx, "nats://127.0.0.1:1", "0123456789abcdef0123456789abcdef"); err == nil {
		t.Fatal("Connect succeeded without a server")
	}
}

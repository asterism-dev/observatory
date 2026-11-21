// Package bus connects to the NATS JetStream event bus and declares its EVENTS stream.
package bus

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/nats-io/nats-server/v2/server"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
)

// Stream holds every event under events.<org or "platform">.<type>.
const Stream = "EVENTS"

const (
	connectAttempt = 30 * time.Second
	maxRetryWait   = 30 * time.Second
	repeatWarning  = time.Minute
)

var errShortToken = errors.New("nats token must be at least 32 characters")

// Connect waits until NATS accepts the token, gives up for good or ctx ends, then returns a JetStream handle that reconnects forever.
func Connect(ctx context.Context, url, token string) (jetstream.JetStream, func(), error) {
	if len(token) < 32 {
		return nil, nil, errShortToken
	}
	connected := make(chan struct{}, 1)
	closed := make(chan struct{}, 1)
	signal := func(ch chan struct{}) func(*nats.Conn) {
		return func(*nats.Conn) {
			select {
			case ch <- struct{}{}:
			default:
			}
		}
	}
	w := &warner{}
	nc, err := nats.Connect(url, nats.Token(token), nats.RetryOnFailedConnect(true), nats.MaxReconnects(-1),
		nats.ReconnectWait(time.Second),
		nats.ConnectHandler(signal(connected)),
		// nats gives up after the same auth error twice; without this Connect would wait for ctx forever.
		nats.ClosedHandler(signal(closed)),
		nats.DisconnectErrHandler(func(_ *nats.Conn, err error) { w.warn("event bus disconnected", err) }),
		nats.ReconnectErrHandler(func(_ *nats.Conn, err error) { w.warn("event bus reconnect failed", err) }),
		nats.ReconnectHandler(func(*nats.Conn) {
			w.reset()
			slog.Info("event bus reconnected")
		}))
	if err != nil {
		return nil, nil, fmt.Errorf("connect to nats: %w", err)
	}
	if !nc.IsConnected() {
		select {
		case <-connected:
		case <-closed:
			return nil, nil, fmt.Errorf("connect to nats: %w", errors.Join(errors.New("connection closed"), nc.LastError()))
		case <-ctx.Done():
			lastErr := nc.LastError()
			nc.Close()
			return nil, nil, fmt.Errorf("connect to nats: %w", errors.Join(ctx.Err(), lastErr))
		}
	}
	js, err := jetstream.New(nc)
	if err != nil {
		nc.Close()
		return nil, nil, fmt.Errorf("jetstream: %w", err)
	}
	return js, nc.Close, nil
}

// Await is Connect with a time limit per attempt, retrying with logged backoff until it succeeds or ctx ends.
func Await(ctx context.Context, url, token string) (jetstream.JetStream, func(), error) {
	for wait := time.Second; ; wait = min(2*wait, maxRetryWait) {
		attempt, cancel := context.WithTimeout(ctx, connectAttempt)
		js, closeConn, err := Connect(attempt, url, token)
		cancel()
		if err == nil {
			return js, closeConn, nil
		}
		if ctx.Err() != nil || errors.Is(err, errShortToken) {
			return nil, nil, err
		}
		slog.Warn("event bus unavailable", "err", err, "retry_in", wait)
		select {
		case <-ctx.Done():
			return nil, nil, ctx.Err()
		case <-time.After(wait):
		}
	}
}

// warner logs a connection problem when it changes and repeats it at most once a minute, since reconnects fail every second.
type warner struct {
	mu   sync.Mutex
	last string
	at   time.Time
}

func (w *warner) warn(msg string, err error) {
	if err == nil {
		return
	}
	key := msg + ": " + err.Error()
	w.mu.Lock()
	if key == w.last && time.Since(w.at) < repeatWarning {
		w.mu.Unlock()
		return
	}
	w.last, w.at = key, time.Now()
	w.mu.Unlock()
	slog.Warn(msg, "err", err)
}

func (w *warner) reset() {
	w.mu.Lock()
	w.last = ""
	w.mu.Unlock()
}

// EnsureStream creates the EVENTS stream or updates it to the current config.
func EnsureStream(ctx context.Context, js jetstream.JetStream) error {
	_, err := js.CreateOrUpdateStream(ctx, jetstream.StreamConfig{
		Name:       Stream,
		Subjects:   []string{"events.>"},
		Storage:    jetstream.FileStorage,
		Retention:  jetstream.LimitsPolicy,
		MaxAge:     7 * 24 * time.Hour, // ponytail: a consumer down longer than this loses events (the hub's purges); replay from the outbox or audit if that matters.
		Duplicates: 10 * time.Minute,
	})
	if err != nil {
		return fmt.Errorf("ensure stream %s: %w", Stream, err)
	}
	return nil
}

// Embedded runs a JetStream-enabled NATS server in-process on a random loopback port, guarded by a random token.
func Embedded(ctx context.Context, dataDir string) (url, token string, stop func(), err error) {
	secret := make([]byte, 32)
	if _, err := rand.Read(secret); err != nil {
		return "", "", nil, fmt.Errorf("nats token: %w", err)
	}
	token = hex.EncodeToString(secret)
	s, err := server.NewServer(&server.Options{
		Host:          "127.0.0.1",
		Port:          server.RANDOM_PORT,
		JetStream:     true,
		StoreDir:      dataDir,
		Authorization: token,
		NoSigs:        true,
		NoLog:         true,
	})
	if err != nil {
		return "", "", nil, fmt.Errorf("embedded nats: %w", err)
	}
	go s.Start()
	stop = func() {
		s.Shutdown()
		s.WaitForShutdown()
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	for !s.ReadyForConnections(100 * time.Millisecond) {
		if ctx.Err() != nil {
			stop()
			return "", "", nil, errors.New("embedded nats did not become ready")
		}
	}
	return s.ClientURL(), token, stop, nil
}

// Package bus connects to the NATS JetStream event bus and declares its EVENTS stream.
package bus

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"github.com/nats-io/nats-server/v2/server"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
)

// Stream holds every event under events.<org or "platform">.<type>.
const Stream = "EVENTS"

// Connect waits until NATS accepts the token or ctx ends, then returns a JetStream handle that reconnects forever.
func Connect(ctx context.Context, url, token string) (jetstream.JetStream, func(), error) {
	if len(token) < 32 {
		return nil, nil, errors.New("nats token must be at least 32 characters")
	}
	connected := make(chan struct{}, 1)
	nc, err := nats.Connect(url, nats.Token(token), nats.RetryOnFailedConnect(true), nats.MaxReconnects(-1),
		nats.ReconnectWait(time.Second),
		nats.ConnectHandler(func(*nats.Conn) {
			select {
			case connected <- struct{}{}:
			default:
			}
		}))
	if err != nil {
		return nil, nil, fmt.Errorf("connect to nats: %w", err)
	}
	if !nc.IsConnected() {
		select {
		case <-connected:
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

// EnsureStream creates the EVENTS stream or updates it to the current config.
func EnsureStream(ctx context.Context, js jetstream.JetStream) error {
	_, err := js.CreateOrUpdateStream(ctx, jetstream.StreamConfig{
		Name:       Stream,
		Subjects:   []string{"events.>"},
		Storage:    jetstream.FileStorage,
		Retention:  jetstream.LimitsPolicy,
		MaxAge:     7 * 24 * time.Hour,
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

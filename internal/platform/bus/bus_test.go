package bus_test

import (
	"context"
	"testing"
	"time"

	"github.com/nats-io/nats.go/jetstream"

	"github.com/asterism-dev/observatory/internal/platform/bus"
)

func TestEmbeddedStream(t *testing.T) {
	ctx := context.Background()
	url, stop, err := bus.Embedded(ctx, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer stop()
	js, closeConn, err := bus.Connect(ctx, url)
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

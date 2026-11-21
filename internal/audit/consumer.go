package audit

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/nats-io/nats.go/jetstream"
	"google.golang.org/protobuf/proto"

	eventsv1 "github.com/asterism-dev/observatory/gen/go/observatory/events/v1"
	"github.com/asterism-dev/observatory/internal/platform/bus"
	"github.com/asterism-dev/observatory/internal/platform/uuidx"
)

const (
	consumerName = "audit"
	ackWait      = 30 * time.Second
	maxRetry     = time.Minute
)

// consume stores audit events from the bus until ctx ends; an event is acked only once its row exists.
func consume(ctx context.Context, js jetstream.JetStream, s *store) error {
	c, err := js.CreateOrUpdateConsumer(ctx, bus.Stream, jetstream.ConsumerConfig{
		Durable:       consumerName,
		FilterSubject: "events.>",
		AckPolicy:     jetstream.AckExplicitPolicy,
		AckWait:       ackWait,
		MaxDeliver:    -1,
	})
	if err != nil {
		return fmt.Errorf("audit consumer: %w", err)
	}
	cc, err := c.Consume(func(msg jetstream.Msg) { handle(ctx, s, msg) })
	if err != nil {
		return fmt.Errorf("consume: %w", err)
	}
	select {
	case <-ctx.Done():
		cc.Drain()
		<-cc.Closed()
		return nil
	case <-cc.Closed():
		return errors.New("audit consumer closed unexpectedly")
	}
}

func handle(ctx context.Context, s *store, msg jetstream.Msg) {
	var e eventsv1.Event
	if err := proto.Unmarshal(msg.Data(), &e); err != nil {
		terminate(msg, "undecodable event", err)
		return
	}
	if !e.GetAudit() {
		_ = msg.Ack()
		return
	}
	if err := validate(&e); err != nil {
		terminate(msg, "invalid audit event", err)
		return
	}
	if err := s.insert(ctx, &e); err != nil {
		delay := retryDelay(msg)
		slog.Warn("audit event not stored, retrying", "id", e.GetId(), "err", err, "retry_in", delay)
		_ = msg.NakWithDelay(delay)
		return
	}
	_ = msg.Ack()
}

// terminate drops a message no redelivery can fix, loudly, since it may be a lost audit record.
func terminate(msg jetstream.Msg, reason string, err error) {
	slog.Error("audit: dropping "+reason, "subject", msg.Subject(), "err", err)
	_ = msg.Term()
}

func validate(e *eventsv1.Event) error {
	if _, err := uuidx.Canonical(e.GetId()); err != nil {
		return fmt.Errorf("id: %w", err)
	}
	if e.GetOrg() != "" {
		if _, err := uuidx.Canonical(e.GetOrg()); err != nil {
			return fmt.Errorf("org: %w", err)
		}
	}
	if e.GetTime() == nil || !e.GetTime().IsValid() || e.GetType() == "" {
		return fmt.Errorf("event %s lacks time or type", e.GetId())
	}
	return nil
}

// retryDelay backs off exponentially per delivery, capped at maxRetry.
func retryDelay(msg jetstream.Msg) time.Duration {
	n := uint64(1)
	if md, err := msg.Metadata(); err == nil && md.NumDelivered > 0 {
		n = md.NumDelivered
	}
	if n > 6 {
		return maxRetry
	}
	return min(time.Second<<(n-1), maxRetry)
}

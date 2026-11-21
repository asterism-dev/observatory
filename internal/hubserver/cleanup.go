package hubserver

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/nats-io/nats.go/jetstream"
	"google.golang.org/protobuf/proto"

	eventsv1 "github.com/asterism-dev/observatory/gen/go/observatory/events/v1"
	"github.com/asterism-dev/observatory/internal/authz"
	"github.com/asterism-dev/observatory/internal/platform/bus"
	"github.com/asterism-dev/observatory/internal/platform/uuidx"
	"github.com/asterism-dev/observatory/internal/store"
)

const (
	cleanupConsumer = "hub"
	cleanupAckWait  = 30 * time.Second
	cleanupMaxRetry = 30 * time.Second
)

var errUnparsable = errors.New("unparsable event")

// consumeIAMEvents purges the hub's rows of removed members, deleted teams and ended sessions until ctx ends.
func consumeIAMEvents(ctx context.Context, js jetstream.JetStream, s *store.Store, notify func(org uuid.UUID)) error {
	c, err := js.CreateOrUpdateConsumer(ctx, bus.Stream, jetstream.ConsumerConfig{
		Durable: cleanupConsumer,
		FilterSubjects: []string{
			"events.*." + store.ActionMemberRemove,
			"events.*." + store.ActionMemberLeave,
			"events.*." + store.ActionTeamDelete,
			"events.platform." + store.ActionAuthLogout,
			"events.platform." + store.ActionSessionRevoke,
		},
		AckPolicy:  jetstream.AckExplicitPolicy,
		AckWait:    cleanupAckWait,
		MaxDeliver: -1,
	})
	if err != nil {
		return fmt.Errorf("hub consumer: %w", err)
	}
	cc, err := c.Consume(func(msg jetstream.Msg) { handleIAMEvent(ctx, s, notify, msg) })
	if err != nil {
		return fmt.Errorf("consume: %w", err)
	}
	select {
	case <-ctx.Done():
		cc.Drain()
		<-cc.Closed()
		return nil
	case <-cc.Closed():
		return errors.New("hub consumer closed unexpectedly")
	}
}

func handleIAMEvent(ctx context.Context, s *store.Store, notify func(uuid.UUID), msg jetstream.Msg) {
	var e eventsv1.Event
	if err := proto.Unmarshal(msg.Data(), &e); err != nil {
		slog.Error("hub: dropping undecodable event", "subject", msg.Subject(), "err", err)
		_ = msg.Ack()
		return
	}
	if e.GetResult() != eventsv1.Result_RESULT_ALLOWED {
		_ = msg.Ack()
		return
	}
	err := purge(ctx, s, notify, &e)
	switch {
	case errors.Is(err, errUnparsable):
		slog.Error("hub: dropping iam event", "subject", msg.Subject(), "id", e.GetId(), "err", err)
	case err != nil:
		delay := cleanupRetryDelay(msg)
		slog.Warn("hub: iam event cleanup failed, retrying", "id", e.GetId(), "type", e.GetType(), "err", err, "retry_in", delay)
		_ = msg.NakWithDelay(delay)
		return
	}
	_ = msg.Ack()
}

func purge(ctx context.Context, s *store.Store, notify func(uuid.UUID), e *eventsv1.Event) error {
	switch e.GetType() {
	case store.ActionMemberRemove, store.ActionMemberLeave:
		org, user, err := orgAndID(e, authz.ObjMembers+"/")
		if err != nil {
			return err
		}
		if err := s.PurgeMember(ctx, org, user); err != nil {
			return err
		}
		notify(org)
	case store.ActionTeamDelete:
		org, team, err := orgAndID(e, authz.ObjTeams+"/")
		if err != nil {
			return err
		}
		if err := s.PurgeTeam(ctx, org, team); err != nil {
			return err
		}
		notify(org)
	case store.ActionAuthLogout, store.ActionSessionRevoke:
		session, err := idAfter(e.GetResource(), "session/")
		if err != nil {
			return err
		}
		return s.DeleteSessionCredentials(ctx, session)
	default:
		return fmt.Errorf("%w: type %q", errUnparsable, e.GetType())
	}
	return nil
}

func orgAndID(e *eventsv1.Event, prefix string) (org, id uuid.UUID, err error) {
	if org, err = uuidx.Canonical(e.GetOrg()); err != nil {
		return uuid.Nil, uuid.Nil, fmt.Errorf("%w: org: %w", errUnparsable, err)
	}
	id, err = idAfter(e.GetResource(), prefix)
	return org, id, err
}

func idAfter(resource, prefix string) (uuid.UUID, error) {
	raw, ok := strings.CutPrefix(resource, prefix)
	id, err := uuidx.Canonical(raw)
	if !ok || err != nil {
		return uuid.Nil, fmt.Errorf("%w: resource %q", errUnparsable, resource)
	}
	return id, nil
}

// cleanupRetryDelay backs off from 1s, doubling per delivery, up to cleanupMaxRetry.
func cleanupRetryDelay(msg jetstream.Msg) time.Duration {
	n := uint64(1)
	if md, err := msg.Metadata(); err == nil && md.NumDelivered > 0 {
		n = md.NumDelivered
	}
	return min(time.Second<<min(n-1, 5), cleanupMaxRetry)
}

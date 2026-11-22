package quota

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/nats-io/nats.go/jetstream"
	"google.golang.org/protobuf/proto"

	eventsv1 "github.com/asterism-dev/observatory/gen/go/observatory/events/v1"
	"github.com/asterism-dev/observatory/gen/go/observatory/iam/v1/iamv1connect"
	coreauthz "github.com/asterism-dev/observatory/internal/authz"
	"github.com/asterism-dev/observatory/internal/platform/bus"
	"github.com/asterism-dev/observatory/internal/platform/iamclient"
	lifecycle "github.com/asterism-dev/observatory/internal/platform/usage"
	"github.com/asterism-dev/observatory/internal/platform/uuidx"
)

const (
	consumerName = "quota"
	ackWait      = 30 * time.Second
	maxRetry     = 30 * time.Second

	memberDelete = "iam.members.delete"
	memberLeave  = "iam.members.leave"
	teamDelete   = "iam.teams.delete"
)

var errUnparsable = errors.New("unparsable event")

// consume applies lifecycle events, commits and iam removals one at a time, in stream order, until ctx ends.
func consume(ctx context.Context, js jetstream.JetStream, s *store, iam iamv1connect.IAMInternalServiceClient) error {
	c, err := js.CreateOrUpdateConsumer(ctx, bus.Stream, jetstream.ConsumerConfig{
		Durable: consumerName,
		FilterSubjects: []string{
			"events.*." + lifecycle.InstanceCreated,
			"events.*." + lifecycle.InstanceDeleted,
			"events.*.compute.disks.>",
			"events.*.compute.snapshots.>",
			"events.*." + lifecycle.ReservationsCommitted,
			"events.*." + memberDelete,
			"events.*." + memberLeave,
			"events.*." + teamDelete,
		},
		AckPolicy:     jetstream.AckExplicitPolicy,
		AckWait:       ackWait,
		MaxDeliver:    -1,
		MaxAckPending: 1,
	})
	// Stopped while starting is a shutdown; the durable consumer resumes next start.
	if ctx.Err() != nil {
		return nil
	}
	if err != nil {
		return fmt.Errorf("quota consumer: %w", err)
	}
	cc, err := c.Consume(func(msg jetstream.Msg) { handle(ctx, s, iam, msg) })
	if err != nil {
		if ctx.Err() != nil {
			return nil
		}
		return fmt.Errorf("consume: %w", err)
	}
	select {
	case <-ctx.Done():
		cc.Drain()
		<-cc.Closed()
		return nil
	case <-cc.Closed():
		return errors.New("quota consumer closed unexpectedly")
	}
}

func handle(ctx context.Context, s *store, iam iamv1connect.IAMInternalServiceClient, msg jetstream.Msg) {
	var e eventsv1.Event
	if err := proto.Unmarshal(msg.Data(), &e); err != nil {
		slog.Error("quota: dropping undecodable event", "subject", msg.Subject(), "err", err)
		_ = msg.Ack()
		return
	}
	if e.GetResult() != eventsv1.Result_RESULT_ALLOWED {
		_ = msg.Ack()
		return
	}
	err := apply(ctx, s, iam, &e)
	switch {
	case errors.Is(err, errUnparsable):
		slog.Error("quota: dropping unparsable event", "type", e.GetType(), "id", e.GetId(), "err", err)
	case err != nil:
		delay := retryDelay(msg)
		if ctx.Err() == nil {
			slog.Warn("quota: event not applied, retrying", "type", e.GetType(), "id", e.GetId(), "err", err, "retry_in", delay)
		}
		_ = msg.NakWithDelay(delay)
		return
	}
	_ = msg.Ack()
}

func apply(ctx context.Context, s *store, iam iamv1connect.IAMInternalServiceClient, e *eventsv1.Event) error {
	org, err := uuidx.Canonical(e.GetOrg())
	if err != nil {
		return fmt.Errorf("%w: org: %w", errUnparsable, err)
	}
	switch e.GetType() {
	case lifecycle.InstanceCreated, lifecycle.DiskCreated, lifecycle.DiskResized, lifecycle.SnapshotCreated:
		id, err := uuidx.Canonical(e.GetId())
		if err != nil {
			return fmt.Errorf("%w: id: %w", errUnparsable, err)
		}
		l, err := parseLifecycle(e)
		if err != nil {
			return err
		}
		if !validOwner(l.Owner) || l.Parent != "" && !validResource(l.Parent) {
			return fmt.Errorf("%w: owner %q, parent %q", errUnparsable, l.Owner, l.Parent)
		}
		a := l.Amounts
		return s.setAllocation(ctx, org, l.Resource, l.Owner, l.Parent, Amounts{VCPU: ptr32(a.VCPU), RAMMB: ptr32(a.RAMMB), DiskGB: ptr32(a.DiskGB), VMCount: ptr32(a.VMCount), SnapshotCount: ptr32(a.SnapshotCount)}, id)
	case lifecycle.InstanceDeleted, lifecycle.DiskDeleted, lifecycle.SnapshotDeleted:
		l, err := parseLifecycle(e)
		if err != nil {
			return err
		}
		return s.Release(ctx, org, []string{l.Resource})
	case lifecycle.ReservationsCommitted:
		ids, err := lifecycle.ParseCommitted(e)
		if err != nil {
			return fmt.Errorf("%w: %w", errUnparsable, err)
		}
		return s.Cancel(ctx, ids)
	case memberDelete, memberLeave:
		user, err := idAfter(e.GetResource(), coreauthz.ObjMembers+"/")
		if err != nil {
			return err
		}
		return s.dropMemberLimits(ctx, org, user, func(ctx context.Context) (bool, error) { return iamclient.IsMember(ctx, iam, org, user) })
	case teamDelete:
		team, err := idAfter(e.GetResource(), coreauthz.ObjTeams+"/")
		if err != nil {
			return err
		}
		return s.dropLimits(ctx, org, coreauthz.TeamSubject(team))
	}
	return fmt.Errorf("%w: type %q", errUnparsable, e.GetType())
}

func parseLifecycle(e *eventsv1.Event) (lifecycle.Lifecycle, error) {
	l, err := lifecycle.Parse(e)
	if err != nil {
		return l, fmt.Errorf("%w: %w", errUnparsable, err)
	}
	if !validResource(l.Resource) {
		return l, fmt.Errorf("%w: resource %q", errUnparsable, l.Resource)
	}
	return l, nil
}

func idAfter(resource, prefix string) (uuid.UUID, error) {
	raw, ok := strings.CutPrefix(resource, prefix)
	id, err := uuidx.Canonical(raw)
	if !ok || err != nil {
		return uuid.Nil, fmt.Errorf("%w: resource %q", errUnparsable, resource)
	}
	return id, nil
}

func ptr32(n int) *int32 {
	v := int32(n) //nolint:gosec // lifecycle.Parse caps amounts at MaxInt32
	return &v
}

// retryDelay backs off from 1 s, doubling per delivery, up to maxRetry.
func retryDelay(msg jetstream.Msg) time.Duration {
	n := uint64(1)
	if md, err := msg.Metadata(); err == nil && md.NumDelivered > 0 {
		n = md.NumDelivered
	}
	return min(time.Second<<min(n-1, 5), maxRetry)
}

// dropMemberLimits deletes user's limit row under the org lock unless iam says the user is a member again, so a stale event never wipes a re-joined member's limits.
func (s *store) dropMemberLimits(ctx context.Context, org, user uuid.UUID, member func(context.Context) (bool, error)) error {
	return pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		// SetLimits takes this lock too, so a limit set after the re-join is seen here.
		if err := lockOrg(ctx, tx, org); err != nil {
			return err
		}
		// ponytail: the transaction holds a connection and the org lock during the iam call; fine at today's removal rate.
		back, err := member(ctx)
		if err != nil || back {
			return err
		}
		if _, err := tx.Exec(ctx, "DELETE FROM limits WHERE org = $1 AND scope = $2", org, coreauthz.UserSubject(user)); err != nil {
			return fmt.Errorf("drop member limits: %w", err)
		}
		return nil
	})
}

// dropLimits deletes scope's limit row; team ids are never reused, so no re-check is needed.
func (s *store) dropLimits(ctx context.Context, org uuid.UUID, scope string) error {
	if _, err := s.pool.Exec(ctx, "DELETE FROM limits WHERE org = $1 AND scope = $2", org, scope); err != nil {
		return fmt.Errorf("drop limits: %w", err)
	}
	return nil
}

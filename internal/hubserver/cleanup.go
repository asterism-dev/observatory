package hubserver

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/nats-io/nats.go/jetstream"
	"google.golang.org/protobuf/proto"

	eventsv1 "github.com/asterism-dev/observatory/gen/go/observatory/events/v1"
	"github.com/asterism-dev/observatory/gen/go/observatory/iam/v1/iamv1connect"
	"github.com/asterism-dev/observatory/internal/api"
	"github.com/asterism-dev/observatory/internal/authz"
	"github.com/asterism-dev/observatory/internal/platform/bus"
	"github.com/asterism-dev/observatory/internal/platform/iamclient"
	"github.com/asterism-dev/observatory/internal/platform/uuidx"
	"github.com/asterism-dev/observatory/internal/store"
)

const (
	cleanupConsumer = "hub"
	cleanupAckWait  = 30 * time.Second
	cleanupMaxRetry = 30 * time.Second
)

var errUnparsable = errors.New("unparsable event")

// consumeIAMEvents purges the hub's rows of removed members, deleted teams and ended sessions, and re-pushes the orgs of users whose SSH keys changed, until ctx ends.
func consumeIAMEvents(ctx context.Context, js jetstream.JetStream, s *store.Store, iam iamv1connect.IAMInternalServiceClient, notify func(org uuid.UUID), policy func() *authz.Enforcer, manifests api.ManifestDeleter) error {
	c, err := js.CreateOrUpdateConsumer(ctx, bus.Stream, jetstream.ConsumerConfig{
		Durable: cleanupConsumer,
		FilterSubjects: []string{
			"events.*." + store.ActionMemberRemove,
			"events.*." + store.ActionMemberLeave,
			"events.*." + store.ActionTeamDelete,
			"events.platform." + store.ActionAuthLogout,
			"events.platform." + store.ActionSessionRevoke,
			"events.platform." + store.ActionSSHKeyAdd,
			"events.platform." + store.ActionSSHKeyDelete,
		},
		AckPolicy:  jetstream.AckExplicitPolicy,
		AckWait:    cleanupAckWait,
		MaxDeliver: -1,
	})
	if err != nil {
		return fmt.Errorf("hub consumer: %w", err)
	}
	cc, err := c.Consume(func(msg jetstream.Msg) { handleIAMEvent(ctx, s, iam, notify, policy, manifests, msg) })
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

func handleIAMEvent(ctx context.Context, s *store.Store, iam iamv1connect.IAMInternalServiceClient, notify func(uuid.UUID), policy func() *authz.Enforcer, manifests api.ManifestDeleter, msg jetstream.Msg) {
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
	err := purge(ctx, s, iam, notify, policy, manifests, &e)
	switch {
	case errors.Is(err, errUnparsable):
		slog.Error("hub: dropping iam event", "subject", msg.Subject(), "id", e.GetId(), "err", err)
	case err != nil:
		delay := cleanupRetryDelay(msg)
		if ctx.Err() != nil {
			_ = msg.NakWithDelay(delay)
			return
		}
		slog.Warn("hub: iam event cleanup failed, retrying", "id", e.GetId(), "type", e.GetType(), "err", err, "retry_in", delay)
		_ = msg.NakWithDelay(delay)
		return
	}
	_ = msg.Ack()
}

func purge(ctx context.Context, s *store.Store, iam iamv1connect.IAMInternalServiceClient, notify func(uuid.UUID), policy func() *authz.Enforcer, manifests api.ManifestDeleter, e *eventsv1.Event) error {
	switch e.GetType() {
	case store.ActionMemberRemove, store.ActionMemberLeave:
		org, user, err := orgAndID(e, authz.ObjMembers+"/")
		if err != nil {
			return err
		}
		images, err := api.OwnedImageVersions(ctx, s, org, store.EnvScopeUser, user)
		if err != nil {
			return err
		}
		// A stale event, e.g. replayed to a fresh consumer, must not wipe a re-joined member's rows.
		removedAt, err := strconv.ParseUint(e.GetDetails().GetFields()["revision"].GetStringValue(), 10, 64)
		if err != nil {
			return fmt.Errorf("%w: revision: %w", errUnparsable, err)
		}
		purged, err := s.PurgeMember(ctx, org, user, removedAt, func(ctx context.Context) (bool, error) { return iamclient.IsMember(ctx, iam, org, user) })
		if err != nil {
			return err
		}
		if !purged {
			slog.Info("hub: member is back in the org, keeping their rows", "org", org, "user", user, "id", e.GetId())
			return nil
		}
		deleteManifests(ctx, manifests, images)
		notify(org)
	case store.ActionTeamDelete:
		org, team, err := orgAndID(e, authz.ObjTeams+"/")
		if err != nil {
			return err
		}
		images, err := api.OwnedImageVersions(ctx, s, org, store.EnvScopeTeam, team)
		if err != nil {
			return err
		}
		if err := s.PurgeTeam(ctx, org, team); err != nil {
			return err
		}
		deleteManifests(ctx, manifests, images)
		notify(org)
	case store.ActionAuthLogout, store.ActionSessionRevoke:
		session, err := idAfter(e.GetResource(), "session/")
		if err != nil {
			return err
		}
		return s.DeleteSessionCredentials(ctx, session)
	case store.ActionSSHKeyAdd, store.ActionSSHKeyDelete:
		user, err := uuidx.Canonical(e.GetActor().GetId())
		if err != nil {
			return fmt.Errorf("%w: actor: %w", errUnparsable, err)
		}
		// A user's keys reach the VMs they own or share through teams in every org they belong to.
		for _, org := range policy().OrgsOf(user) {
			notify(org)
		}
	default:
		return fmt.Errorf("%w: type %q", errUnparsable, e.GetType())
	}
	return nil
}

func deleteManifests(ctx context.Context, m api.ManifestDeleter, images map[uuid.UUID][]string) {
	for image, digests := range images {
		api.DeleteManifests(ctx, m, image, digests)
	}
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

package store

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"time"

	"connectrpc.com/connect"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	quotav1 "github.com/asterism-dev/observatory/gen/go/observatory/quota/v1"
	"github.com/asterism-dev/observatory/gen/go/observatory/quota/v1/quotav1connect"
	"github.com/asterism-dev/observatory/internal/authz"
	"github.com/asterism-dev/observatory/internal/platform/outbox"
	"github.com/asterism-dev/observatory/internal/platform/quotaclient"
	"github.com/asterism-dev/observatory/internal/platform/usage"
)

var ErrQuotaUnavailable = errors.New("quota service unavailable")

// Limits are what an operation adds; nil dimensions are not checked.
type Limits struct{ VCPU, RAMMB, DiskGB, VMCount, SnapshotCount *int }

// Quota is the quota service as the hub uses it.
type Quota interface {
	// Reserve holds add for owner, naming the resources the hold turns into; a crossed limit is a *LimitError, an unreachable service ErrQuotaUnavailable.
	Reserve(ctx context.Context, org uuid.UUID, owner string, add Limits, resources []string) (uuid.UUID, error)
	// Cancel and Release are best effort: the reservation timeout and the deleted event are their backstops.
	Cancel(ctx context.Context, ids []uuid.UUID)
	Release(ctx context.Context, org uuid.UUID, resources []string)
	// ImageStorageLimit is the org's image storage limit in GB, nil for unlimited.
	ImageStorageLimit(ctx context.Context, org uuid.UUID) (*int, error)
}

type quotaClient struct {
	c quotav1connect.QuotaInternalServiceClient
}

// NewQuota adapts a QuotaInternalService client.
func NewQuota(c quotav1connect.QuotaInternalServiceClient) Quota { return quotaClient{c} }

func int32p(v *int) *int32 {
	if v == nil {
		return nil
	}
	n := int32(min(max(*v, 0), math.MaxInt32))
	return &n
}

func quotaErr(op string, err error) error {
	if c := connect.CodeOf(err); c == connect.CodeUnavailable || c == connect.CodeDeadlineExceeded {
		return fmt.Errorf("quota %s: %w (%w)", op, ErrQuotaUnavailable, err)
	}
	return fmt.Errorf("quota %s: %w", op, err)
}

func (q quotaClient) Reserve(ctx context.Context, org uuid.UUID, owner string, add Limits, resources []string) (uuid.UUID, error) {
	res, err := q.c.Reserve(ctx, connect.NewRequest(&quotav1.ReserveRequest{OrgId: org.String(), Owner: owner, Resources: resources,
		Amounts: &quotav1.Amounts{Vcpu: int32p(add.VCPU), RamMb: int32p(add.RAMMB), DiskGb: int32p(add.DiskGB), VmCount: int32p(add.VMCount), SnapshotCount: int32p(add.SnapshotCount)}}))
	if d, ok := quotaclient.LimitExceeded(err); ok {
		return uuid.Nil, &LimitError{Scope: d.GetScope(), Name: d.GetName(), Unit: d.GetUnit(), Limit: int(d.GetLimit()), Got: int(d.GetGot())}
	}
	if err != nil {
		return uuid.Nil, quotaErr("reserve", err)
	}
	id, err := uuid.Parse(res.Msg.GetReservationId())
	if err != nil {
		return uuid.Nil, fmt.Errorf("quota reserve: reservation id: %w", err)
	}
	return id, nil
}

func (q quotaClient) Cancel(ctx context.Context, ids []uuid.UUID) {
	raw := make([]string, len(ids))
	for i, id := range ids {
		raw[i] = id.String()
	}
	if _, err := q.c.Cancel(ctx, connect.NewRequest(&quotav1.CancelRequest{ReservationIds: raw})); err != nil {
		slog.Warn("quota cancel failed; the holds expire on their own", "err", err)
	}
}

func (q quotaClient) Release(ctx context.Context, org uuid.UUID, resources []string) {
	if _, err := q.c.Release(ctx, connect.NewRequest(&quotav1.ReleaseRequest{OrgId: org.String(), Resources: resources})); err != nil {
		slog.Warn("quota release failed; the deleted event frees the usage", "org", org, "err", err)
	}
}

func (q quotaClient) ImageStorageLimit(ctx context.Context, org uuid.UUID) (*int, error) {
	res, err := q.c.GetLimits(ctx, connect.NewRequest(&quotav1.QuotaInternalServiceGetLimitsRequest{OrgId: org.String()}))
	if err != nil {
		return nil, quotaErr("limits", err)
	}
	l := res.Msg.GetLimits().GetOrg().ImageStorageGb
	if l == nil {
		return nil, nil
	}
	n := int(*l)
	return &n, nil
}

// ponytail: artifacts still computes image usage itself (layer dedup); move to reservations when artifacts is extracted, if layer accounting allows.
func (s *Store) ImageStorageLimit(ctx context.Context, org uuid.UUID) (*int, error) {
	return s.quota.ImageStorageLimit(ctx, org)
}

type reservation struct{ org, id uuid.UUID }

type release struct {
	org      uuid.UUID
	resource string
}

// pendingQuota is a transaction's reservations and the resources it deletes.
type pendingQuota struct {
	reserved []reservation
	released []release
}

// writeCommitted writes one committed event per org, after the transaction's lifecycle events.
func (p *pendingQuota) writeCommitted(ctx context.Context, tx pgx.Tx) error {
	byOrg := map[uuid.UUID][]uuid.UUID{}
	var orgs []uuid.UUID
	for _, r := range p.reserved {
		if byOrg[r.org] == nil {
			orgs = append(orgs, r.org)
		}
		byOrg[r.org] = append(byOrg[r.org], r.id)
	}
	for _, org := range orgs {
		if err := outbox.Write(ctx, tx, usage.Committed(org, byOrg[org])); err != nil {
			return err
		}
	}
	return nil
}

// quotaTimeout bounds the best-effort calls after a transaction, which outlive a cancelled request.
const quotaTimeout = 5 * time.Second

func (p *pendingQuota) cancel(ctx context.Context, q Quota) {
	if len(p.reserved) == 0 {
		return
	}
	ids := make([]uuid.UUID, len(p.reserved))
	for i, r := range p.reserved {
		ids[i] = r.id
	}
	ctx, stop := context.WithTimeout(context.WithoutCancel(ctx), quotaTimeout)
	defer stop()
	q.Cancel(ctx, ids)
}

func (p *pendingQuota) release(ctx context.Context, q Quota) {
	if len(p.released) == 0 {
		return
	}
	byOrg := map[uuid.UUID][]string{}
	for _, r := range p.released {
		byOrg[r.org] = append(byOrg[r.org], r.resource)
	}
	ctx, stop := context.WithTimeout(context.WithoutCancel(ctx), quotaTimeout)
	defer stop()
	for org, resources := range byOrg {
		q.Release(ctx, org, resources)
	}
}

func ownerSubject(user, team *uuid.UUID) (string, error) {
	switch {
	case user != nil && team == nil:
		return authz.UserSubject(*user), nil
	case team != nil && user == nil:
		return authz.TeamSubject(*team), nil
	}
	return "", ErrNotFound
}

// reserve holds add in quota for the owner (exactly one of user and team), naming the resources the transaction creates or grows.
func (s *Store) reserve(ctx context.Context, org uuid.UUID, user, team *uuid.UUID, add Limits, resources ...string) error {
	if s.pending == nil {
		return errors.New("quota reserve outside a transaction")
	}
	owner, err := ownerSubject(user, team)
	if err != nil {
		return err
	}
	id, err := s.quota.Reserve(ctx, org, owner, add, resources)
	if err != nil {
		return err
	}
	s.pending.reserved = append(s.pending.reserved, reservation{org, id})
	return nil
}

// lifecycle writes a lifecycle event for quota to the outbox, in s's transaction.
func (s *Store) lifecycle(ctx context.Context, org uuid.UUID, typ, object string, l usage.Lifecycle) error {
	return pgx.BeginFunc(ctx, s.q, func(tx pgx.Tx) error { return outbox.Write(ctx, tx, usage.Event(org, typ, object, l)) })
}

// deleted writes resource's deleted event and releases it in quota once the outermost transaction commits.
func (s *Store) deleted(ctx context.Context, org uuid.UUID, typ, object, resource string) error {
	if s.pending == nil {
		return errors.New("quota release outside a transaction")
	}
	if err := s.lifecycle(ctx, org, typ, object, usage.Lifecycle{Resource: resource}); err != nil {
		return err
	}
	s.pending.released = append(s.pending.released, release{org, resource})
	return nil
}

func vmResource(id uuid.UUID) string       { return "vm/" + id.String() }
func diskResource(id uuid.UUID) string     { return "disk/" + id.String() }
func snapshotResource(id uuid.UUID) string { return "snapshot/" + id.String() }
func vmObject(id uuid.UUID) string         { return authz.ObjVMs + "/" + id.String() }
func diskObject(id uuid.UUID) string       { return authz.ObjDisks + "/" + id.String() }

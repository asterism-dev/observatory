// Package quotatest is an in-memory QuotaInternalService for other services' tests.
package quotatest

import (
	"context"
	"errors"
	"fmt"
	"math"
	"slices"
	"strings"
	"sync"
	"time"

	"connectrpc.com/connect"
	"github.com/google/uuid"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	quotav1 "github.com/asterism-dev/observatory/gen/go/observatory/quota/v1"
	"github.com/asterism-dev/observatory/gen/go/observatory/quota/v1/quotav1connect"
	coreauthz "github.com/asterism-dev/observatory/internal/authz"
	"github.com/asterism-dev/observatory/internal/platform/quotaclient"
	"github.com/asterism-dev/observatory/internal/platform/uuidx"
)

const ttl = 5 * time.Minute

var dimensions = [5][2]string{{"vCPU", ""}, {"RAM", " MB"}, {"disk", " GB"}, {"VM count", ""}, {"snapshot count", ""}}

type hold struct {
	org       uuid.UUID
	owner     string
	amounts   [5]int64
	resources []string
	expires   time.Time
}

// Fake checks reserves like the quota service; it consumes no events, so its unexpired holds stand in for allocations, and Release drops the holds naming a released resource.
type Fake struct {
	quotav1connect.UnimplementedQuotaInternalServiceHandler

	mu         sync.Mutex
	limits     map[uuid.UUID]map[string]*quotav1.Limits
	holds      map[string]hold
	reserves   []*quotav1.ReserveRequest
	reserveErr error
	limitsErr  error
}

var _ quotav1connect.QuotaInternalServiceHandler = (*Fake)(nil)

func NewFake() *Fake {
	return &Fake{limits: map[uuid.UUID]map[string]*quotav1.Limits{}, holds: map[string]hold{}}
}

// SetLimits replaces scope's limits (org, team:<uuid> or user:<uuid>); nil removes them.
func (f *Fake) SetLimits(org uuid.UUID, scope string, l *quotav1.Limits) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.limits[org] == nil {
		f.limits[org] = map[string]*quotav1.Limits{}
	}
	if l == nil {
		delete(f.limits[org], scope)
		return
	}
	f.limits[org][scope] = proto.CloneOf(l)
}

// FailReserves makes every Reserve fail with err until called with nil.
func (f *Fake) FailReserves(err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.reserveErr = err
}

// FailGetLimits makes every GetLimits fail with err until called with nil.
func (f *Fake) FailGetLimits(err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.limitsErr = err
}

// Reserves returns the Reserve requests received, oldest first.
func (f *Fake) Reserves() []*quotav1.ReserveRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.reserves)
}

// Holds counts the unexpired holds.
func (f *Fake) Holds() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, h := range f.holds {
		if h.expires.After(time.Now()) {
			n++
		}
	}
	return n
}

func amounts(a *quotav1.Amounts) [5]*int32 {
	if a == nil {
		return [5]*int32{}
	}
	return [5]*int32{a.Vcpu, a.RamMb, a.DiskGb, a.VmCount, a.SnapshotCount}
}

func limitFields(l *quotav1.Limits) [5]*int32 {
	return [5]*int32{l.Vcpu, l.RamMb, l.DiskGb, l.VmCount, l.SnapshotCount}
}

func (f *Fake) Reserve(_ context.Context, r *connect.Request[quotav1.ReserveRequest]) (*connect.Response[quotav1.ReserveResponse], error) {
	f.mu.Lock()
	f.reserves = append(f.reserves, proto.CloneOf(r.Msg))
	err := f.reserveErr
	f.mu.Unlock()
	if err != nil {
		return nil, err
	}
	org, err := uuidx.Canonical(r.Msg.GetOrgId())
	if err != nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("org_id: %w", err))
	}
	owner := r.Msg.GetOwner()
	kind, raw, _ := strings.Cut(owner, ":")
	id, err := uuidx.Canonical(raw)
	if err != nil || !(kind == "user" && owner == coreauthz.UserSubject(id) || kind == "team" && owner == coreauthz.TeamSubject(id)) {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("owner must be user:<uuid> or team:<uuid>"))
	}
	for _, res := range r.Msg.GetResources() {
		if !validResource(res) {
			return nil, errBadResource()
		}
	}
	add := amounts(r.Msg.GetAmounts())
	var held [5]int64
	for i, v := range add {
		if v != nil && *v < 0 {
			return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("amounts must not be negative"))
		}
		if v != nil {
			held[i] = int64(*v)
		}
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	now := time.Now()
	for _, scope := range []string{"org", owner} {
		l, ok := f.limits[org][scope]
		if !ok {
			continue
		}
		var used [5]int64
		for _, h := range f.holds {
			if h.org == org && h.expires.After(now) && (scope == "org" || h.owner == owner) {
				for i := range used {
					used[i] += h.amounts[i]
				}
			}
		}
		m := limitFields(l)
		for i, a := range add {
			if a == nil || m[i] == nil {
				continue
			}
			if got := used[i] + int64(*a); got > int64(*m[i]) {
				return nil, quotaclient.NewLimitExceeded(&quotav1.LimitExceeded{Scope: scope, Name: dimensions[i][0], Unit: dimensions[i][1], Limit: *m[i], Got: int32(min(got, math.MaxInt32))})
			}
		}
	}
	hid := uuid.NewString()
	f.holds[hid] = hold{org: org, owner: owner, amounts: held, resources: r.Msg.GetResources(), expires: now.Add(ttl)}
	return connect.NewResponse(&quotav1.ReserveResponse{ReservationId: hid, ExpiresAt: timestamppb.New(now.Add(ttl))}), nil
}

func (f *Fake) Cancel(_ context.Context, r *connect.Request[quotav1.CancelRequest]) (*connect.Response[quotav1.CancelResponse], error) {
	for _, id := range r.Msg.GetReservationIds() {
		if _, err := uuidx.Canonical(id); err != nil {
			return nil, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("reservation_ids: %w", err))
		}
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, id := range r.Msg.GetReservationIds() {
		delete(f.holds, id)
	}
	return connect.NewResponse(&quotav1.CancelResponse{}), nil
}

func (f *Fake) Release(_ context.Context, r *connect.Request[quotav1.ReleaseRequest]) (*connect.Response[quotav1.ReleaseResponse], error) {
	org, err := uuidx.Canonical(r.Msg.GetOrgId())
	if err != nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("org_id: %w", err))
	}
	for _, res := range r.Msg.GetResources() {
		if !validResource(res) {
			return nil, errBadResource()
		}
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	for id, h := range f.holds {
		if h.org == org && slices.ContainsFunc(h.resources, func(res string) bool { return slices.Contains(r.Msg.GetResources(), res) }) {
			delete(f.holds, id)
		}
	}
	return connect.NewResponse(&quotav1.ReleaseResponse{}), nil
}

func validResource(res string) bool {
	kind, raw, _ := strings.Cut(res, "/")
	_, err := uuidx.Canonical(raw)
	return err == nil && (kind == "vm" || kind == "disk" || kind == "snapshot")
}

func errBadResource() error {
	return connect.NewError(connect.CodeInvalidArgument, errors.New("resources must be vm/<uuid>, disk/<uuid> or snapshot/<uuid>"))
}

func (f *Fake) GetLimits(_ context.Context, r *connect.Request[quotav1.QuotaInternalServiceGetLimitsRequest]) (*connect.Response[quotav1.QuotaInternalServiceGetLimitsResponse], error) {
	org, err := uuidx.Canonical(r.Msg.GetOrgId())
	if err != nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("org_id: %w", err))
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.limitsErr != nil {
		return nil, f.limitsErr
	}
	out := &quotav1.OrgLimits{Org: &quotav1.Limits{}, Teams: map[string]*quotav1.Limits{}, Members: map[string]*quotav1.Limits{}}
	for scope, l := range f.limits[org] {
		if scope == "org" {
			out.Org = proto.CloneOf(l)
		} else if id, ok := strings.CutPrefix(scope, "team:"); ok {
			out.Teams[id] = proto.CloneOf(l)
		} else if id, ok := strings.CutPrefix(scope, "user:"); ok {
			out.Members[id] = proto.CloneOf(l)
		}
	}
	return connect.NewResponse(&quotav1.QuotaInternalServiceGetLimitsResponse{Limits: out}), nil
}

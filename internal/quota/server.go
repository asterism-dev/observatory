package quota

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"

	"connectrpc.com/connect"
	"github.com/google/uuid"

	quotav1 "github.com/asterism-dev/observatory/gen/go/observatory/quota/v1"
	"github.com/asterism-dev/observatory/gen/go/observatory/quota/v1/quotav1connect"
	coreauthz "github.com/asterism-dev/observatory/internal/authz"
	"github.com/asterism-dev/observatory/internal/platform/auditlog"
	"github.com/asterism-dev/observatory/internal/platform/authz"
	"github.com/asterism-dev/observatory/internal/platform/identity"
	"github.com/asterism-dev/observatory/internal/platform/uuidx"
)

// server relies on the authz interceptor: a method reaches it only for callers allowed on the request's org or the platform.
type server struct {
	quotav1connect.UnimplementedQuotaServiceHandler
	store  *store
	policy authz.Source
}

func (s *server) GetLimits(ctx context.Context, r *connect.Request[quotav1.GetLimitsRequest]) (*connect.Response[quotav1.GetLimitsResponse], error) {
	org, err := parseOrg("org", r.Msg.GetOrg())
	if err != nil {
		return nil, err
	}
	ls, err := s.orgLimits(ctx, org)
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(&quotav1.GetLimitsResponse{Limits: ls}), nil
}

// UpdateLimits sets team and member scopes; the org scope belongs to platform admins only.
func (s *server) UpdateLimits(ctx context.Context, r *connect.Request[quotav1.UpdateLimitsRequest]) (*connect.Response[quotav1.UpdateLimitsResponse], error) {
	scope := r.Msg.GetScope()
	if scope == scopeOrg {
		identity.MarkDenied(ctx)
		return nil, connect.NewError(connect.CodePermissionDenied, errors.New("permission denied"))
	}
	org, err := parseOrg("org", r.Msg.GetOrg())
	if err != nil {
		return nil, err
	}
	l, err := fromProto(r.Msg.GetLimits())
	if err != nil {
		return nil, err
	}
	e, err := s.enforcer()
	if err != nil {
		return nil, err
	}
	if err := s.set(ctx, org, scope, l, func(sub string) bool { return e.HasSubject(org, sub) }); err != nil {
		return nil, err
	}
	d := details(l)
	d["scope"] = scope
	auditlog.SetDetails(ctx, d)
	return connect.NewResponse(&quotav1.UpdateLimitsResponse{}), nil
}

func (s *server) GetOrgLimits(ctx context.Context, r *connect.Request[quotav1.GetOrgLimitsRequest]) (*connect.Response[quotav1.GetOrgLimitsResponse], error) {
	org, err := s.knownOrg(r.Msg.GetOrgId())
	if err != nil {
		return nil, err
	}
	ls, err := s.orgLimits(ctx, org)
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(&quotav1.GetOrgLimitsResponse{Limits: ls}), nil
}

func (s *server) UpdateOrgLimits(ctx context.Context, r *connect.Request[quotav1.UpdateOrgLimitsRequest]) (*connect.Response[quotav1.UpdateOrgLimitsResponse], error) {
	l, err := fromProto(r.Msg.GetLimits())
	if err != nil {
		return nil, err
	}
	org, err := s.knownOrg(r.Msg.GetOrgId())
	if err != nil {
		return nil, err
	}
	if err := s.set(ctx, org, scopeOrg, l, nil); err != nil {
		return nil, err
	}
	d := details(l)
	d["org"] = org.String()
	auditlog.SetDetails(ctx, d)
	return connect.NewResponse(&quotav1.UpdateOrgLimitsResponse{}), nil
}

func (s *server) enforcer() (*coreauthz.Enforcer, error) {
	e, ok := s.policy.Current()
	if !ok {
		return nil, connect.NewError(connect.CodeUnavailable, errors.New("no policy snapshot yet"))
	}
	return e, nil
}

// knownOrg wants a canonical org id with members in the policy; every org has an owner.
func (s *server) knownOrg(raw string) (uuid.UUID, error) {
	org, err := parseOrg("org_id", raw)
	if err != nil {
		return uuid.Nil, err
	}
	e, err := s.enforcer()
	if err != nil {
		return uuid.Nil, err
	}
	if len(e.UsersOf(org)) == 0 {
		return uuid.Nil, connect.NewError(connect.CodeNotFound, errors.New("not found"))
	}
	return org, nil
}

func parseOrg(field, raw string) (uuid.UUID, error) {
	org, err := uuidx.Canonical(raw)
	if err != nil {
		return uuid.Nil, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("%s: %w", field, err))
	}
	return org, nil
}

func (s *server) set(ctx context.Context, org uuid.UUID, scope string, l Limits, known func(string) bool) error {
	err := s.store.SetLimits(ctx, org, scope, l, known)
	switch {
	case errors.Is(err, errLimitExceedsOrg):
		return connect.NewError(connect.CodeInvalidArgument, err)
	case errors.Is(err, errNotFound):
		return connect.NewError(connect.CodeNotFound, err)
	case err != nil:
		return internalErr("set limits", err)
	}
	return nil
}

func (s *server) orgLimits(ctx context.Context, org uuid.UUID) (*quotav1.OrgLimits, error) {
	ls, err := s.store.limits(ctx, org)
	if err != nil {
		return nil, internalErr("get limits", err)
	}
	out := &quotav1.OrgLimits{Org: toProto(ls[scopeOrg]), Teams: map[string]*quotav1.Limits{}, Members: map[string]*quotav1.Limits{}}
	for scope, l := range ls {
		if id, ok := strings.CutPrefix(scope, "team:"); ok {
			out.Teams[id] = toProto(l)
		} else if id, ok := strings.CutPrefix(scope, "user:"); ok {
			out.Members[id] = toProto(l)
		}
	}
	return out, nil
}

// fromProto refuses negative values; int32 already caps them at 2147483647. No limits mean all unlimited.
func fromProto(p *quotav1.Limits) (Limits, error) {
	if p == nil {
		return Limits{}, nil
	}
	l := Limits{VCPU: p.Vcpu, RAMMB: p.RamMb, DiskGB: p.DiskGb, VMCount: p.VmCount, SnapshotCount: p.SnapshotCount, ImageStorageGB: p.ImageStorageGb}
	for _, v := range l.fields() {
		if v != nil && *v < 0 {
			return Limits{}, connect.NewError(connect.CodeInvalidArgument, errors.New("invalid limit"))
		}
	}
	return l, nil
}

func toProto(l Limits) *quotav1.Limits {
	return &quotav1.Limits{Vcpu: l.VCPU, RamMb: l.RAMMB, DiskGb: l.DiskGB, VmCount: l.VMCount, SnapshotCount: l.SnapshotCount, ImageStorageGb: l.ImageStorageGB}
}

// details names the limits as 0.x audit entries did, with null for unlimited.
func details(l Limits) map[string]any {
	val := func(p *int32) any {
		if p == nil {
			return nil
		}
		return *p
	}
	return map[string]any{"vcpu": val(l.VCPU), "ramMb": val(l.RAMMB), "diskGb": val(l.DiskGB), "vmCount": val(l.VMCount),
		"snapshotCount": val(l.SnapshotCount), "imageStorageGb": val(l.ImageStorageGB)}
}

func internalErr(what string, err error) error {
	slog.Error("quota: "+what, "err", err)
	return connect.NewError(connect.CodeInternal, errors.New(what+" failed"))
}

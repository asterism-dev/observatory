package api

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"time"

	"connectrpc.com/connect"

	hubv1 "github.com/asterism-dev/observatory/gen/go/observatory/hub/v1"
	"github.com/asterism-dev/observatory/gen/go/observatory/hub/v1/hubv1connect"
	"github.com/asterism-dev/observatory/internal/authz"
	"github.com/asterism-dev/observatory/internal/platform/uuidx"
	"github.com/asterism-dev/observatory/internal/store"
)

// canHoldWait bounds how long CanHold waits for the hub's policy to reach the caller's revision.
var canHoldWait = 5 * time.Second

// HubPolicy is the hub's enforcer and the revision of iam's policy it includes.
type HubPolicy interface {
	Current() (*authz.Enforcer, bool)
	WaitRevision(ctx context.Context, rev uint64) error
}

// ResourceServer serves ResourceService from the hub's store and policy; the caller mounts it behind a service token.
type ResourceServer struct {
	Store  *store.Store
	Policy HubPolicy
}

var _ hubv1connect.ResourceServiceHandler = ResourceServer{}

func (s ResourceServer) CheckOwnership(ctx context.Context, req *connect.Request[hubv1.CheckOwnershipRequest]) (*connect.Response[hubv1.CheckOwnershipResponse], error) {
	org, err := uuidx.Canonical(req.Msg.OrgId)
	if err != nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("invalid org_id"))
	}
	kind, raw, _ := strings.Cut(req.Msg.Subject, ":")
	scope := map[string]store.EnvScope{"user": store.EnvScopeUser, "team": store.EnvScopeTeam}[kind]
	id, err := uuidx.Canonical(raw)
	if scope == "" || err != nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("subject must be user:<uuid> or team:<uuid>"))
	}
	vms, disks, err := s.Store.Owns(ctx, org, scope, id)
	if err != nil {
		slog.Error("check ownership", "org", org, "subject", req.Msg.Subject, "err", err)
		return nil, connect.NewError(connect.CodeInternal, errors.New("internal error"))
	}
	return connect.NewResponse(&hubv1.CheckOwnershipResponse{OwnsVms: vms, OwnsDisks: disks}), nil
}

func (s ResourceServer) CanHold(ctx context.Context, req *connect.Request[hubv1.CanHoldRequest]) (*connect.Response[hubv1.CanHoldResponse], error) {
	org, err := uuidx.Canonical(req.Msg.OrgId)
	if err != nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("invalid org_id"))
	}
	rules := make([]authz.Rule, len(req.Msg.Rules))
	for i, r := range req.Msg.Rules {
		rules[i] = authz.Rule{Effect: authz.Effect(r.Effect), Object: r.Object, Action: r.Action}
	}
	wctx, cancel := context.WithTimeout(ctx, canHoldWait)
	defer cancel()
	if err := s.Policy.WaitRevision(wctx, req.Msg.MinRevision); err != nil {
		return nil, connect.NewError(connect.CodeUnavailable, errors.New("hub policy is behind iam, retry"))
	}
	e, ok := s.Policy.Current()
	if !ok {
		return nil, connect.NewError(connect.CodeUnavailable, errors.New("hub policy unavailable"))
	}
	err = e.CanHold(req.Msg.Actor, org, rules)
	switch {
	case err == nil:
		return connect.NewResponse(&hubv1.CanHoldResponse{}), nil
	case errors.Is(err, authz.ErrEscalation):
		return nil, connect.NewError(connect.CodePermissionDenied, err)
	case errors.Is(err, authz.ErrInvalidPattern):
		return nil, connect.NewError(connect.CodeInvalidArgument, err)
	}
	slog.Error("can hold", "org", org, "actor", req.Msg.Actor, "err", err)
	return nil, connect.NewError(connect.CodeInternal, errors.New("internal error"))
}

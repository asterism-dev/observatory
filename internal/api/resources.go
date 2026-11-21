package api

import (
	"context"
	"errors"
	"log/slog"
	"strings"

	"connectrpc.com/connect"

	hubv1 "github.com/asterism-dev/observatory/gen/go/observatory/hub/v1"
	"github.com/asterism-dev/observatory/gen/go/observatory/hub/v1/hubv1connect"
	"github.com/asterism-dev/observatory/internal/platform/uuidx"
	"github.com/asterism-dev/observatory/internal/store"
)

// ResourceServer serves ResourceService from the hub's store; the caller mounts it behind a service token.
type ResourceServer struct{ Store *store.Store }

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

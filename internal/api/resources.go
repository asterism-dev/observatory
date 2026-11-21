package api

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"strings"

	"connectrpc.com/connect"
	"github.com/google/uuid"

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

// ownsNothing answers 409 while subject owns VMs or live disks in org; ponytail: checked before and not under the org lock, so a concurrent create can orphan a resource.
func (h *api) ownsNothing(w http.ResponseWriter, r *http.Request, org uuid.UUID, subject string) bool {
	res, err := h.resources.CheckOwnership(r.Context(), connect.NewRequest(&hubv1.CheckOwnershipRequest{OrgId: org.String(), Subject: subject}))
	if err != nil {
		serverError(w, "check ownership", err)
		return false
	}
	if res.Msg.OwnsVms || res.Msg.OwnsDisks {
		writeError(w, http.StatusConflict, store.ErrOwnsResources.Error())
		return false
	}
	return true
}

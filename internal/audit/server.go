package audit

import (
	"context"
	"errors"
	"fmt"

	"connectrpc.com/connect"

	auditv1 "github.com/asterism-dev/observatory/gen/go/observatory/audit/v1"
	"github.com/asterism-dev/observatory/gen/go/observatory/audit/v1/auditv1connect"
	eventsv1 "github.com/asterism-dev/observatory/gen/go/observatory/events/v1"
	"github.com/asterism-dev/observatory/internal/platform/uuidx"
)

// server relies on the authz interceptor: a method reaches it only for callers allowed on the request's org or the platform.
type server struct {
	auditv1connect.UnimplementedAuditServiceHandler
	store *store
}

func (s *server) ListEvents(ctx context.Context, r *connect.Request[auditv1.ListEventsRequest]) (*connect.Response[auditv1.ListEventsResponse], error) {
	m := r.Msg
	events, next, err := s.list(ctx, query(m.GetOrg(), m.GetFilter(), m.GetPageSize(), m.GetPageToken()))
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(&auditv1.ListEventsResponse{Events: events, NextPageToken: next}), nil
}

func (s *server) ListAllEvents(ctx context.Context, r *connect.Request[auditv1.ListAllEventsRequest]) (*connect.Response[auditv1.ListAllEventsResponse], error) {
	m := r.Msg
	if m.GetFilterOrg() != "" {
		if _, err := uuidx.Canonical(m.GetFilterOrg()); err != nil {
			return nil, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("filter_org: %w", err))
		}
	}
	events, next, err := s.list(ctx, query(m.GetFilterOrg(), m.GetFilter(), m.GetPageSize(), m.GetPageToken()))
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(&auditv1.ListAllEventsResponse{Events: events, NextPageToken: next}), nil
}

func query(org string, f *auditv1.EventFilter, pageSize int32, pageToken string) listQuery {
	return listQuery{org: org, actorID: f.GetActorId(), typ: f.GetType(), result: f.GetResult(), from: f.GetFrom(), to: f.GetTo(), pageSize: pageSize, pageToken: pageToken}
}

func (s *server) list(ctx context.Context, q listQuery) ([]*eventsv1.Event, string, error) {
	events, next, err := s.store.list(ctx, q)
	if errors.Is(err, errBadPageToken) {
		return nil, "", connect.NewError(connect.CodeInvalidArgument, err)
	}
	if err != nil {
		return nil, "", internalErr("list events", err)
	}
	return events, next, nil
}

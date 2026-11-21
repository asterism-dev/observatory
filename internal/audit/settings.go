package audit

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"

	"connectrpc.com/connect"
	"github.com/jackc/pgx/v5"

	auditv1 "github.com/asterism-dev/observatory/gen/go/observatory/audit/v1"
	"github.com/asterism-dev/observatory/gen/go/observatory/audit/v1/auditv1connect"
	eventsv1 "github.com/asterism-dev/observatory/gen/go/observatory/events/v1"
	"github.com/asterism-dev/observatory/internal/platform/outbox"
)

// services record through auditlog.Interceptors and so honour data events; add a service here once it is extracted.
var services = []string{"audit"}

const (
	minRetentionDays = 30
	maxRetentionDays = 3650
)

// changed tells services to reload; not an audit event, since the interceptor records the update call itself.
func changed(ctx context.Context, tx pgx.Tx, org string) error {
	return outbox.Write(ctx, tx, &eventsv1.Event{Type: "audit.settings.changed", Org: org, Actor: &eventsv1.Actor{Kind: eventsv1.ActorKind_ACTOR_KIND_SERVICE, Id: "audit"}})
}

func (s *store) dataEvents(ctx context.Context, org string) (map[string]bool, error) {
	out := make(map[string]bool, len(services))
	for _, svc := range services {
		out[svc] = false
	}
	rows, err := s.pool.Query(ctx, "SELECT service, enabled FROM org_data_events WHERE org = $1", org)
	if err != nil {
		return nil, fmt.Errorf("data events: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var svc string
		var on bool
		if err := rows.Scan(&svc, &on); err != nil {
			return nil, fmt.Errorf("data events: %w", err)
		}
		out[svc] = on
	}
	return out, rows.Err()
}

func (s *store) setDataEvents(ctx context.Context, org string, on map[string]bool) error {
	return pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		for svc, enabled := range on {
			if _, err := tx.Exec(ctx, `INSERT INTO org_data_events (org, service, enabled) VALUES ($1, $2, $3)
				ON CONFLICT (org, service) DO UPDATE SET enabled = excluded.enabled`, org, svc, enabled); err != nil {
				return fmt.Errorf("set data events: %w", err)
			}
		}
		return changed(ctx, tx, org)
	})
}

func (s *store) enabledDataEvents(ctx context.Context) ([]*auditv1.DataEvents, error) {
	rows, err := s.pool.Query(ctx, "SELECT org::text, service FROM org_data_events WHERE enabled")
	if err != nil {
		return nil, fmt.Errorf("enabled data events: %w", err)
	}
	defer rows.Close()
	var out []*auditv1.DataEvents
	for rows.Next() {
		e := &auditv1.DataEvents{}
		if err := rows.Scan(&e.Org, &e.Service); err != nil {
			return nil, fmt.Errorf("enabled data events: %w", err)
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

func (s *store) retentionDays(ctx context.Context) (int, error) {
	var days int
	if err := s.pool.QueryRow(ctx, "SELECT retention_days FROM platform_settings").Scan(&days); err != nil {
		return 0, fmt.Errorf("retention days: %w", err)
	}
	return days, nil
}

func (s *store) setRetentionDays(ctx context.Context, days int) error {
	return pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, "UPDATE platform_settings SET retention_days = $1", days); err != nil {
			return fmt.Errorf("set retention days: %w", err)
		}
		return changed(ctx, tx, "")
	})
}

func internalErr(what string, err error) error {
	slog.Error("audit: "+what, "err", err)
	return connect.NewError(connect.CodeInternal, errors.New(what+" failed"))
}

func (s *server) GetSettings(ctx context.Context, r *connect.Request[auditv1.GetSettingsRequest]) (*connect.Response[auditv1.GetSettingsResponse], error) {
	on, err := s.store.dataEvents(ctx, r.Msg.GetOrg())
	if err != nil {
		return nil, internalErr("get settings", err)
	}
	return connect.NewResponse(&auditv1.GetSettingsResponse{DataEvents: on}), nil
}

func (s *server) UpdateSettings(ctx context.Context, r *connect.Request[auditv1.UpdateSettingsRequest]) (*connect.Response[auditv1.UpdateSettingsResponse], error) {
	for svc := range r.Msg.GetDataEvents() {
		if !slices.Contains(services, svc) {
			return nil, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("unknown service %q", svc))
		}
	}
	if err := s.store.setDataEvents(ctx, r.Msg.GetOrg(), r.Msg.GetDataEvents()); err != nil {
		return nil, internalErr("update settings", err)
	}
	on, err := s.store.dataEvents(ctx, r.Msg.GetOrg())
	if err != nil {
		return nil, internalErr("get settings", err)
	}
	return connect.NewResponse(&auditv1.UpdateSettingsResponse{DataEvents: on}), nil
}

func (s *server) GetPlatformSettings(ctx context.Context, _ *connect.Request[auditv1.GetPlatformSettingsRequest]) (*connect.Response[auditv1.GetPlatformSettingsResponse], error) {
	days, err := s.store.retentionDays(ctx)
	if err != nil {
		return nil, internalErr("get platform settings", err)
	}
	return connect.NewResponse(&auditv1.GetPlatformSettingsResponse{RetentionDays: int32(days)}), nil
}

func (s *server) UpdatePlatformSettings(ctx context.Context, r *connect.Request[auditv1.UpdatePlatformSettingsRequest]) (*connect.Response[auditv1.UpdatePlatformSettingsResponse], error) {
	days := r.Msg.GetRetentionDays()
	if days < minRetentionDays || days > maxRetentionDays {
		return nil, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("retention_days must be %d to %d", minRetentionDays, maxRetentionDays))
	}
	if err := s.store.setRetentionDays(ctx, int(days)); err != nil {
		return nil, internalErr("update platform settings", err)
	}
	return connect.NewResponse(&auditv1.UpdatePlatformSettingsResponse{RetentionDays: days}), nil
}

// internalServer is reachable only under /internal/audit/ with a service token, never through the edge.
type internalServer struct {
	auditv1connect.UnimplementedAuditInternalServiceHandler
	store *store
}

func (s *internalServer) GetDataEvents(ctx context.Context, _ *connect.Request[auditv1.GetDataEventsRequest]) (*connect.Response[auditv1.GetDataEventsResponse], error) {
	on, err := s.store.enabledDataEvents(ctx)
	if err != nil {
		return nil, internalErr("get data events", err)
	}
	return connect.NewResponse(&auditv1.GetDataEventsResponse{Enabled: on}), nil
}

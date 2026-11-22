package quota

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"slices"
	"strings"

	"connectrpc.com/connect"
	"github.com/google/uuid"
	"google.golang.org/protobuf/types/known/timestamppb"

	quotav1 "github.com/asterism-dev/observatory/gen/go/observatory/quota/v1"
	"github.com/asterism-dev/observatory/gen/go/observatory/quota/v1/quotav1connect"
	coreauthz "github.com/asterism-dev/observatory/internal/authz"
	"github.com/asterism-dev/observatory/internal/platform/quotaclient"
	"github.com/asterism-dev/observatory/internal/platform/servicetoken"
	"github.com/asterism-dev/observatory/internal/platform/uuidx"
)

// internalPrefix serves other services; the edge never routes /internal/.
const internalPrefix = "/internal/quota"

// internalCalls lists the QuotaInternalService procedures each calling service may use; any other caller is refused everything.
var internalCalls = map[string][]string{
	"hub": {quotav1connect.QuotaInternalServiceReserveProcedure, quotav1connect.QuotaInternalServiceCancelProcedure,
		quotav1connect.QuotaInternalServiceReleaseProcedure, quotav1connect.QuotaInternalServiceGetLimitsProcedure},
}

// internalHandler serves /internal/quota/* to the callers internalCalls admits, with a capped body.
func internalHandler(st *store, tokens map[string]string) http.Handler {
	_, h := quotav1connect.NewQuotaInternalServiceHandler(&internalServer{store: st}, connect.WithReadMaxBytes(maxBody))
	allowed := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		caller, _ := servicetoken.Caller(r.Context())
		if !slices.Contains(internalCalls[caller], r.URL.Path) {
			slog.Warn("internal call outside the caller's allow list", "caller", caller, "path", r.URL.Path)
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		h.ServeHTTP(w, r)
	})
	return http.MaxBytesHandler(servicetoken.Middleware(tokens)(http.StripPrefix(internalPrefix, allowed)), maxBody)
}

type internalServer struct {
	quotav1connect.UnimplementedQuotaInternalServiceHandler
	store *store
}

func (s *internalServer) Reserve(ctx context.Context, r *connect.Request[quotav1.ReserveRequest]) (*connect.Response[quotav1.ReserveResponse], error) {
	org, err := parseOrg("org_id", r.Msg.GetOrgId())
	if err != nil {
		return nil, err
	}
	owner := r.Msg.GetOwner()
	if !validOwner(owner) {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("owner must be user:<uuid> or team:<uuid>"))
	}
	a := r.Msg.GetAmounts()
	if a == nil {
		a = &quotav1.Amounts{}
	}
	add := Amounts{VCPU: a.Vcpu, RAMMB: a.RamMb, DiskGB: a.DiskGb, VMCount: a.VmCount, SnapshotCount: a.SnapshotCount}
	for _, v := range add.fields() {
		if v != nil && *v < 0 {
			return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("amounts must not be negative"))
		}
	}
	id, expires, err := s.store.Reserve(ctx, org, owner, add)
	var le *limitError
	switch {
	case errors.As(err, &le):
		return nil, quotaclient.NewLimitExceeded(le.detail())
	case err != nil:
		return nil, internalErr("reserve", err)
	}
	return connect.NewResponse(&quotav1.ReserveResponse{ReservationId: id.String(), ExpiresAt: timestamppb.New(expires)}), nil
}

func (s *internalServer) Cancel(ctx context.Context, r *connect.Request[quotav1.CancelRequest]) (*connect.Response[quotav1.CancelResponse], error) {
	ids := make([]uuid.UUID, 0, len(r.Msg.GetReservationIds()))
	for _, raw := range r.Msg.GetReservationIds() {
		id, err := uuidx.Canonical(raw)
		if err != nil {
			return nil, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("reservation_ids: %w", err))
		}
		ids = append(ids, id)
	}
	if err := s.store.Cancel(ctx, ids); err != nil {
		return nil, internalErr("cancel", err)
	}
	return connect.NewResponse(&quotav1.CancelResponse{}), nil
}

func (s *internalServer) Release(ctx context.Context, r *connect.Request[quotav1.ReleaseRequest]) (*connect.Response[quotav1.ReleaseResponse], error) {
	org, err := parseOrg("org_id", r.Msg.GetOrgId())
	if err != nil {
		return nil, err
	}
	for _, res := range r.Msg.GetResources() {
		if !validResource(res) {
			return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("resources must be vm/<uuid>, disk/<uuid> or snapshot/<uuid>"))
		}
	}
	if err := s.store.Release(ctx, org, r.Msg.GetResources()); err != nil {
		return nil, internalErr("release", err)
	}
	return connect.NewResponse(&quotav1.ReleaseResponse{}), nil
}

func (s *internalServer) GetLimits(ctx context.Context, r *connect.Request[quotav1.QuotaInternalServiceGetLimitsRequest]) (*connect.Response[quotav1.QuotaInternalServiceGetLimitsResponse], error) {
	org, err := parseOrg("org_id", r.Msg.GetOrgId())
	if err != nil {
		return nil, err
	}
	ls, err := orgLimits(ctx, s.store, org)
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(&quotav1.QuotaInternalServiceGetLimitsResponse{Limits: ls}), nil
}

func validOwner(owner string) bool {
	kind, raw, _ := strings.Cut(owner, ":")
	id, err := uuidx.Canonical(raw)
	return err == nil && (kind == "user" && owner == coreauthz.UserSubject(id) || kind == "team" && owner == coreauthz.TeamSubject(id))
}

func validResource(res string) bool {
	kind, raw, _ := strings.Cut(res, "/")
	_, err := uuidx.Canonical(raw)
	return err == nil && (kind == "vm" || kind == "disk" || kind == "snapshot")
}

// Package iam serves the public IAMService over Connect and REST; it runs inside the hub on the hub's store for now.
package iam

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/url"

	"connectrpc.com/connect"
	"connectrpc.com/vanguard"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/asterism-dev/observatory/gen/go/observatory/hub/v1/hubv1connect"
	"github.com/asterism-dev/observatory/gen/go/observatory/iam/v1/iamv1connect"
	"github.com/asterism-dev/observatory/internal/authz"
	"github.com/asterism-dev/observatory/internal/platform/auditlog"
	"github.com/asterism-dev/observatory/internal/platform/identity"
	"github.com/asterism-dev/observatory/internal/platform/rest"
	"github.com/asterism-dev/observatory/internal/platform/textx"
	"github.com/asterism-dev/observatory/internal/store"
)

// Prefix is where the hub mounts Handler.
const Prefix = "/api/iam/v1"

const maxBody = 1 << 20

type Deps struct {
	Store     *store.Store
	Policy    *authz.Service
	Verifier  *identity.Verifier
	Resources hubv1connect.ResourceServiceClient
	Pool      *pgxpool.Pool
	// Settings nil means no data events.
	Settings *auditlog.Settings
	// PublicURL builds invite links.
	PublicURL *url.URL
	// Revision is the latest revision of Policy's stream; CanHold waits for the hub to reach it.
	Revision func() uint64
}

type server struct {
	iamv1connect.UnimplementedIAMServiceHandler
	Deps
}

// Handler serves IAMService under Prefix; writes record their own audit events inside their transaction.
func Handler(d Deps) (http.Handler, error) {
	if d.Revision == nil {
		return nil, errors.New("iam: Deps.Revision is required")
	}
	interceptors := append(auditlog.Interceptors("iam", d.Verifier, d.Policy, d.Settings, d.Pool, auditlog.HandlerRecordsWrites()), sessionActive(d.Store))
	// Bodies are read before the interceptors run, so the cap is what keeps unauthenticated callers from filling memory.
	path, h := iamv1connect.NewIAMServiceHandler(&server{Deps: d}, connect.WithInterceptors(interceptors...), connect.WithReadMaxBytes(maxBody))
	tc, err := vanguard.NewTranscoder([]*vanguard.Service{vanguard.NewService(path, h, vanguard.WithMaxMessageBufferBytes(maxBody))})
	if err != nil {
		return nil, err
	}
	// Connect drains an oversized body to the end; MaxBytesHandler stops reading at the cap instead.
	return http.MaxBytesHandler(http.StripPrefix(Prefix, rest.NoQuery(path, tc)), maxBody), nil
}

// ponytail: one session lookup per call so logout and revoke apply at once, as in auth.Require; 5b-2 moves sessions into iam's own store.
func sessionActive(s *store.Store) connect.UnaryInterceptorFunc {
	return func(next connect.UnaryFunc) connect.UnaryFunc {
		return func(ctx context.Context, req connect.AnyRequest) (connect.AnyResponse, error) {
			active, err := s.SessionActive(ctx, caller(ctx).Session)
			if err != nil {
				slog.Error("session lookup", "err", err)
				return nil, connect.NewError(connect.CodeInternal, errors.New("internal error"))
			}
			if !active {
				return nil, connect.NewError(connect.CodeUnauthenticated, errors.New("session ended"))
			}
			return next(ctx, req)
		}
	}
}

// caller is the principal the authz interceptor verified; every handler runs behind it.
func caller(ctx context.Context) identity.Principal {
	p, _ := identity.FromContext(ctx)
	return p
}

func auditEntry(ctx context.Context, h http.Header, org *uuid.UUID, action, resource string, result store.Result) store.AuditEntry {
	return store.AuditEntry{ActorKind: store.ActorUser, ActorID: caller(ctx).User.String(), OrgID: org,
		Action: action, Resource: resource, Result: result, IP: identity.ClientIP(ctx), UserAgent: textx.Sanitize(h.Get("User-Agent"))}
}

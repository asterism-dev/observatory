// Package iam serves the public IAMService over Connect, gRPC and REST; it runs inside the hub on the hub's store for now.
package iam

import (
	"context"
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

type Deps struct {
	Store     *store.Store
	Policy    *authz.Service
	Verifier  *identity.Verifier
	Resources hubv1connect.ResourceServiceClient
	// Notify re-pushes an org's state to its nodes after a change that reaches them.
	Notify func(org uuid.UUID)
	Pool   *pgxpool.Pool
	// Settings nil means no data events.
	Settings *auditlog.Settings
	// PublicURL builds invite links.
	PublicURL *url.URL
}

type server struct {
	iamv1connect.UnimplementedIAMServiceHandler
	Deps
}

// Handler serves IAMService under Prefix; writes record their own audit events inside their transaction.
func Handler(d Deps) (http.Handler, error) {
	path, h := iamv1connect.NewIAMServiceHandler(&server{Deps: d}, connect.WithInterceptors(
		auditlog.Interceptors("iam", d.Verifier, d.Policy, d.Settings, d.Pool, auditlog.HandlerRecordsWrites())...))
	tc, err := vanguard.NewTranscoder([]*vanguard.Service{vanguard.NewService(path, h)})
	if err != nil {
		return nil, err
	}
	return http.StripPrefix(Prefix, rest.NoQuery(path, tc)), nil
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

// notifyUserOrgs re-pushes every org of user, since its keys reach the VMs it owns or shares through teams.
func (s *server) notifyUserOrgs(user uuid.UUID) {
	for _, org := range s.Policy.Enforcer().OrgsOf(user) {
		s.Notify(org)
	}
}

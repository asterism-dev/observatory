package iam

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"connectrpc.com/connect"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	hubv1 "github.com/asterism-dev/observatory/gen/go/observatory/hub/v1"
	"github.com/asterism-dev/observatory/gen/go/observatory/hub/v1/hubv1connect"
	"github.com/asterism-dev/observatory/gen/go/observatory/iam/v1/iamv1connect"
	"github.com/asterism-dev/observatory/internal/authz"
	"github.com/asterism-dev/observatory/internal/iam/store"
	"github.com/asterism-dev/observatory/internal/platform/auditlog"
	platformauthz "github.com/asterism-dev/observatory/internal/platform/authz"
	"github.com/asterism-dev/observatory/internal/platform/bus"
	"github.com/asterism-dev/observatory/internal/platform/outbox"
	"github.com/asterism-dev/observatory/internal/platform/servicetoken"
	"github.com/asterism-dev/observatory/internal/seal"
)

// loginPolicyWait bounds how long a login waits for its new personal org to reach the hub's policy.
const loginPolicyWait = 5 * time.Second

type Config struct {
	DatabaseURL, PublicURL, OIDCIssuer, WebClientID, CLIClientID, EdgeToken string
	// HubURL and ServiceToken reach the hub's /internal/hub/; AuditURL (optional) the audit service's /internal/audit/.
	HubURL, ServiceToken, AuditURL string
	NATSURL, NATSToken             string
	// SecretsKey seals the JWT signing key.
	SecretsKey seal.Key
	// ServiceTokens (name → token) authenticate other services' calls to /internal/iam/.
	ServiceTokens map[string]string
}

type Service struct {
	cfg      Config
	pool     *pgxpool.Pool
	settings *auditlog.Settings
	handler  http.Handler
}

// New migrates schema iam and builds iam's handlers; Run relays its events.
func New(ctx context.Context, cfg Config) (*Service, error) {
	publicURL, err := url.Parse(cfg.PublicURL)
	if err != nil || publicURL.Scheme == "" || publicURL.Host == "" {
		return nil, errors.New("iam: public url must be absolute")
	}
	if cfg.HubURL == "" || cfg.ServiceToken == "" {
		return nil, errors.New("iam: hub url and service token are required")
	}
	if cfg.SecretsKey == (seal.Key{}) {
		return nil, errors.New("iam: secrets key is required")
	}
	pool, err := store.Open(ctx, cfg.DatabaseURL)
	if err != nil {
		return nil, err
	}
	svc, err := build(ctx, cfg, pool, publicURL)
	if err != nil {
		pool.Close()
		return nil, err
	}
	return svc, nil
}

func build(ctx context.Context, cfg Config, pool *pgxpool.Pool, publicURL *url.URL) (*Service, error) {
	s := store.New(pool)
	iss, err := NewIssuer(ctx, s, cfg.SecretsKey, strings.TrimRight(cfg.PublicURL, "/"))
	if err != nil {
		return nil, err
	}
	z, err := authz.NewService(ctx, s.PolicySnapshot)
	if err != nil {
		return nil, err
	}
	policies := platformauthz.NewPublisher()
	z.OnReload(func(_, p authz.Policy) { policies.Publish(p) })
	oidc, err := NewOIDC(ctx, cfg.OIDCIssuer, cfg.WebClientID, cfg.CLIClientID, strings.TrimRight(cfg.PublicURL, "/")+"/auth/callback")
	if err != nil {
		return nil, err
	}
	resources := hubv1connect.NewResourceServiceClient(&http.Client{Timeout: 30 * time.Second}, strings.TrimRight(cfg.HubURL, "/")+"/internal/hub",
		connect.WithInterceptors(servicetoken.Bearer(cfg.ServiceToken)))
	var settings *auditlog.Settings
	if cfg.AuditURL != "" {
		if settings, err = auditlog.NewSettings(strings.TrimRight(cfg.AuditURL, "/")+"/internal/audit", cfg.ServiceToken); err != nil {
			return nil, err
		}
	}
	api, err := Handler(Deps{Store: s, Policy: z, Verifier: iss.Verifier(), Resources: resources, Pool: pool, Settings: settings, PublicURL: publicURL, Revision: policies.Revision})
	if err != nil {
		return nil, err
	}
	login := &loginHandler{Store: s, Auth: oidc, PublicURL: publicURL, Issuer: cfg.OIDCIssuer, CLIClientID: cfg.CLIClientID, EdgeToken: cfg.EdgeToken,
		Reload: func(ctx context.Context, user, org uuid.UUID) error {
			if err := z.Reload(ctx); err != nil {
				return err
			}
			// The SPA calls hub routes right after login; CanHold without rules answers once the hub has this revision.
			wctx, cancel := context.WithTimeout(ctx, loginPolicyWait)
			defer cancel()
			_, err := resources.CanHold(wctx, connect.NewRequest(&hubv1.CanHoldRequest{OrgId: org.String(), Actor: authz.UserSubject(user), MinRevision: policies.Revision()}))
			return err
		}}
	if len(cfg.ServiceTokens) == 0 {
		slog.Warn("iam: no service tokens configured; every /internal/iam/ call is refused")
	}
	mux := http.NewServeMux()
	mux.Handle(Prefix+"/", api)
	login.Routes(mux)
	mux.Handle("/internal/iam/", servicetoken.Middleware(cfg.ServiceTokens)(internalHandler(iss, policies, internalServer{Store: s})))
	return &Service{cfg: cfg, pool: pool, settings: settings, handler: mux}, nil
}

// internalHandler serves /internal/iam/*; the caller wraps it in servicetoken.Middleware.
func internalHandler(iss *Issuer, ps *platformauthz.Publisher, iam iamv1connect.IAMInternalServiceHandler) http.Handler {
	mux := http.NewServeMux()
	mux.Handle("/internal/iam/token", iss.TokenHandler())
	mux.Handle("/internal/iam/jwks", iss.JWKSHandler())
	path, h := iamv1connect.NewIAMInternalServiceHandler(iam)
	mux.Handle("/internal/iam"+path, http.StripPrefix("/internal/iam", h))
	mux.Handle(ps.Handler("/internal/iam"))
	return mux
}

// Handler serves /api/iam/v1/, /internal/iam/, /auth/ and /api/auth/.
func (s *Service) Handler() http.Handler { return s.handler }

// Run relays iam's outbox to the bus and follows the audit settings until ctx ends; until the bus is reachable, events wait in the outbox.
func (s *Service) Run(ctx context.Context) error {
	js, closeBus, err := bus.Await(ctx, s.cfg.NATSURL, s.cfg.NATSToken)
	if err != nil {
		if ctx.Err() != nil {
			return nil
		}
		return fmt.Errorf("iam: event bus: %w", err)
	}
	defer closeBus()
	for wait := time.Second; ; wait = min(2*wait, 30*time.Second) {
		err := bus.EnsureStream(ctx, js)
		if err == nil {
			break
		}
		if ctx.Err() != nil {
			return nil
		}
		slog.Warn("iam: event stream unavailable, events wait in the outbox", "err", err, "retry_in", wait)
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(wait):
		}
	}
	var wg sync.WaitGroup
	if s.settings != nil {
		wg.Go(func() { s.settings.Run(ctx, js.Conn()) })
	}
	outbox.NewRelay(s.pool, js).Run(ctx)
	wg.Wait()
	return nil
}

func (s *Service) Close() { s.pool.Close() }

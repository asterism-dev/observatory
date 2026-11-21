package hubserver

import (
	"context"
	"log/slog"
	"maps"
	"net/http"
	"slices"
	"strings"

	"github.com/asterism-dev/observatory/gen/go/observatory/iam/v1/iamv1connect"
	"github.com/asterism-dev/observatory/internal/auth"
	"github.com/asterism-dev/observatory/internal/authz"
	"github.com/asterism-dev/observatory/internal/iaminternal"
	platformauthz "github.com/asterism-dev/observatory/internal/platform/authz"
	"github.com/asterism-dev/observatory/internal/platform/servicetoken"
	"github.com/asterism-dev/observatory/internal/store"
)

// selfService names the token the hub's own verifier fetches the JWKS with.
const selfService = "hub"

// mountIdentity serves iam's identity issuer and policy stream under /internal/iam/, fed by every successful reload of iam's z.
func mountIdentity(ctx context.Context, mux *http.ServeMux, s *store.Store, z *authz.Service, cfg config) (*platformauthz.Publisher, error) {
	jwtIssuer, err := auth.NewIssuer(ctx, s, strings.TrimRight(cfg.PublicURL, "/"))
	if err != nil {
		return nil, err
	}
	policies := platformauthz.NewPublisher()
	z.OnReload(func(_, p authz.Policy) { policies.Publish(p) })
	if !slices.ContainsFunc(slices.Collect(maps.Keys(cfg.ServiceTokens)), func(name string) bool { return name != selfService }) {
		slog.Warn("OBSERVATORY_SERVICE_TOKENS is not set; the hub refuses every /internal/iam/ call")
	}
	mux.Handle("/internal/iam/", servicetoken.Middleware(cfg.ServiceTokens)(internalIAM(jwtIssuer, policies, iaminternal.Server{Store: s})))
	return policies, nil
}

// internalIAM serves /internal/iam/*; the caller wraps it in servicetoken.Middleware.
func internalIAM(iss *auth.Issuer, ps *platformauthz.Publisher, iam iamv1connect.IAMInternalServiceHandler) http.Handler {
	mux := http.NewServeMux()
	mux.Handle("/internal/iam/token", iss.TokenHandler())
	mux.Handle("/internal/iam/jwks", iss.JWKSHandler())
	iamPath, iamHandler := iamv1connect.NewIAMInternalServiceHandler(iam)
	mux.Handle("/internal/iam"+iamPath, http.StripPrefix("/internal/iam", iamHandler))
	mux.Handle(ps.Handler("/internal/iam"))
	return mux
}

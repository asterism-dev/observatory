// Package auth admits hub requests that carry an identity JWT the edge attached, checking the session at iam.
package auth

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/asterism-dev/observatory/gen/go/observatory/iam/v1/iamv1connect"
	"github.com/asterism-dev/observatory/internal/platform/clientip"
	"github.com/asterism-dev/observatory/internal/platform/iamclient"
	"github.com/asterism-dev/observatory/internal/platform/identity"
	"github.com/asterism-dev/observatory/internal/platform/textx"
)

const SessionCLI = "cli"

type User struct {
	ID            uuid.UUID
	Name          string
	PlatformAdmin bool
}

type Session struct {
	ID        uuid.UUID
	Kind      string
	ExpiresAt time.Time
}

type Principal struct {
	User    User
	Session Session
}

type principalKey struct{}

func PrincipalFrom(ctx context.Context) (Principal, bool) {
	p, ok := ctx.Value(principalKey{}).(Principal)
	return p, ok
}

type Handler struct {
	PublicURL *url.URL
	// EdgeToken authenticates the edge's client IP header; TrustEdge resolves the client IP with it once per request.
	EdgeToken string
	// Verifier checks the JWT the edge attaches to /api/ requests.
	Verifier Verifier
	// IAM answers Require's per-request session check.
	IAM iamv1connect.IAMInternalServiceClient
}

type clientIPKey struct{}

// TrustEdge stores the client IP for RequestMeta, taking the edge's header only when it carries h.EdgeToken.
func (h *Handler) TrustEdge(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), clientIPKey{}, clientip.From(r, h.EdgeToken))))
	})
}

// RequestMeta returns the client IP resolved by TrustEdge and the sanitized user agent; X-Forwarded-For is deliberately not trusted.
func RequestMeta(r *http.Request) (ip, userAgent string) {
	ip, ok := r.Context().Value(clientIPKey{}).(string)
	if !ok {
		ip = clientip.From(r, "")
	}
	return ip, textx.Sanitize(r.UserAgent())
}

// Verifier checks an edge-attached identity JWT.
type Verifier interface {
	Verify(ctx context.Context, raw string) (identity.Principal, error)
}

// Require accepts only a JWT the edge attached; CSRF is the edge's job, it refuses cookie writes without X-Observatory before swapping.
func (h *Handler) Require(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		scheme, raw, ok := strings.Cut(r.Header.Get("Authorization"), " ")
		if !ok || !strings.EqualFold(scheme, "Bearer") || strings.TrimSpace(raw) == "" {
			writeError(w, http.StatusUnauthorized, "unauthorized")
			return
		}
		p, err := h.Verifier.Verify(r.Context(), strings.TrimSpace(raw))
		if errors.Is(err, identity.ErrNoKey) {
			// Anyone can send an unknown kid, so a Warn here would be an unauthenticated log flood.
			slog.Debug("jwt verification: no matching key", "err", err)
		}
		// Tokens without sk/sx predate the hub seam and cannot fill the session the handlers rely on.
		if err == nil && (p.SessionKind == "" || p.SessionExpires.IsZero()) {
			err = errors.New("missing session claims")
		}
		if err != nil {
			writeError(w, http.StatusUnauthorized, "unauthorized")
			return
		}
		// ponytail: one iam call per hub request so logout and revoke apply at once; cache briefly if iam's load matters.
		active, err := iamclient.SessionActive(r.Context(), h.IAM, p.Session)
		if err != nil {
			slog.Error("session check at iam", "err", err)
			writeError(w, http.StatusServiceUnavailable, "session check unavailable")
			return
		}
		if !active {
			writeError(w, http.StatusUnauthorized, "unauthorized")
			return
		}
		ctx := context.WithValue(r.Context(), principalKey{}, Principal{
			User:    User{ID: p.User, Name: p.Name, PlatformAdmin: p.PlatformAdmin},
			Session: Session{ID: p.Session, Kind: p.SessionKind, ExpiresAt: p.SessionExpires},
		})
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

func writeError(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": msg})
}

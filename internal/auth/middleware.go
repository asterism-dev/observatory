package auth

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strings"

	"github.com/asterism-dev/observatory/internal/platform/clientip"
	"github.com/asterism-dev/observatory/internal/platform/identity"
	"github.com/asterism-dev/observatory/internal/platform/textx"
	"github.com/asterism-dev/observatory/internal/store"
)

const sessionCookie = "obs_session"

type Principal struct {
	User    store.User
	Session store.Session
}

type principalKey struct{}

func PrincipalFrom(ctx context.Context) (Principal, bool) {
	p, ok := ctx.Value(principalKey{}).(Principal)
	return p, ok
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
			slog.Warn("jwt verification: no matching key", "err", err)
		}
		// Tokens without sk/sx predate the hub seam and cannot fill the session the handlers rely on.
		if err == nil && (p.SessionKind == "" || p.SessionExpires.IsZero()) {
			err = errors.New("missing session claims")
		}
		if err != nil {
			writeError(w, http.StatusUnauthorized, "unauthorized")
			return
		}
		ctx := context.WithValue(r.Context(), principalKey{}, Principal{
			User:    store.User{ID: p.User, Name: p.Name, PlatformAdmin: p.PlatformAdmin},
			Session: store.Session{ID: p.Session, UserID: p.User, Kind: store.SessionKind(p.SessionKind), ExpiresAt: p.SessionExpires},
		})
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// requireSession guards /auth/ routes, which the edge forwards with the raw session.
func (h *Handler) requireSession(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		token, fromCookie := credentials(r)
		if token == "" {
			writeError(w, http.StatusUnauthorized, "unauthorized")
			return
		}
		sess, err := h.Store.SessionByToken(r.Context(), token)
		if errors.Is(err, store.ErrNotFound) {
			writeError(w, http.StatusUnauthorized, "unauthorized")
			return
		}
		if err != nil {
			slog.Error("session lookup", "err", err)
			writeError(w, http.StatusInternalServerError, "internal error")
			return
		}
		// Cross-site forms can ride the cookie but cannot set custom headers without a CORS preflight.
		if fromCookie && r.Method != http.MethodGet && r.Method != http.MethodHead && r.Header.Get("X-Observatory") != "1" {
			writeError(w, http.StatusForbidden, "missing X-Observatory header")
			return
		}
		ctx := context.WithValue(r.Context(), principalKey{}, Principal{User: store.User{ID: sess.UserID}, Session: sess})
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// credentials prefers an Authorization header; a present but malformed one is not replaced by the cookie.
func credentials(r *http.Request) (token string, fromCookie bool) {
	if authz := r.Header.Get("Authorization"); authz != "" {
		scheme, tok, ok := strings.Cut(authz, " ")
		if !ok || !strings.EqualFold(scheme, "Bearer") {
			return "", false
		}
		return strings.TrimSpace(tok), false
	}
	if c, err := r.Cookie(sessionCookie); err == nil {
		return c.Value, true
	}
	return "", false
}

func writeError(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": msg})
}

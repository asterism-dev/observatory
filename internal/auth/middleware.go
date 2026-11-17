package auth

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"strings"

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

// RequestMeta returns the peer IP; X-Forwarded-For is deliberately not trusted.
func RequestMeta(r *http.Request) (ip, userAgent string) {
	ip, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		ip = r.RemoteAddr
	}
	return ip, r.UserAgent()
}

func (h *Handler) Require(next http.Handler) http.Handler {
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
			writeError(w, http.StatusInternalServerError, "internal error")
			return
		}
		// Cross-site forms can ride the cookie but cannot set custom headers without a CORS preflight.
		if fromCookie && r.Method != http.MethodGet && r.Method != http.MethodHead && r.Header.Get("X-Observatory") != "1" {
			writeError(w, http.StatusForbidden, "missing X-Observatory header")
			return
		}
		user, err := h.Store.User(r.Context(), sess.UserID)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "internal error")
			return
		}
		ctx := context.WithValue(r.Context(), principalKey{}, Principal{User: user, Session: sess})
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

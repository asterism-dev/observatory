package edge

import (
	"bytes"
	"container/list"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"
)

const (
	sessionCookie = "obs_session"
	jwtCacheSize  = 10000
	// Refetching early keeps a cached JWT from lapsing in flight or inside a service's clock skew allowance.
	jwtRefreshMargin = 60 * time.Second
	// Bounds how long a logout or revoked session keeps reaching services through a cached JWT.
	jwtCacheTTL = 60 * time.Second
)

var errUnauthorized = errors.New("session rejected by issuer")

type jwtKey struct{}

// hubIdentity is what the hub proxy forwards for an /api/ request: the swapped JWT, or none once the issuer refused the session.
type hubIdentity struct {
	jwt     string
	swapped bool
}

type hubIdentityKey struct{}

// identity swaps the client's session token for a short-lived JWT from the hub's issuer.
type identity struct {
	tokenURL, serviceToken string
	client                 *http.Client
	cache                  *jwtCache
}

func (id *identity) middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		session, fromCookie := credentials(r)
		if session == "" {
			writeError(w, http.StatusUnauthorized, "unauthorized")
			return
		}
		// Same CSRF rule as the hub: services never see the cookie, so the edge must enforce it.
		if fromCookie && r.Method != http.MethodGet && r.Method != http.MethodHead && r.Header.Get("X-Observatory") != "1" {
			writeError(w, http.StatusForbidden, "missing X-Observatory header")
			return
		}
		jwt, err := id.jwt(r.Context(), session)
		if errors.Is(err, errUnauthorized) {
			writeError(w, http.StatusUnauthorized, "unauthorized")
			return
		}
		if err != nil {
			slog.Error("edge: identity token", "err", err)
			writeError(w, http.StatusBadGateway, "identity unavailable")
			return
		}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), jwtKey{}, jwt)))
	})
}

// hubMiddleware swaps a session for a JWT on hub /api/ routes; other bearers (node, gateway, storage, ...) pass unchanged.
func (id *identity) hubMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		session, fromCookie := credentials(r)
		if !isSession(session) {
			next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), hubIdentityKey{}, hubIdentity{})))
			return
		}
		if fromCookie && r.Method != http.MethodGet && r.Method != http.MethodHead && r.Header.Get("X-Observatory") != "1" {
			writeError(w, http.StatusForbidden, "missing X-Observatory header")
			return
		}
		jwt, err := id.jwt(r.Context(), session)
		// A stale session is dropped, not refused, so public routes like /api/auth/config still work; protected ones answer 401.
		if err != nil && !errors.Is(err, errUnauthorized) {
			slog.Error("edge: identity token", "err", err)
			writeError(w, http.StatusBadGateway, "identity unavailable")
			return
		}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), hubIdentityKey{}, hubIdentity{jwt: jwt, swapped: true})))
	})
}

func isSession(token string) bool {
	return strings.HasPrefix(token, "obs_")
}

func (id *identity) jwt(ctx context.Context, session string) (string, error) {
	key := sha256.Sum256([]byte(session))
	if jwt, ok := id.cache.get(key, time.Now()); ok {
		return jwt, nil
	}
	body, err := json.Marshal(map[string]string{"session_token": session})
	if err != nil {
		return "", err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, id.tokenURL, bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+id.serviceToken)
	req.Header.Set("Content-Type", "application/json")
	resp, err := id.client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		// Draining lets the connection be reused.
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
	}
	switch resp.StatusCode {
	case http.StatusOK:
	case http.StatusUnauthorized:
		return "", errUnauthorized
	case http.StatusForbidden:
		return "", errors.New("issuer rejected the edge's service token (check OBSERVATORY_SERVICE_TOKEN)")
	default:
		return "", fmt.Errorf("issuer answered %s", resp.Status)
	}
	var out struct {
		Token     string    `json:"token"`
		ExpiresAt time.Time `json:"expires_at"`
	}
	// The decode error is dropped: it could quote part of the token.
	if err := json.NewDecoder(io.LimitReader(resp.Body, 64<<10)).Decode(&out); err != nil || out.Token == "" {
		return "", errors.New("issuer sent an invalid response")
	}
	id.cache.add(key, out.Token, cacheUntil(out.ExpiresAt, time.Now()))
	return out.Token, nil
}

func cacheUntil(exp, now time.Time) time.Time {
	until := exp.Add(-jwtRefreshMargin)
	if limit := now.Add(jwtCacheTTL); limit.Before(until) {
		return limit
	}
	return until
}

// credentials mirrors the hub's: an Authorization header wins, and a malformed one is not replaced by the cookie.
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

// jwtCache is a bounded LRU of JWTs keyed by the SHA-256 of the session token, so raw session tokens are not kept.
type jwtCache struct {
	mu    sync.Mutex
	max   int
	order *list.List
	items map[[32]byte]*list.Element
}

type jwtEntry struct {
	key   [32]byte
	jwt   string
	until time.Time
}

func newJWTCache(max int) *jwtCache {
	return &jwtCache{max: max, order: list.New(), items: map[[32]byte]*list.Element{}}
}

func (c *jwtCache) get(key [32]byte, now time.Time) (string, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.items[key]
	if !ok {
		return "", false
	}
	ent := e.Value.(*jwtEntry)
	if !now.Before(ent.until) {
		c.order.Remove(e)
		delete(c.items, key)
		return "", false
	}
	c.order.MoveToFront(e)
	return ent.jwt, true
}

func (c *jwtCache) add(key [32]byte, jwt string, until time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if e, ok := c.items[key]; ok {
		*e.Value.(*jwtEntry) = jwtEntry{key, jwt, until}
		c.order.MoveToFront(e)
		return
	}
	c.items[key] = c.order.PushFront(&jwtEntry{key, jwt, until})
	if c.order.Len() > c.max {
		oldest := c.order.Back()
		c.order.Remove(oldest)
		delete(c.items, oldest.Value.(*jwtEntry).key)
	}
}

package identity

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sync"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
)

const (
	audience        = "observatory"
	leeway          = 30 * time.Second
	minRefreshGap   = 30 * time.Second
	maxKeyAge       = 10 * time.Minute
	maxJWKSBodySize = 1 << 20
)

type Principal struct {
	User          uuid.UUID
	Session       uuid.UUID
	PlatformAdmin bool
}

type principalKey struct{}

func WithPrincipal(ctx context.Context, p Principal) context.Context {
	return context.WithValue(ctx, principalKey{}, p)
}

func FromContext(ctx context.Context) (Principal, bool) {
	p, ok := ctx.Value(principalKey{}).(Principal)
	return p, ok
}

type claims struct {
	jwt.RegisteredClaims
	Session       string `json:"sid"`
	PlatformAdmin bool   `json:"pa"`
}

// Verifier validates identity JWTs against the hub's JWKS, caching keys between refreshes.
type Verifier struct {
	jwksURL, issuer, serviceToken string
	client                        *http.Client
	now                           func() time.Time

	mu          sync.Mutex
	keys        map[string]ed25519.PublicKey
	fetchedAt   time.Time
	lastAttempt time.Time
}

func NewVerifier(jwksURL, issuer, serviceToken string, client *http.Client) *Verifier {
	if client == nil {
		client = http.DefaultClient
	}
	return &Verifier{jwksURL: jwksURL, issuer: issuer, serviceToken: serviceToken, client: client, now: time.Now}
}

func (v *Verifier) Verify(ctx context.Context, raw string) (Principal, error) {
	var c claims
	_, err := jwt.ParseWithClaims(raw, &c, func(t *jwt.Token) (any, error) {
		kid, _ := t.Header["kid"].(string)
		if kid == "" {
			return nil, errors.New("missing kid")
		}
		return v.key(ctx, kid)
	},
		jwt.WithValidMethods([]string{"EdDSA"}),
		jwt.WithAudience(audience),
		jwt.WithIssuer(v.issuer),
		jwt.WithLeeway(leeway),
		jwt.WithExpirationRequired(),
		jwt.WithIssuedAt(),
		jwt.WithTimeFunc(v.now),
	)
	if err != nil {
		return Principal{}, fmt.Errorf("invalid token: %w", err)
	}
	user, err := uuid.Parse(c.Subject)
	if err != nil {
		return Principal{}, errors.New("invalid token: bad sub")
	}
	sid, err := uuid.Parse(c.Session)
	if err != nil {
		return Principal{}, errors.New("invalid token: bad sid")
	}
	return Principal{User: user, Session: sid, PlatformAdmin: c.PlatformAdmin}, nil
}

func (v *Verifier) key(ctx context.Context, kid string) (ed25519.PublicKey, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	now := v.now()
	key, known := v.keys[kid]
	stale := now.Sub(v.fetchedAt) >= maxKeyAge
	if (!known || stale) && now.Sub(v.lastAttempt) >= minRefreshGap {
		v.lastAttempt = now
		// A failed refresh keeps the cached keys so a hub outage does not log everyone out.
		if keys, err := v.fetch(ctx); err == nil {
			v.keys, v.fetchedAt = keys, now
			key, known = keys[kid]
		}
	}
	if !known {
		return nil, errors.New("unknown kid")
	}
	return key, nil
}

func (v *Verifier) fetch(ctx context.Context) (map[string]ed25519.PublicKey, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, v.jwksURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+v.serviceToken)
	resp, err := v.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("jwks: status %d", resp.StatusCode)
	}
	var set struct {
		Keys []struct {
			Kty, Crv, Kid, X string
		} `json:"keys"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxJWKSBodySize)).Decode(&set); err != nil {
		return nil, err
	}
	keys := map[string]ed25519.PublicKey{}
	for _, k := range set.Keys {
		if k.Kty != "OKP" || k.Crv != "Ed25519" || k.Kid == "" {
			continue
		}
		x, err := base64.RawURLEncoding.DecodeString(k.X)
		if err != nil || len(x) != ed25519.PublicKeySize {
			continue
		}
		keys[k.Kid] = ed25519.PublicKey(x)
	}
	return keys, nil
}

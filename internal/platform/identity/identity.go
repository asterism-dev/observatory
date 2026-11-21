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
	"sync/atomic"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
	"golang.org/x/sync/singleflight"
)

const (
	audience        = "observatory"
	leeway          = 30 * time.Second
	minRefreshGap   = 30 * time.Second
	maxKeyAge       = 10 * time.Minute
	maxJWKSBodySize = 1 << 20
	fetchTimeout    = 5 * time.Second
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

type keySet struct {
	keys      map[string]ed25519.PublicKey
	fetchedAt time.Time
}

// Verifier validates identity JWTs against the hub's JWKS; HTTP never runs under a lock the verify path takes.
type Verifier struct {
	jwksURL, issuer, serviceToken string
	client                        *http.Client
	now                           func() time.Time
	fetchTimeout                  time.Duration

	set    atomic.Pointer[keySet]
	flight singleflight.Group

	mu          sync.Mutex
	lastAttempt time.Time
}

func NewVerifier(jwksURL, issuer, serviceToken string, client *http.Client) (*Verifier, error) {
	if jwksURL == "" || issuer == "" || serviceToken == "" {
		return nil, errors.New("identity: jwks url, issuer and service token are required")
	}
	if client == nil {
		client = &http.Client{Timeout: fetchTimeout}
	}
	return &Verifier{jwksURL: jwksURL, issuer: issuer, serviceToken: serviceToken, client: client, now: time.Now, fetchTimeout: fetchTimeout}, nil
}

func canonicalUUID(s string) (uuid.UUID, error) {
	id, err := uuid.Parse(s)
	if err == nil && id.String() != s {
		err = errors.New("not canonical")
	}
	return id, err
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
	user, err := canonicalUUID(c.Subject)
	if err != nil {
		return Principal{}, errors.New("invalid token: bad sub")
	}
	sid, err := canonicalUUID(c.Session)
	if err != nil {
		return Principal{}, errors.New("invalid token: bad sid")
	}
	return Principal{User: user, Session: sid, PlatformAdmin: c.PlatformAdmin}, nil
}

func (v *Verifier) key(ctx context.Context, kid string) (ed25519.PublicKey, error) {
	cur := v.set.Load()
	if cur != nil {
		if key, ok := cur.keys[kid]; ok {
			if v.now().Sub(cur.fetchedAt) >= maxKeyAge {
				v.refresh(ctx)
			}
			return key, nil
		}
	}
	if ch := v.refresh(ctx); ch != nil {
		select {
		case <-ch:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	if cur = v.set.Load(); cur != nil {
		if key, ok := cur.keys[kid]; ok {
			return key, nil
		}
	}
	return nil, errors.New("unknown kid")
}

// refresh starts a shared fetch unless one ran within the last 30 s; the channel is nil when it was skipped.
func (v *Verifier) refresh(ctx context.Context) <-chan singleflight.Result {
	v.mu.Lock()
	now := v.now()
	if now.Sub(v.lastAttempt) < minRefreshGap {
		v.mu.Unlock()
		return nil
	}
	v.lastAttempt = now
	v.mu.Unlock()
	return v.flight.DoChan("jwks", func() (any, error) {
		// Detached so one caller giving up does not fail the fetch the others share.
		fctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), v.fetchTimeout)
		defer cancel()
		keys, err := v.fetch(fctx)
		if err != nil {
			return nil, err
		}
		v.set.Store(&keySet{keys: keys, fetchedAt: v.now()})
		return nil, nil
	})
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
			Kty string `json:"kty"`
			Crv string `json:"crv"`
			Kid string `json:"kid"`
			X   string `json:"x"`
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
	if len(keys) == 0 {
		return nil, errors.New("jwks: no usable keys")
	}
	return keys, nil
}

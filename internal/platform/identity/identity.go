package identity

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/asterism-dev/observatory/internal/platform/uuidx"
	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
)

const (
	audience        = "observatory"
	leeway          = 30 * time.Second
	minRefreshGap   = 30 * time.Second
	maxKeyAge       = 10 * time.Minute
	maxJWKSBodySize = 1 << 20
	fetchTimeout    = 5 * time.Second
)

// ErrNoKey means no verification key matched: the token's kid is unknown or the key set could not be fetched.
var ErrNoKey = errors.New("no verification key")

type Principal struct {
	User           uuid.UUID
	Session        uuid.UUID
	PlatformAdmin  bool
	Name           string
	SessionKind    string
	SessionExpires time.Time
}

type principalKey struct{}

func WithPrincipal(ctx context.Context, p Principal) context.Context {
	return context.WithValue(ctx, principalKey{}, p)
}

func FromContext(ctx context.Context) (Principal, bool) {
	p, ok := ctx.Value(principalKey{}).(Principal)
	return p, ok
}

type clientIPKey struct{}

// WithClientIP records the edge's client IP header of a call whose JWT verified.
func WithClientIP(ctx context.Context, ip string) context.Context {
	return context.WithValue(ctx, clientIPKey{}, ip)
}

func ClientIP(ctx context.Context) string {
	ip, _ := ctx.Value(clientIPKey{}).(string)
	return ip
}

type Caller struct {
	Principal Principal
	ClientIP  string
	Verified  bool
	Denied    bool
	Details   map[string]any
}

type callerKey struct{}

// WithCallerSlot lets an outer interceptor learn whom authz verified, also when authz then refuses the call.
func WithCallerSlot(ctx context.Context) (context.Context, *Caller) {
	c := &Caller{}
	return context.WithValue(ctx, callerKey{}, c), c
}

// WithVerified stores p and the edge's client IP for handlers and fills an outer caller slot, if any.
func WithVerified(ctx context.Context, p Principal, ip string) context.Context {
	if c, ok := ctx.Value(callerKey{}).(*Caller); ok {
		*c = Caller{Principal: p, ClientIP: ip, Verified: true}
	}
	return WithClientIP(WithPrincipal(ctx, p), ip)
}

// MarkDenied tells an outer caller slot that authorization, not the handler, refused the call.
func MarkDenied(ctx context.Context) {
	if c, ok := ctx.Value(callerKey{}).(*Caller); ok {
		c.Denied = true
	}
}

// SetDetails gives an outer caller slot the handler's audit details.
func SetDetails(ctx context.Context, d map[string]any) {
	if c, ok := ctx.Value(callerKey{}).(*Caller); ok {
		c.Details = d
	}
}

type claims struct {
	jwt.RegisteredClaims
	Session       string `json:"sid"`
	PlatformAdmin bool   `json:"pa"`
	Name          string `json:"name"`
	SessionKind   string `json:"sk"`
	SessionExp    int64  `json:"sx"`
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
	// local verifiers hold their signer's key and never fetch.
	local bool

	set atomic.Pointer[keySet]

	mu          sync.Mutex
	lastAttempt time.Time
	inflight    chan struct{}
	lastErr     string
}

// NewVerifier fetches keys from hubURL's /internal/iam/jwks; issuer is the public URL, compared without a trailing slash as the hub issues it.
func NewVerifier(hubURL, issuer, serviceToken string, client *http.Client) (*Verifier, error) {
	issuer = strings.TrimRight(issuer, "/")
	if hubURL == "" || issuer == "" || serviceToken == "" {
		return nil, errors.New("identity: hub url, issuer and service token are required")
	}
	jwksURL := strings.TrimRight(hubURL, "/") + "/internal/iam/jwks"
	if client == nil {
		client = &http.Client{Timeout: fetchTimeout}
	}
	return &Verifier{jwksURL: jwksURL, issuer: issuer, serviceToken: serviceToken, client: client, now: time.Now, fetchTimeout: fetchTimeout}, nil
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
	user, err := uuidx.Canonical(c.Subject)
	if err != nil {
		return Principal{}, errors.New("invalid token: bad sub")
	}
	sid, err := uuidx.Canonical(c.Session)
	if err != nil {
		return Principal{}, errors.New("invalid token: bad sid")
	}
	p := Principal{User: user, Session: sid, PlatformAdmin: c.PlatformAdmin, Name: c.Name, SessionKind: c.SessionKind}
	if c.SessionExp > 0 {
		p.SessionExpires = time.Unix(c.SessionExp, 0)
	}
	return p, nil
}

func (v *Verifier) key(ctx context.Context, kid string) (ed25519.PublicKey, error) {
	cur := v.set.Load()
	if v.local {
		if key, ok := cur.keys[kid]; ok {
			return key, nil
		}
		return nil, ErrNoKey
	}
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
	return nil, ErrNoKey
}

// refresh joins the fetch in flight, or starts one unless the last attempt was under 30 s ago (nil channel).
func (v *Verifier) refresh(ctx context.Context) <-chan struct{} {
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.inflight != nil {
		return v.inflight
	}
	now := v.now()
	if now.Sub(v.lastAttempt) < minRefreshGap {
		return nil
	}
	v.lastAttempt = now
	done := make(chan struct{})
	v.inflight = done
	// Detached so one caller giving up does not fail the fetch the others share.
	base := context.WithoutCancel(ctx)
	go func() {
		fctx, cancel := context.WithTimeout(base, v.fetchTimeout)
		defer cancel()
		keys, err := v.fetch(fctx)
		msg := ""
		if err == nil {
			v.set.Store(&keySet{keys: keys, fetchedAt: v.now()})
		} else {
			msg = err.Error()
		}
		v.mu.Lock()
		v.inflight = nil
		// Logged on change only: refreshes run at most every 30 s, and a down hub would otherwise repeat itself.
		changed := msg != v.lastErr
		v.lastErr = msg
		v.mu.Unlock()
		switch {
		case changed && err != nil:
			slog.Warn("identity: jwks fetch failed", "url", v.jwksURL, "err", err)
		case changed:
			slog.Info("identity: jwks fetch recovered", "url", v.jwksURL)
		}
		close(done)
	}()
	return done
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

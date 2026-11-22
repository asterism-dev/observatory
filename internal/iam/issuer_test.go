package iam

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"

	"github.com/asterism-dev/observatory/internal/iam/store"
	"github.com/asterism-dev/observatory/internal/iam/store/storetest"
	"github.com/asterism-dev/observatory/internal/platform/identity"
	"github.com/asterism-dev/observatory/internal/platform/servicetoken"
	"github.com/asterism-dev/observatory/internal/seal"
)

const (
	tokenEdgeSecret = "0123456789abcdef0123456789abcdef"
	tokenIssuer     = "https://hub.test"
)

type tokenEnv struct {
	s *store.Store
	h http.Handler
}

func newTokenEnv(t *testing.T) *tokenEnv {
	t.Helper()
	s := store.New(storetest.NewDB(t))
	iss, err := NewIssuer(context.Background(), s, seal.Key{1}, tokenIssuer)
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	mux.Handle("/token", iss.TokenHandler())
	mux.Handle("/jwks", iss.JWKSHandler())
	return &tokenEnv{s: s, h: servicetoken.Middleware(map[string]string{"edge": tokenEdgeSecret})(mux)}
}

func (e *tokenEnv) do(method, path, auth string, body any) *httptest.ResponseRecorder {
	var buf bytes.Buffer
	if body != nil {
		_ = json.NewEncoder(&buf).Encode(body)
	}
	r := httptest.NewRequest(method, path, &buf)
	if auth != "" {
		r.Header.Set("Authorization", auth)
	}
	rec := httptest.NewRecorder()
	e.h.ServeHTTP(rec, r)
	return rec
}

func (e *tokenEnv) login(t *testing.T, subject string) (store.User, string, store.Session) {
	t.Helper()
	ctx := context.Background()
	res, err := e.s.Login(ctx, store.Identity{Subject: subject, Email: subject + "@x.io", Name: subject})
	if err != nil {
		t.Fatal(err)
	}
	tok, sess, err := e.s.CreateSession(ctx, res.User.ID, store.SessionBrowser, time.Hour, "test", "127.0.0.1")
	if err != nil {
		t.Fatal(err)
	}
	return res.User, tok, sess
}

func (e *tokenEnv) jwks(t *testing.T) map[string]ed25519.PublicKey {
	t.Helper()
	rec := e.do("GET", "/jwks", "Bearer "+tokenEdgeSecret, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("jwks: %d %s", rec.Code, rec.Body)
	}
	var set struct {
		Keys []struct{ Kty, Crv, Alg, Use, Kid, X string }
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &set); err != nil {
		t.Fatal(err)
	}
	keys := map[string]ed25519.PublicKey{}
	for _, k := range set.Keys {
		if k.Kty != "OKP" || k.Crv != "Ed25519" || k.Alg != "EdDSA" || k.Use != "sig" || k.Kid == "" {
			t.Fatalf("bad jwk %+v", k)
		}
		x, err := base64.RawURLEncoding.DecodeString(k.X)
		if err != nil || len(x) != ed25519.PublicKeySize {
			t.Fatalf("bad x %q: %v", k.X, err)
		}
		keys[k.Kid] = x
	}
	return keys
}

func TestJWKSHasCurrentKey(t *testing.T) {
	e := newTokenEnv(t)
	keys := e.jwks(t)
	if len(keys) != 1 {
		t.Fatalf("keys: %v", keys)
	}
	// The key is sealed in iam.secrets, so a second issuer (restart, another instance) publishes the same one.
	iss, err := NewIssuer(context.Background(), e.s, seal.Key{1}, tokenIssuer)
	if err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	iss.JWKSHandler().ServeHTTP(rec, httptest.NewRequest("GET", "/", nil))
	if rec.Body.String() != e.do("GET", "/jwks", "Bearer "+tokenEdgeSecret, nil).Body.String() {
		t.Fatalf("restart changed the key: %v vs %s", keys, rec.Body)
	}
}

func TestTokenEndpointIssuesJWT(t *testing.T) {
	e := newTokenEnv(t)
	user, tok, sess := e.login(t, "alice")
	rec := e.do("POST", "/token", "Bearer "+tokenEdgeSecret, map[string]string{"session_token": tok})
	if rec.Code != http.StatusOK {
		t.Fatalf("token: %d %s", rec.Code, rec.Body)
	}
	var out struct {
		Token     string    `json:"token"`
		ExpiresAt time.Time `json:"expires_at"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	keys := e.jwks(t)
	var c struct {
		jwt.RegisteredClaims
		SID string `json:"sid"`
		PA  bool   `json:"pa"`
	}
	parsed, err := jwt.ParseWithClaims(out.Token, &c, func(tk *jwt.Token) (any, error) {
		k, ok := keys[tk.Header["kid"].(string)]
		if !ok {
			return nil, errors.New("unknown kid")
		}
		return k, nil
	}, jwt.WithValidMethods([]string{"EdDSA"}), jwt.WithAudience("observatory"), jwt.WithIssuer(tokenIssuer), jwt.WithIssuedAt())
	if err != nil || !parsed.Valid {
		t.Fatalf("verify: %v", err)
	}
	if c.Subject != user.ID.String() || c.SID != sess.ID.String() || !c.PA {
		t.Fatalf("claims: sub=%s sid=%s pa=%v", c.Subject, c.SID, c.PA)
	}
	if !slices.Equal(c.Audience, jwt.ClaimStrings{"observatory"}) {
		t.Fatalf("aud: %v", c.Audience)
	}
	if !c.NotBefore.Equal(c.IssuedAt.Time) || c.ExpiresAt.Sub(c.IssuedAt.Time) != 5*time.Minute || !out.ExpiresAt.Equal(c.ExpiresAt.Time) {
		t.Fatalf("times: iat=%v nbf=%v exp=%v expires_at=%v", c.IssuedAt, c.NotBefore, c.ExpiresAt, out.ExpiresAt)
	}

	// The second user is not a platform admin.
	_, tok2, _ := e.login(t, "bob")
	rec = e.do("POST", "/token", "Bearer "+tokenEdgeSecret, map[string]string{"session_token": tok2})
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	c.PA = true
	if _, err := jwt.ParseWithClaims(out.Token, &c, func(tk *jwt.Token) (any, error) { return keys[tk.Header["kid"].(string)], nil }); err != nil || c.PA {
		t.Fatalf("bob: pa=%v err=%v", c.PA, err)
	}
}

func TestTokenCarriesNameKindAndSessionExpiry(t *testing.T) {
	e := newTokenEnv(t)
	res, err := e.s.Login(context.Background(), store.Identity{Subject: "alice", Email: "alice@x.io", Name: "Alice\x07 Doe"})
	if err != nil {
		t.Fatal(err)
	}
	tok, sess, err := e.s.CreateSession(context.Background(), res.User.ID, store.SessionCLI, time.Hour, "test", "127.0.0.1")
	if err != nil {
		t.Fatal(err)
	}
	rec := e.do("POST", "/token", "Bearer "+tokenEdgeSecret, map[string]string{"session_token": tok})
	var out struct {
		Token string `json:"token"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("%d %s: %v", rec.Code, rec.Body, err)
	}
	var c struct {
		jwt.RegisteredClaims
		Name string `json:"name"`
		SK   string `json:"sk"`
		SX   int64  `json:"sx"`
	}
	keys := e.jwks(t)
	if _, err := jwt.ParseWithClaims(out.Token, &c, func(tk *jwt.Token) (any, error) { return keys[tk.Header["kid"].(string)], nil }); err != nil {
		t.Fatal(err)
	}
	if c.Name != "Alice Doe" || c.SK != "cli" || c.SX != sess.ExpiresAt.Unix() {
		t.Fatalf("name %q, sk %q, sx %d (session expires %d)", c.Name, c.SK, c.SX, sess.ExpiresAt.Unix())
	}
}

func TestTokenExpiryBoundBySession(t *testing.T) {
	e := newTokenEnv(t)
	res, err := e.s.Login(context.Background(), store.Identity{Subject: "alice", Email: "alice@x.io", Name: "alice"})
	if err != nil {
		t.Fatal(err)
	}
	tok, sess, err := e.s.CreateSession(context.Background(), res.User.ID, store.SessionCLI, time.Minute, "test", "127.0.0.1")
	if err != nil {
		t.Fatal(err)
	}
	rec := e.do("POST", "/token", "Bearer "+tokenEdgeSecret, map[string]string{"session_token": tok})
	var out struct {
		Token     string    `json:"token"`
		ExpiresAt time.Time `json:"expires_at"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("%d %s: %v", rec.Code, rec.Body, err)
	}
	var c jwt.RegisteredClaims
	keys := e.jwks(t)
	if _, err := jwt.ParseWithClaims(out.Token, &c, func(tk *jwt.Token) (any, error) { return keys[tk.Header["kid"].(string)], nil }); err != nil {
		t.Fatal(err)
	}
	if c.ExpiresAt.After(sess.ExpiresAt) || sess.ExpiresAt.Sub(c.ExpiresAt.Time) >= time.Second || !out.ExpiresAt.Equal(c.ExpiresAt.Time) {
		t.Fatalf("exp %v, expires_at %v, session expires %v", c.ExpiresAt, out.ExpiresAt, sess.ExpiresAt)
	}
}

func TestTokenEndpointRejectsRevokedSession(t *testing.T) {
	e := newTokenEnv(t)
	user, tok, sess := e.login(t, "alice")
	if err := e.s.RevokeSession(context.Background(), user.ID, sess.ID); err != nil {
		t.Fatal(err)
	}
	for name, body := range map[string]any{
		"revoked": map[string]string{"session_token": tok},
		"unknown": map[string]string{"session_token": "obs_nope"},
		"empty":   map[string]string{},
	} {
		if rec := e.do("POST", "/token", "Bearer "+tokenEdgeSecret, body); rec.Code != http.StatusUnauthorized {
			t.Errorf("%s: %d %s", name, rec.Code, rec.Body)
		}
	}
}

func TestSigningKeySealedInIAM(t *testing.T) {
	ctx := context.Background()
	pool := storetest.NewDB(t)
	s := store.New(pool)
	iss, err := NewIssuer(ctx, s, seal.Key{1}, tokenIssuer)
	if err != nil {
		t.Fatal(err)
	}
	var sealed []byte
	if err := pool.QueryRow(ctx, "SELECT sealed FROM secrets WHERE name = $1", signingKeySecret).Scan(&sealed); err != nil {
		t.Fatal(err)
	}
	seed, err := s.Secret(ctx, seal.Key{1}, signingKeySecret, nil)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(sealed, seed) {
		t.Fatal("signing key stored in the clear")
	}
	stored, err := identity.NewSigner(ed25519.NewKeyFromSeed(seed), tokenIssuer)
	if err != nil {
		t.Fatal(err)
	}
	tok, _, err := iss.signer.Mint(identity.Principal{User: uuid.New(), Session: uuid.New(), SessionKind: "cli", SessionExpires: time.Now().Add(time.Hour)})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := stored.Verifier().Verify(ctx, tok); err != nil {
		t.Fatalf("issuer does not sign with the sealed key: %v", err)
	}
	if _, err := NewIssuer(ctx, s, seal.Key{2}, tokenIssuer); err == nil {
		t.Fatal("signing key opened with another OBSERVATORY_SECRETS_KEY")
	}
}

func TestNewRefusesZeroSecretsKey(t *testing.T) {
	_, err := New(context.Background(), Config{PublicURL: "https://hub.test", HubURL: "http://hub", ServiceToken: "t"})
	if err == nil || !strings.Contains(err.Error(), "secrets key") {
		t.Fatalf("zero secrets key: %v", err)
	}
}

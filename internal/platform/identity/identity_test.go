package identity

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
)

const (
	testIssuer = "https://obs.example"
	testToken  = "service-token-service-token-service-token"
)

type fakeJWKS struct {
	mu    sync.Mutex
	keys  map[string]ed25519.PublicKey
	down  bool
	hits  atomic.Int32
	srv   *httptest.Server
	badAu atomic.Bool
	block chan struct{}
}

func newFakeJWKS(t *testing.T) *fakeJWKS {
	f := &fakeJWKS{keys: map[string]ed25519.PublicKey{}}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.hits.Add(1)
		if r.Header.Get("Authorization") != "Bearer "+testToken {
			f.badAu.Store(true)
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		f.mu.Lock()
		blk := f.block
		f.mu.Unlock()
		if blk != nil {
			select {
			case <-blk:
			case <-r.Context().Done():
				return
			}
		}
		f.mu.Lock()
		defer f.mu.Unlock()
		if f.down {
			http.Error(w, "down", http.StatusServiceUnavailable)
			return
		}
		var out []map[string]string
		for kid, pub := range f.keys {
			out = append(out, map[string]string{"kty": "OKP", "crv": "Ed25519", "alg": "EdDSA", "use": "sig", "kid": kid, "x": base64.RawURLEncoding.EncodeToString(pub)})
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"keys": out})
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeJWKS) addKey(kid string) ed25519.PrivateKey {
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	f.mu.Lock()
	f.keys[kid] = pub
	f.mu.Unlock()
	return priv
}

func (f *fakeJWKS) setDown(v bool) {
	f.mu.Lock()
	f.down = v
	f.mu.Unlock()
}

type fixture struct {
	jwks *fakeJWKS
	key  ed25519.PrivateKey
	v    *Verifier
	now  time.Time
	user uuid.UUID
	sess uuid.UUID
}

func newFixture(t *testing.T) *fixture {
	f := &fixture{jwks: newFakeJWKS(t), now: time.Now(), user: uuid.New(), sess: uuid.New()}
	f.key = f.jwks.addKey("k1")
	v, err := NewVerifier(f.jwks.srv.URL, testIssuer, testToken, f.jwks.srv.Client())
	if err != nil {
		t.Fatal(err)
	}
	f.v = v
	f.v.now = func() time.Time { return f.now }
	return f
}

func (f *fixture) claims() jwt.MapClaims {
	return jwt.MapClaims{
		"iss": testIssuer, "aud": []string{"observatory"}, "sub": f.user.String(), "sid": f.sess.String(), "pa": true,
		"iat": f.now.Unix(), "nbf": f.now.Unix(), "exp": f.now.Add(5 * time.Minute).Unix(),
	}
}

func sign(t *testing.T, method jwt.SigningMethod, kid string, claims jwt.MapClaims, key any) string {
	tok := jwt.NewWithClaims(method, claims)
	if kid != "" {
		tok.Header["kid"] = kid
	}
	s, err := tok.SignedString(key)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestVerifyValid(t *testing.T) {
	f := newFixture(t)
	p, err := f.v.Verify(context.Background(), sign(t, jwt.SigningMethodEdDSA, "k1", f.claims(), f.key))
	if err != nil {
		t.Fatal(err)
	}
	if p.User != f.user || p.Session != f.sess || !p.PlatformAdmin {
		t.Fatalf("principal = %+v", p)
	}
	if f.jwks.badAu.Load() {
		t.Fatal("jwks fetched without service token")
	}
}

func TestVerifyRejects(t *testing.T) {
	_, otherKey, _ := ed25519.GenerateKey(rand.Reader)
	cases := map[string]func(f *fixture) string{
		"wrong key": func(f *fixture) string { return sign(t, jwt.SigningMethodEdDSA, "k1", f.claims(), otherKey) },
		"alg none": func(f *fixture) string {
			return sign(t, jwt.SigningMethodNone, "k1", f.claims(), jwt.UnsafeAllowNoneSignatureType)
		},
		"hs256 with public key as secret": func(f *fixture) string {
			return sign(t, jwt.SigningMethodHS256, "k1", f.claims(), []byte(f.key.Public().(ed25519.PublicKey)))
		},
		"expired": func(f *fixture) string {
			c := f.claims()
			c["exp"] = f.now.Add(-time.Minute).Unix()
			return sign(t, jwt.SigningMethodEdDSA, "k1", c, f.key)
		},
		"nbf only in future": func(f *fixture) string {
			c := f.claims()
			c["nbf"] = f.now.Add(time.Minute).Unix()
			return sign(t, jwt.SigningMethodEdDSA, "k1", c, f.key)
		},
		"not yet valid": func(f *fixture) string {
			c := f.claims()
			c["nbf"] = f.now.Add(time.Minute).Unix()
			c["iat"] = f.now.Add(time.Minute).Unix()
			return sign(t, jwt.SigningMethodEdDSA, "k1", c, f.key)
		},
		"wrong audience": func(f *fixture) string {
			c := f.claims()
			c["aud"] = []string{"other"}
			return sign(t, jwt.SigningMethodEdDSA, "k1", c, f.key)
		},
		"wrong issuer": func(f *fixture) string {
			c := f.claims()
			c["iss"] = "https://evil.example"
			return sign(t, jwt.SigningMethodEdDSA, "k1", c, f.key)
		},
		"missing kid": func(f *fixture) string { return sign(t, jwt.SigningMethodEdDSA, "", f.claims(), f.key) },
		"missing exp": func(f *fixture) string {
			c := f.claims()
			delete(c, "exp")
			return sign(t, jwt.SigningMethodEdDSA, "k1", c, f.key)
		},
		"bad sub": func(f *fixture) string {
			c := f.claims()
			c["sub"] = "not-a-uuid"
			return sign(t, jwt.SigningMethodEdDSA, "k1", c, f.key)
		},
		"non-canonical sub": func(f *fixture) string {
			c := f.claims()
			c["sub"] = "urn:uuid:" + f.user.String()
			return sign(t, jwt.SigningMethodEdDSA, "k1", c, f.key)
		},
		"bad sid": func(f *fixture) string {
			c := f.claims()
			c["sid"] = ""
			return sign(t, jwt.SigningMethodEdDSA, "k1", c, f.key)
		},
		"garbage": func(f *fixture) string { return "not.a.jwt" },
	}
	for name, mk := range cases {
		t.Run(name, func(t *testing.T) {
			f := newFixture(t)
			if _, err := f.v.Verify(context.Background(), mk(f)); err == nil {
				t.Fatal("expected rejection")
			}
		})
	}
}

func TestVerifySkewAllowed(t *testing.T) {
	f := newFixture(t)
	c := f.claims()
	c["exp"] = f.now.Add(-20 * time.Second).Unix()
	if _, err := f.v.Verify(context.Background(), sign(t, jwt.SigningMethodEdDSA, "k1", c, f.key)); err != nil {
		t.Fatalf("20s past exp should be within skew: %v", err)
	}
}

func TestUnknownKidRefreshesOncePer30s(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	if _, err := f.v.Verify(ctx, sign(t, jwt.SigningMethodEdDSA, "k1", f.claims(), f.key)); err != nil {
		t.Fatal(err)
	}
	f.now = f.now.Add(31 * time.Second)
	base := f.jwks.hits.Load()
	_, otherKey, _ := ed25519.GenerateKey(rand.Reader)
	bad := sign(t, jwt.SigningMethodEdDSA, "ghost", f.claims(), otherKey)
	if _, err := f.v.Verify(ctx, bad); err == nil {
		t.Fatal("unknown kid accepted")
	}
	if got := f.jwks.hits.Load() - base; got != 1 {
		t.Fatalf("refreshes = %d, want 1", got)
	}
	if _, err := f.v.Verify(ctx, bad); err == nil {
		t.Fatal("unknown kid accepted")
	}
	if got := f.jwks.hits.Load() - base; got != 1 {
		t.Fatalf("refresh not rate limited: %d", got)
	}

	k2 := f.jwks.addKey("k2")
	f.now = f.now.Add(31 * time.Second)
	c := f.claims()
	if _, err := f.v.Verify(ctx, sign(t, jwt.SigningMethodEdDSA, "k2", c, k2)); err != nil {
		t.Fatalf("rotated key should be found after refresh: %v", err)
	}
}

func TestOutageKeepsCachedKeys(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	if _, err := f.v.Verify(ctx, sign(t, jwt.SigningMethodEdDSA, "k1", f.claims(), f.key)); err != nil {
		t.Fatal(err)
	}
	f.jwks.setDown(true)
	f.now = f.now.Add(11 * time.Minute)
	base := f.jwks.hits.Load()
	if _, err := f.v.Verify(ctx, sign(t, jwt.SigningMethodEdDSA, "k1", f.claims(), f.key)); err != nil {
		t.Fatalf("cached key must survive outage: %v", err)
	}
	for i := 0; f.jwks.hits.Load() == base; i++ {
		if i > 200 {
			t.Fatal("stale cache was not refreshed")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestFromContext(t *testing.T) {
	if _, ok := FromContext(context.Background()); ok {
		t.Fatal("empty context has principal")
	}
	want := Principal{User: uuid.New(), Session: uuid.New()}
	got, ok := FromContext(WithPrincipal(context.Background(), want))
	if !ok || got != want {
		t.Fatalf("got %+v %v", got, ok)
	}
}

func TestNewVerifierRequiresConfig(t *testing.T) {
	for _, a := range [][3]string{{"", "i", "t"}, {"u", "", "t"}, {"u", "i", ""}} {
		if _, err := NewVerifier(a[0], a[1], a[2], nil); err == nil {
			t.Fatalf("accepted %v", a)
		}
	}
}

func TestStaleKnownKidDoesNotWaitForBlockedJWKS(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	if _, err := f.v.Verify(ctx, sign(t, jwt.SigningMethodEdDSA, "k1", f.claims(), f.key)); err != nil {
		t.Fatal(err)
	}
	f.v.fetchTimeout = 200 * time.Millisecond
	blk := make(chan struct{})
	f.jwks.mu.Lock()
	f.jwks.block = blk
	f.jwks.mu.Unlock()
	t.Cleanup(func() { close(blk) })

	f.now = f.now.Add(11 * time.Minute)
	start := time.Now()
	if _, err := f.v.Verify(ctx, sign(t, jwt.SigningMethodEdDSA, "k1", f.claims(), f.key)); err != nil {
		t.Fatal(err)
	}
	if d := time.Since(start); d > 100*time.Millisecond {
		t.Fatalf("stale known kid waited %v", d)
	}

	f.now = f.now.Add(time.Minute)
	_, otherKey, _ := ed25519.GenerateKey(rand.Reader)
	start = time.Now()
	if _, err := f.v.Verify(ctx, sign(t, jwt.SigningMethodEdDSA, "ghost", f.claims(), otherKey)); err == nil {
		t.Fatal("unknown kid accepted")
	}
	if d := time.Since(start); d > 2*time.Second {
		t.Fatalf("unknown kid waited %v", d)
	}
}

func TestEmptyJWKSKeepsCache(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	if _, err := f.v.Verify(ctx, sign(t, jwt.SigningMethodEdDSA, "k1", f.claims(), f.key)); err != nil {
		t.Fatal(err)
	}
	f.jwks.mu.Lock()
	f.jwks.keys = map[string]ed25519.PublicKey{}
	f.jwks.mu.Unlock()
	f.now = f.now.Add(31 * time.Second)
	_, otherKey, _ := ed25519.GenerateKey(rand.Reader)
	if _, err := f.v.Verify(ctx, sign(t, jwt.SigningMethodEdDSA, "ghost", f.claims(), otherKey)); err == nil {
		t.Fatal("unknown kid accepted")
	}
	if _, err := f.v.Verify(ctx, sign(t, jwt.SigningMethodEdDSA, "k1", f.claims(), f.key)); err != nil {
		t.Fatalf("cache dropped after empty JWKS: %v", err)
	}
}

func TestConcurrentUnknownKidJoinsInFlightFetch(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	if _, err := f.v.Verify(ctx, sign(t, jwt.SigningMethodEdDSA, "k1", f.claims(), f.key)); err != nil {
		t.Fatal(err)
	}
	k2 := f.jwks.addKey("k2")
	f.now = f.now.Add(31 * time.Second)
	blk := make(chan struct{})
	f.jwks.mu.Lock()
	f.jwks.block = blk
	f.jwks.mu.Unlock()
	base := f.jwks.hits.Load()
	tok := sign(t, jwt.SigningMethodEdDSA, "k2", f.claims(), k2)

	const n = 10
	errs := make(chan error, n)
	for range n {
		go func() { _, err := f.v.Verify(ctx, tok); errs <- err }()
	}
	for f.jwks.hits.Load() == base {
		time.Sleep(5 * time.Millisecond)
	}
	time.Sleep(50 * time.Millisecond)
	close(blk)
	for range n {
		if err := <-errs; err != nil {
			t.Fatal(err)
		}
	}
	if got := f.jwks.hits.Load() - base; got != 1 {
		t.Fatalf("jwks requests = %d, want 1", got)
	}
}

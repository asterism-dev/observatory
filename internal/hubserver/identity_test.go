package hubserver

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
	"testing"
	"time"

	"connectrpc.com/connect"
	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"

	iamv1 "github.com/asterism-dev/observatory/gen/go/observatory/iam/v1"
	"github.com/asterism-dev/observatory/gen/go/observatory/iam/v1/iamv1connect"
	"github.com/asterism-dev/observatory/internal/authz"
	"github.com/asterism-dev/observatory/internal/db/dbtest"
	"github.com/asterism-dev/observatory/internal/platform/servicetoken"
	"github.com/asterism-dev/observatory/internal/seal"
	"github.com/asterism-dev/observatory/internal/store"
)

const (
	svcToken  = "0123456789abcdef0123456789abcdef"
	otherTok  = "fedcba9876543210fedcba9876543210"
	issuerURL = "https://hub.test"
)

type iamEnv struct {
	s *store.Store
	z *authz.Service
	h http.Handler
}

func newIAMEnv(t *testing.T) *iamEnv {
	t.Helper()
	ctx := context.Background()
	s := store.New(dbtest.New(t)).WithMasterKey(seal.Key{1})
	z, err := authz.NewService(ctx, s.PolicySnapshot)
	if err != nil {
		t.Fatal(err)
	}
	iss, err := NewIssuer(ctx, s, issuerURL)
	if err != nil {
		t.Fatal(err)
	}
	ps := NewPolicyServer()
	z.OnReload(ps.Publish)
	return &iamEnv{s: s, z: z, h: servicetoken.Middleware(map[string]string{"edge": svcToken, "compute": otherTok})(internalIAM(iss, ps))}
}

func (e *iamEnv) do(method, path, auth string, body any) *httptest.ResponseRecorder {
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

func (e *iamEnv) login(t *testing.T, subject string) (store.User, string, store.Session) {
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

func (e *iamEnv) jwks(t *testing.T) map[string]ed25519.PublicKey {
	t.Helper()
	rec := e.do("GET", "/internal/iam/jwks", "Bearer "+svcToken, nil)
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
	e := newIAMEnv(t)
	keys := e.jwks(t)
	if len(keys) != 1 {
		t.Fatalf("keys: %v", keys)
	}
	// The key is sealed in hub_secrets, so a second issuer (restart, another hub) publishes the same one.
	iss, err := NewIssuer(context.Background(), e.s, issuerURL)
	if err != nil {
		t.Fatal(err)
	}
	again := e.jwks(t)
	if _, ok := again[iss.kid]; !ok || len(again) != 1 {
		t.Fatalf("restart changed the key: %v vs %s", keys, iss.kid)
	}
}

func TestTokenEndpointIssuesJWT(t *testing.T) {
	e := newIAMEnv(t)
	user, tok, sess := e.login(t, "alice")
	rec := e.do("POST", "/internal/iam/token", "Bearer "+svcToken, map[string]string{"session_token": tok})
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
	}, jwt.WithValidMethods([]string{"EdDSA"}), jwt.WithAudience("observatory"), jwt.WithIssuer(issuerURL), jwt.WithIssuedAt())
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
	rec = e.do("POST", "/internal/iam/token", "Bearer "+svcToken, map[string]string{"session_token": tok2})
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	c.PA = true
	if _, err := jwt.ParseWithClaims(out.Token, &c, func(tk *jwt.Token) (any, error) { return keys[tk.Header["kid"].(string)], nil }); err != nil || c.PA {
		t.Fatalf("bob: pa=%v err=%v", c.PA, err)
	}
}

func TestTokenExpiryBoundBySession(t *testing.T) {
	e := newIAMEnv(t)
	res, err := e.s.Login(context.Background(), store.Identity{Subject: "alice", Email: "alice@x.io", Name: "alice"})
	if err != nil {
		t.Fatal(err)
	}
	tok, sess, err := e.s.CreateSession(context.Background(), res.User.ID, store.SessionCLI, time.Minute, "test", "127.0.0.1")
	if err != nil {
		t.Fatal(err)
	}
	rec := e.do("POST", "/internal/iam/token", "Bearer "+svcToken, map[string]string{"session_token": tok})
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

func TestTokenEndpointOnlyForEdge(t *testing.T) {
	e := newIAMEnv(t)
	_, tok, _ := e.login(t, "alice")
	if rec := e.do("POST", "/internal/iam/token", "Bearer "+otherTok, map[string]string{"session_token": tok}); rec.Code != http.StatusForbidden {
		t.Fatalf("compute minting user tokens: %d", rec.Code)
	}
	if rec := e.do("GET", "/internal/iam/jwks", "Bearer "+otherTok, nil); rec.Code != http.StatusOK {
		t.Fatalf("compute reading jwks: %d", rec.Code)
	}
	srv := httptest.NewServer(e.h)
	defer srv.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req := connect.NewRequest(&iamv1.WatchPolicyRequest{})
	req.Header().Set("Authorization", "Bearer "+otherTok)
	stream, err := iamv1connect.NewPolicyServiceClient(srv.Client(), srv.URL+"/internal/iam").WatchPolicy(ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = stream.Close() }()
	if !stream.Receive() {
		t.Fatalf("compute watching policy: %v", stream.Err())
	}
}

func TestTokenEndpointRejectsRevokedSession(t *testing.T) {
	e := newIAMEnv(t)
	user, tok, sess := e.login(t, "alice")
	if err := e.s.RevokeSession(context.Background(), user.ID, sess.ID); err != nil {
		t.Fatal(err)
	}
	for name, body := range map[string]any{
		"revoked": map[string]string{"session_token": tok},
		"unknown": map[string]string{"session_token": "obs_nope"},
		"empty":   map[string]string{},
	} {
		if rec := e.do("POST", "/internal/iam/token", "Bearer "+svcToken, body); rec.Code != http.StatusUnauthorized {
			t.Errorf("%s: %d %s", name, rec.Code, rec.Body)
		}
	}
}

func TestInternalRequiresServiceToken(t *testing.T) {
	e := newIAMEnv(t)
	_, tok, _ := e.login(t, "alice")
	paths := []struct{ method, path string }{
		{"POST", "/internal/iam/token"},
		{"GET", "/internal/iam/jwks"},
		{"POST", "/internal/iam" + iamv1connect.PolicyServiceWatchPolicyProcedure},
	}
	for _, p := range paths {
		// The session token itself is no service credential.
		for _, auth := range []string{"", "Bearer wrong-wrong-wrong-wrong-wrong-wrong", "Bearer " + tok} {
			if rec := e.do(p.method, p.path, auth, map[string]string{"session_token": tok}); rec.Code != http.StatusForbidden {
				t.Errorf("%s %s with %q: %d", p.method, p.path, auth, rec.Code)
			}
		}
	}

	// Without OBSERVATORY_SERVICE_TOKENS every internal call is refused.
	iss, err := NewIssuer(context.Background(), e.s, issuerURL)
	if err != nil {
		t.Fatal(err)
	}
	closed := servicetoken.Middleware(nil)(internalIAM(iss, NewPolicyServer()))
	for _, p := range paths {
		r := httptest.NewRequest(p.method, p.path, nil)
		r.Header.Set("Authorization", "Bearer ")
		rec := httptest.NewRecorder()
		closed.ServeHTTP(rec, r)
		if rec.Code != http.StatusForbidden {
			t.Errorf("no tokens configured, %s: %d", p.path, rec.Code)
		}
	}
}

func TestWatchPolicySendsSnapshotAndUpdates(t *testing.T) {
	e := newIAMEnv(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	owner, _, _ := e.login(t, "alice")
	bob, _, _ := e.login(t, "bob")
	if err := e.z.Reload(ctx); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(e.h)
	defer srv.Close()
	client := iamv1connect.NewPolicyServiceClient(srv.Client(), srv.URL+"/internal/iam")
	req := connect.NewRequest(&iamv1.WatchPolicyRequest{})
	req.Header().Set("Authorization", "Bearer "+svcToken)
	stream, err := client.WatchPolicy(ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = stream.Close() }()
	if !stream.Receive() {
		t.Fatalf("snapshot: %v", stream.Err())
	}
	first := stream.Msg()
	org, err := e.s.CreateOrg(ctx, "acme", owner.ID)
	if err != nil {
		t.Fatal(err)
	}
	if hasMember(first, org.ID, bob.ID) {
		t.Fatal("bob is a member before the invite")
	}

	invite, _, err := e.s.CreateInvite(ctx, org.ID, store.RoleMember, owner.ID, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := e.s.AcceptInvite(ctx, invite, bob.ID); err != nil {
		t.Fatal(err)
	}
	if err := e.z.Reload(ctx); err != nil {
		t.Fatal(err)
	}
	if !stream.Receive() {
		t.Fatalf("update: %v", stream.Err())
	}
	second := stream.Msg()
	if second.Revision <= first.Revision {
		t.Fatalf("revision %d after %d", second.Revision, first.Revision)
	}
	if !hasMember(second, org.ID, bob.ID) {
		t.Fatalf("bob missing from %v", second.Memberships)
	}
}

func hasMember(p *iamv1.Policy, org, user uuid.UUID) bool {
	return slices.ContainsFunc(p.Memberships, func(m *iamv1.MemberRole) bool {
		return m.OrgId == org.String() && m.UserId == user.String() && m.Role == "member"
	})
}

func TestPolicyPublishDoesNotBlock(t *testing.T) {
	ps := NewPolicyServer()
	// A subscriber that never drains must not stall Publish (which runs under authz's reload lock).
	ch := ps.subscribe()
	defer ps.unsubscribe(ch)
	done := make(chan struct{})
	go func() {
		for range 1000 {
			ps.Publish(authz.Policy{Teams: []authz.TeamRef{{OrgID: uuid.New(), ID: uuid.New()}}})
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Publish blocked on a slow subscriber")
	}
	if got := ps.latest(); got == nil || got.Revision == 0 {
		t.Fatalf("latest: %v", got)
	}
}

func TestPolicyPublishSkipsUnchanged(t *testing.T) {
	ps := NewPolicyServer()
	ps.Publish(authz.Policy{})
	rev := ps.latest().Revision
	ps.Publish(authz.Policy{})
	if got := ps.latest().Revision; got != rev {
		t.Fatalf("unchanged snapshot bumped revision %d → %d", rev, got)
	}
	ps.Publish(authz.Policy{Teams: []authz.TeamRef{{OrgID: uuid.New(), ID: uuid.New()}}})
	if got := ps.latest().Revision; got <= rev {
		t.Fatalf("changed snapshot kept revision %d", got)
	}
}

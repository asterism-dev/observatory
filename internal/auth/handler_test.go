package auth

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/asterism-dev/observatory/internal/db/dbtest"
	"github.com/asterism-dev/observatory/internal/platform/clientip"
	"github.com/asterism-dev/observatory/internal/platform/identity"
	"github.com/asterism-dev/observatory/internal/seal"
	"github.com/asterism-dev/observatory/internal/store"
	"github.com/asterism-dev/observatory/internal/store/storetest"
)

func TestIdentityFrom(t *testing.T) {
	noCall := func() (idClaims, error) { t.Fatal("userinfo called"); return idClaims{}, nil }
	id, err := identityFrom(idClaims{Subject: "s", Email: "e@x.io", Name: "N"}, noCall)
	if err != nil || id != (store.Identity{Subject: "s", Email: "e@x.io", Name: "N"}) {
		t.Fatalf("complete claims: %+v, %v", id, err)
	}

	id, err = identityFrom(idClaims{Subject: "s", Email: "e@x.io"}, func() (idClaims, error) {
		return idClaims{Subject: "s", Name: "From UI"}, nil
	})
	if err != nil || id.Name != "From UI" || id.Email != "e@x.io" {
		t.Fatalf("name from userinfo: %+v, %v", id, err)
	}

	if _, err = identityFrom(idClaims{Subject: "s"}, func() (idClaims, error) {
		return idClaims{Subject: "other", Email: "e@x.io", Name: "N"}, nil
	}); err == nil {
		t.Fatal("userinfo sub mismatch accepted")
	}

	if _, err = identityFrom(idClaims{Subject: "s", Name: "N"}, func() (idClaims, error) {
		return idClaims{Subject: "s"}, nil
	}); err == nil {
		t.Fatal("missing email accepted")
	}

	if _, err = identityFrom(idClaims{Subject: "s"}, func() (idClaims, error) {
		return idClaims{}, errors.New("boom")
	}); err == nil {
		t.Fatal("userinfo error swallowed")
	}
}

type fakeAuth struct {
	id       store.Identity
	cliToken string
}

func (f fakeAuth) AuthCodeURL(state, verifier string, register bool) string {
	return "https://idp.test/auth?state=" + url.QueryEscape(state)
}

func (f fakeAuth) Exchange(ctx context.Context, code, verifier string) (store.Identity, error) {
	if code == "" || verifier == "" {
		return store.Identity{}, errors.New("bad exchange")
	}
	return f.id, nil
}

func (f fakeAuth) VerifyCLIToken(ctx context.Context, rawIDToken, accessToken string) (store.Identity, error) {
	if f.cliToken == "" || rawIDToken != f.cliToken {
		return store.Identity{}, errors.New("secret-detail rejected")
	}
	return f.id, nil
}

type env struct {
	t     *testing.T
	pool  *pgxpool.Pool
	store *store.Store
	a     *Handler
	iss   *Issuer
	h     http.Handler
	// header is added to every request, e.g. to arrive through the edge.
	header http.Header
}

const edgeToken = "0123456789abcdef0123456789abcdef"

func newEnv(t *testing.T) *env {
	t.Helper()
	pool := dbtest.New(t)
	s := store.New(pool).WithMasterKey(seal.Key{1})
	iss, err := NewIssuer(context.Background(), s, "https://hub.test")
	if err != nil {
		t.Fatal(err)
	}
	jwks := httptest.NewServer(iss.JWKSHandler())
	t.Cleanup(jwks.Close)
	v, err := identity.NewVerifier(jwks.URL, "https://hub.test/", "unused-service-token", nil)
	if err != nil {
		t.Fatal(err)
	}
	h := &Handler{
		Verifier:    v,
		Store:       s,
		Auth:        fakeAuth{id: store.Identity{Subject: "sub-1", Email: "a@x.io", Name: "Alice"}, cliToken: "good"},
		Issuer:      "https://idp.test",
		CLIClientID: "cli-client",
		PublicURL:   &url.URL{Scheme: "https", Host: "hub.test"},
		EdgeToken:   edgeToken,
	}
	mux := http.NewServeMux()
	h.Routes(mux)
	mux.Handle("/whoami", h.Require(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p, ok := PrincipalFrom(r.Context())
		if !ok {
			t.Error("no principal")
		}
		_, _ = w.Write([]byte(p.User.Name))
	})))
	return &env{t: t, pool: pool, store: s, a: h, iss: iss, h: h.TrustEdge(mux)}
}

func (e *env) do(r *http.Request) *httptest.ResponseRecorder {
	for k, v := range e.header {
		r.Header[k] = v
	}
	rec := httptest.NewRecorder()
	e.h.ServeHTTP(rec, r)
	return rec
}

func cookieNamed(rec *httptest.ResponseRecorder, name string) *http.Cookie {
	for _, c := range rec.Result().Cookies() {
		if c.Name == name {
			return c
		}
	}
	return nil
}

// login runs /auth/login then /auth/callback and returns the callback response.
func (e *env) login(redirect string) *httptest.ResponseRecorder {
	e.t.Helper()
	rec := e.do(httptest.NewRequest("GET", "/auth/login?redirect="+url.QueryEscape(redirect), nil))
	if rec.Code != http.StatusFound {
		e.t.Fatalf("login: %d", rec.Code)
	}
	oc := cookieNamed(rec, "obs_oidc")
	if oc == nil || !oc.HttpOnly || !oc.Secure || oc.Path != "/auth" || oc.SameSite != http.SameSiteLaxMode {
		e.t.Fatalf("bad obs_oidc cookie: %+v", oc)
	}
	loc, err := url.Parse(rec.Header().Get("Location"))
	if err != nil {
		e.t.Fatal(err)
	}
	state := loc.Query().Get("state")
	req := httptest.NewRequest("GET", "/auth/callback?code=c&state="+url.QueryEscape(state), nil)
	req.AddCookie(oc)
	return e.do(req)
}

func (e *env) sessionCookie() *http.Cookie {
	e.t.Helper()
	rec := e.login("/")
	c := cookieNamed(rec, "obs_session")
	if c == nil {
		e.t.Fatalf("no session cookie (status %d)", rec.Code)
	}
	return c
}

func (e *env) actions(result store.Result) map[string]bool {
	e.t.Helper()
	out := map[string]bool{}
	for _, en := range storetest.Audit(e.t, e.pool, "") {
		if en.Result == result {
			out[en.Action] = true
		}
	}
	return out
}

func TestLoginCallbackCreatesSessionAndAudits(t *testing.T) {
	e := newEnv(t)
	rec := e.login("/profile")
	if rec.Code != http.StatusFound || rec.Header().Get("Location") != "/profile" {
		t.Fatalf("callback: %d %q", rec.Code, rec.Header().Get("Location"))
	}
	sc := cookieNamed(rec, "obs_session")
	if sc == nil || !sc.HttpOnly || !sc.Secure || sc.Path != "/" || sc.MaxAge != 7*24*3600 {
		t.Fatalf("bad obs_session cookie: %+v", sc)
	}
	if oc := cookieNamed(rec, "obs_oidc"); oc == nil || oc.MaxAge >= 0 {
		t.Fatalf("obs_oidc not cleared: %+v", oc)
	}
	if _, err := e.store.SessionByToken(context.Background(), sc.Value); err != nil {
		t.Fatalf("session not stored: %v", err)
	}
	got := e.actions(store.ResultAllowed)
	for _, a := range []string{store.ActionAuthLogin, store.ActionOrgCreate, store.ActionPlatformGrantAdmin} {
		if !got[a] {
			t.Errorf("missing audit %s", a)
		}
	}
}

func TestLoginRejectsOffsiteRedirect(t *testing.T) {
	e := newEnv(t)
	for _, r := range []string{"//evil.com", "https://evil.com", `/\evil.com`, "/\t/evil.com", "/\n/evil.com", "evil", ""} {
		rec := e.login(r)
		if loc := rec.Header().Get("Location"); loc != "/" {
			t.Errorf("redirect %q → Location %q", r, loc)
		}
	}
}

func TestCallbackStateMismatch(t *testing.T) {
	e := newEnv(t)
	rec := e.do(httptest.NewRequest("GET", "/auth/login", nil))
	req := httptest.NewRequest("GET", "/auth/callback?code=c&state=wrong", nil)
	req.AddCookie(cookieNamed(rec, "obs_oidc"))
	rec = e.do(req)
	if rec.Code != http.StatusFound || rec.Header().Get("Location") != "/login?error=1" {
		t.Fatalf("status %d %q", rec.Code, rec.Header().Get("Location"))
	}
	if cookieNamed(rec, "obs_session") != nil {
		t.Fatal("session cookie set")
	}
	if !e.actions(store.ResultFailed)[store.ActionAuthLogin] {
		t.Fatal("no failed iam.sessions.create audit")
	}
	if len(e.actions(store.ResultAllowed)) != 0 {
		t.Fatal("unexpected allowed audit entries")
	}

	// No obs_oidc cookie at all.
	rec = e.do(httptest.NewRequest("GET", "/auth/callback?code=c&state=", nil))
	if rec.Code != http.StatusFound || rec.Header().Get("Location") != "/login?error=1" {
		t.Fatalf("no cookie: status %d", rec.Code)
	}
}

func TestCallbackIdPErrorIsSanitized(t *testing.T) {
	e := newEnv(t)
	rec := e.do(httptest.NewRequest("GET", "/auth/login", nil))
	loc, _ := url.Parse(rec.Header().Get("Location"))
	long := strings.Repeat("y", 2000)
	req := httptest.NewRequest("GET", "/auth/callback?error=%00"+long+"&state="+url.QueryEscape(loc.Query().Get("state")), nil)
	req.AddCookie(cookieNamed(rec, "obs_oidc"))
	req.Header.Set("User-Agent", "agent"+long)
	rec = e.do(req)
	if rec.Code != http.StatusFound || rec.Header().Get("Location") != "/login?error=1" {
		t.Fatalf("status %d %q", rec.Code, rec.Header().Get("Location"))
	}
	entries := storetest.Audit(t, e.pool, "")
	if len(entries) != 1 {
		t.Fatalf("entries %v", entries)
	}
	reason, _ := entries[0].Details["reason"].(string)
	if entries[0].Result != store.ResultFailed || reason != ("idp error: " + long)[:512] {
		t.Fatalf("reason %d bytes: %.40q", len(reason), reason)
	}
	if entries[0].UserAgent != ("agent" + long)[:512] {
		t.Fatalf("user agent %d bytes", len(entries[0].UserAgent))
	}
}

// minted logs Alice in with a session of kind and returns her, the session and its JWT.
func (e *env) minted(kind store.SessionKind) (store.User, store.Session, string) {
	e.t.Helper()
	ctx := context.Background()
	res, err := e.store.Login(ctx, store.Identity{Subject: "sub-1", Email: "a@x.io", Name: "Alice"})
	if err != nil {
		e.t.Fatal(err)
	}
	_, sess, err := e.store.CreateSession(ctx, res.User.ID, kind, time.Hour, "", "")
	if err != nil {
		e.t.Fatal(err)
	}
	tok, _, err := e.iss.Mint(res.User, sess)
	if err != nil {
		e.t.Fatal(err)
	}
	return res.User, sess, tok
}

func (e *env) whoami(method string, set func(*http.Request)) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, "/whoami", nil)
	set(req)
	return e.do(req)
}

func TestRequire(t *testing.T) {
	e := newEnv(t)
	_, _, tok := e.minted(store.SessionBrowser)
	for _, method := range []string{"GET", "POST"} {
		// No X-Observatory: the edge enforces CSRF before it attaches a JWT.
		if rec := e.whoami(method, func(r *http.Request) { r.Header.Set("Authorization", "Bearer "+tok) }); rec.Code != http.StatusOK || rec.Body.String() != "Alice" {
			t.Fatalf("%s: %d %q", method, rec.Code, rec.Body.String())
		}
	}
}

func TestRequireRejects(t *testing.T) {
	e := newEnv(t)
	user, sess, tok := e.minted(store.SessionBrowser)
	raw, _, err := e.store.CreateSession(context.Background(), user.ID, store.SessionCLI, time.Hour, "", "")
	if err != nil {
		t.Fatal(err)
	}
	sign := func(method jwt.SigningMethod, key any, edit func(*userClaims)) string {
		c := e.iss.claims(user, sess, time.Now())
		edit(&c)
		jt := jwt.NewWithClaims(method, c)
		jt.Header["kid"] = e.iss.kid
		s, err := jt.SignedString(key)
		if err != nil {
			t.Fatal(err)
		}
		return s
	}
	_, other, _ := ed25519.GenerateKey(rand.Reader)
	keep := func(*userClaims) {}
	for name, set := range map[string]func(*http.Request){
		"none":        func(*http.Request) {},
		"raw session": func(r *http.Request) { r.Header.Set("Authorization", "Bearer "+raw) },
		"jwt cookie":  func(r *http.Request) { r.AddCookie(&http.Cookie{Name: "obs_session", Value: tok}) },
		"basic":       func(r *http.Request) { r.Header.Set("Authorization", "Basic "+tok) },
		"hs256": func(r *http.Request) {
			r.Header.Set("Authorization", "Bearer "+sign(jwt.SigningMethodHS256, []byte(e.iss.key.Public().(ed25519.PublicKey)), keep))
		},
		"forged": func(r *http.Request) {
			r.Header.Set("Authorization", "Bearer "+sign(jwt.SigningMethodEdDSA, other, keep))
		},
		"expired": func(r *http.Request) {
			r.Header.Set("Authorization", "Bearer "+sign(jwt.SigningMethodEdDSA, e.iss.key, func(c *userClaims) {
				c.ExpiresAt = jwt.NewNumericDate(time.Now().Add(-time.Minute))
			}))
		},
		"wrong aud": func(r *http.Request) {
			r.Header.Set("Authorization", "Bearer "+sign(jwt.SigningMethodEdDSA, e.iss.key, func(c *userClaims) { c.Audience = jwt.ClaimStrings{"other"} }))
		},
		"no sk": func(r *http.Request) {
			r.Header.Set("Authorization", "Bearer "+sign(jwt.SigningMethodEdDSA, e.iss.key, func(c *userClaims) { c.SK = "" }))
		},
		"no sx": func(r *http.Request) {
			r.Header.Set("Authorization", "Bearer "+sign(jwt.SigningMethodEdDSA, e.iss.key, func(c *userClaims) { c.SX = 0 }))
		},
		"wrong iss": func(r *http.Request) {
			r.Header.Set("Authorization", "Bearer "+sign(jwt.SigningMethodEdDSA, e.iss.key, func(c *userClaims) { c.Issuer = "https://evil.test" }))
		},
	} {
		if rec := e.whoami("GET", set); rec.Code != http.StatusUnauthorized || rec.Body.String() != `{"error":"unauthorized"}`+"\n" {
			t.Errorf("%s: %d %q", name, rec.Code, rec.Body.String())
		}
	}
}

func TestRequirePrincipalFromClaims(t *testing.T) {
	e := newEnv(t)
	user, sess, tok := e.minted(store.SessionCLI)
	var got Principal
	h := e.a.Require(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) { got, _ = PrincipalFrom(r.Context()) }))
	do := func() int {
		req := httptest.NewRequest("GET", "/", nil)
		req.Header.Set("Authorization", "Bearer "+tok)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec.Code
	}
	if code := do(); code != http.StatusOK {
		t.Fatalf("status %d", code)
	}
	if got.User.ID != user.ID || got.User.Name != "Alice" || !got.User.PlatformAdmin {
		t.Fatalf("user %+v", got.User)
	}
	if got.Session.ID != sess.ID || got.Session.UserID != user.ID || got.Session.Kind != store.SessionCLI || got.Session.ExpiresAt.Unix() != sess.ExpiresAt.Unix() {
		t.Fatalf("session %+v, want %+v", got.Session, sess)
	}
	// The JWT is still valid, but its session is not.
	if err := e.store.RevokeSession(context.Background(), user.ID, sess.ID); err != nil {
		t.Fatal(err)
	}
	if code := do(); code != http.StatusUnauthorized {
		t.Fatalf("revoked session: %d", code)
	}
}

func TestLogoutRevokesSession(t *testing.T) {
	e := newEnv(t)
	sc := e.sessionCookie()
	sess, err := e.store.SessionByToken(context.Background(), sc.Value)
	if err != nil {
		t.Fatal(err)
	}
	cred, _, err := e.store.CreateRegistryCredential(context.Background(), sess.UserID, sess.ID, sess.ExpiresAt, time.Hour)
	if err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest("POST", "/auth/logout", nil)
	req.AddCookie(sc)
	if rec := e.do(req); rec.Code != http.StatusForbidden ||
		rec.Body.String() != `{"error":"missing X-Observatory header"}`+"\n" {
		t.Fatalf("cookie logout without header: %d %q", rec.Code, rec.Body.String())
	}

	req = httptest.NewRequest("POST", "/auth/logout", nil)
	req.AddCookie(sc)
	req.Header.Set("X-Observatory", "1")
	rec := e.do(req)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("logout: %d", rec.Code)
	}
	if c := cookieNamed(rec, "obs_session"); c == nil || c.MaxAge >= 0 {
		t.Fatalf("session cookie not cleared: %+v", c)
	}
	if _, err := e.store.SessionByToken(context.Background(), sc.Value); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("session after logout: %v", err)
	}
	if !e.actions(store.ResultAllowed)[store.ActionAuthLogout] {
		t.Fatal("no iam.sessions.delete audit")
	}
	// No consumer runs here, so the refusal must come from the logout itself, not from the bus.
	if _, err := e.store.RegistryCredentialUser(context.Background(), cred); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("registry credential after logout: %v", err)
	}
	if _, _, err := e.store.CreateRegistryCredential(context.Background(), sess.UserID, sess.ID, sess.ExpiresAt, time.Hour); !errors.Is(err, store.ErrSessionRevoked) {
		t.Fatalf("new registry credential after logout: %v", err)
	}

	req = httptest.NewRequest("POST", "/auth/logout", nil)
	req.AddCookie(sc)
	req.Header.Set("X-Observatory", "1")
	if rec := e.do(req); rec.Code != http.StatusUnauthorized {
		t.Fatalf("logout with a revoked session: %d", rec.Code)
	}
}

func TestRequestMetaUsesEdgeHeader(t *testing.T) {
	h := &Handler{EdgeToken: edgeToken}
	for token, want := range map[string]string{edgeToken: "203.0.113.7", "wrong": "192.0.2.1", "": "192.0.2.1"} {
		var got string
		h.TrustEdge(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) { got, _ = RequestMeta(r) })).
			ServeHTTP(httptest.NewRecorder(), func() *http.Request {
				r := httptest.NewRequest("GET", "/", nil)
				r.Header.Set(clientip.HeaderClientIP, "203.0.113.7")
				r.Header.Set(clientip.HeaderEdge, token)
				return r
			}())
		if got != want {
			t.Errorf("edge token %q: ip %q, want %q", token, got, want)
		}
	}
}

func TestAuditIPBehindEdge(t *testing.T) {
	for name, tc := range map[string]struct{ token, want string }{
		"through the edge": {edgeToken, "203.0.113.7"},
		"spoofed header":   {"forged", "192.0.2.1"},
	} {
		t.Run(name, func(t *testing.T) {
			e := newEnv(t)
			e.header = http.Header{}
			e.header.Set(clientip.HeaderClientIP, "203.0.113.7")
			e.header.Set(clientip.HeaderEdge, tc.token)
			if rec := e.login("/"); rec.Code != http.StatusFound {
				t.Fatalf("login: %d", rec.Code)
			}
			entries := storetest.Audit(t, e.pool, store.ActionAuthLogin)
			if len(entries) != 1 || entries[0].IP != tc.want {
				t.Fatalf("audit ip: %+v (want %s)", entries, tc.want)
			}
		})
	}
}

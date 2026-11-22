package iam

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/asterism-dev/observatory/internal/iam/store"
	"github.com/asterism-dev/observatory/internal/iam/store/storetest"
	"github.com/asterism-dev/observatory/internal/platform/clientip"
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

type loginEnv struct {
	t     *testing.T
	pool  *pgxpool.Pool
	store *store.Store
	a     *loginHandler
	h     http.Handler
	// header is added to every request, e.g. to arrive through the edge.
	header http.Header
}

const edgeToken = "0123456789abcdef0123456789abcdef"

func newLoginEnv(t *testing.T) *loginEnv {
	t.Helper()
	pool := storetest.NewDB(t)
	s := store.New(pool)
	h := &loginHandler{
		Store:       s,
		Auth:        fakeAuth{id: store.Identity{Subject: "sub-1", Email: "a@x.io", Name: "Alice"}, cliToken: "good"},
		Issuer:      "https://idp.test",
		CLIClientID: "cli-client",
		PublicURL:   &url.URL{Scheme: "https", Host: "hub.test"},
		EdgeToken:   edgeToken,
	}
	mux := http.NewServeMux()
	h.Routes(mux)
	return &loginEnv{t: t, pool: pool, store: s, a: h, h: mux}
}

func (e *loginEnv) do(r *http.Request) *httptest.ResponseRecorder {
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
func (e *loginEnv) login(redirect string) *httptest.ResponseRecorder {
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

func (e *loginEnv) sessionCookie() *http.Cookie {
	e.t.Helper()
	rec := e.login("/")
	c := cookieNamed(rec, "obs_session")
	if c == nil {
		e.t.Fatalf("no session cookie (status %d)", rec.Code)
	}
	return c
}

func (e *loginEnv) actions(result store.Result) map[string]bool {
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
	e := newLoginEnv(t)
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
	e := newLoginEnv(t)
	for _, r := range []string{"//evil.com", "https://evil.com", `/\evil.com`, "/\t/evil.com", "/\n/evil.com", "evil", ""} {
		rec := e.login(r)
		if loc := rec.Header().Get("Location"); loc != "/" {
			t.Errorf("redirect %q → Location %q", r, loc)
		}
	}
}

func TestCallbackStateMismatch(t *testing.T) {
	e := newLoginEnv(t)
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
	e := newLoginEnv(t)
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

func TestLogoutRevokesSession(t *testing.T) {
	e := newLoginEnv(t)
	sc := e.sessionCookie()
	sess, err := e.store.SessionByToken(context.Background(), sc.Value)
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
	if active, err := e.store.SessionActive(context.Background(), sess.ID); err != nil || active {
		t.Fatalf("session active after logout: %v %v", active, err)
	}

	req = httptest.NewRequest("POST", "/auth/logout", nil)
	req.AddCookie(sc)
	req.Header.Set("X-Observatory", "1")
	if rec := e.do(req); rec.Code != http.StatusUnauthorized {
		t.Fatalf("logout with a revoked session: %d", rec.Code)
	}
}

func TestAuditIPBehindEdge(t *testing.T) {
	for name, tc := range map[string]struct{ token, want string }{
		"through the edge": {edgeToken, "203.0.113.7"},
		"spoofed header":   {"forged", "192.0.2.1"},
	} {
		t.Run(name, func(t *testing.T) {
			e := newLoginEnv(t)
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

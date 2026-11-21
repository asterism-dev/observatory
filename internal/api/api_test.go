package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/asterism-dev/observatory/internal/auth"
	"github.com/asterism-dev/observatory/internal/authz"
	"github.com/asterism-dev/observatory/internal/db/dbtest"
	"github.com/asterism-dev/observatory/internal/disks"
	"github.com/asterism-dev/observatory/internal/fleet"
	"github.com/asterism-dev/observatory/internal/network"
	"github.com/asterism-dev/observatory/internal/platform/identity"
	"github.com/asterism-dev/observatory/internal/seal"
	"github.com/asterism-dev/observatory/internal/store"
	"github.com/asterism-dev/observatory/internal/store/storetest"
	"github.com/asterism-dev/observatory/internal/version"
)

type env struct {
	t     *testing.T
	pool  *pgxpool.Pool
	store *store.Store
	z     *authz.Service
	net   *network.Hub
	disks *disks.Hub
	api   *api
	h     http.Handler

	iss      *auth.Issuer
	verifier *fakeVerifier
	oauth    *fakeOAuth
	google   *fakeGoogle
}

const gatewayToken = "0123456789abcdef0123456789abcdef"

func newEnv(t *testing.T) *env {
	t.Helper()
	pool := dbtest.New(t)
	s := store.New(pool).WithMasterKey(seal.Key{1})
	z, err := authz.NewService(context.Background(), s.PolicySnapshot)
	if err != nil {
		t.Fatal(err)
	}
	iss, err := auth.NewIssuer(context.Background(), s, "https://hub.test")
	if err != nil {
		t.Fatal(err)
	}
	jwks := httptest.NewServer(iss.JWKSHandler())
	t.Cleanup(jwks.Close)
	idv, err := identity.NewVerifier(jwks.URL, "https://hub.test", "unused-service-token", nil)
	if err != nil {
		t.Fatal(err)
	}
	a := &auth.Handler{Store: s, PublicURL: &url.URL{Scheme: "https", Host: "hub.test"}, Verifier: idv}
	nh, err := network.Load(context.Background(), s, z, network.Config{GatewayHost: "hub.test", GatewayToken: gatewayToken})
	if err != nil {
		t.Fatal(err)
	}
	f, err := fleet.New(context.Background(), s, z, nh)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(f.Close)
	dh := disks.New(s, "storage-token", f.NotifyOrg)
	fv, fo, fg := &fakeVerifier{}, &fakeOAuth{}, &fakeGoogle{}
	h, handler := newAPI(s, a, z, f, nh, dh, Integrations{Verifier: fv, OAuth: fo, Google: fg}, nil, nil, nil, nil, "https://docs.test/", func() string { return "v9.9.9" }, "v1.18.0", nil, 14*24*time.Hour)
	e := &env{t: t, pool: pool, store: s, z: z, net: nh, disks: dh, api: h, iss: iss, verifier: fv, oauth: fo, google: fg}
	e.h = e.asEdge(handler)
	return e
}

// asEdge swaps a session bearer on /api/ for a JWT as the edge does; an unknown or revoked session loses the credential.
func (e *env) asEdge(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		tok, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		if !ok || !strings.HasPrefix(tok, "obs_") || !strings.HasPrefix(r.URL.Path, "/api/") {
			next.ServeHTTP(w, r)
			return
		}
		r.Header.Del("Authorization")
		if sess, err := e.store.SessionByToken(r.Context(), tok); err == nil {
			u, err := e.store.User(r.Context(), sess.UserID)
			if err != nil {
				e.t.Errorf("edge: load user: %v", err)
				return
			}
			jwt, _, err := e.iss.Mint(u, sess)
			if err != nil {
				e.t.Errorf("edge: mint: %v", err)
				return
			}
			r.Header.Set("Authorization", "Bearer "+jwt)
		}
		next.ServeHTTP(w, r)
	})
}

func (e *env) newFleet() *fleet.Fleet {
	e.t.Helper()
	f, err := fleet.New(context.Background(), e.store, e.z, e.net)
	if err != nil {
		e.t.Fatal(err)
	}
	e.t.Cleanup(f.Close)
	return f
}

// user logs an identity in and returns the user and a fresh session token.
func (e *env) user(sub string) (store.User, string) {
	e.t.Helper()
	ctx := context.Background()
	res, err := e.store.Login(ctx, store.Identity{Subject: sub, Email: sub + "@x.io", Name: sub})
	if err != nil {
		e.t.Fatal(err)
	}
	// The login handler reloads the policy after creating a user and personal org.
	e.reload()
	return res.User, e.session(res.User)
}

func (e *env) session(u store.User) string {
	e.t.Helper()
	tok, _, err := e.store.CreateSession(context.Background(), u.ID, store.SessionBrowser, time.Hour, "ua", "1.2.3.4")
	if err != nil {
		e.t.Fatal(err)
	}
	return tok
}

func (e *env) do(method, path, token string) *httptest.ResponseRecorder {
	return e.send(method, path, token, "")
}

func (e *env) send(method, path, token, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+token)
	rec := httptest.NewRecorder()
	e.h.ServeHTTP(rec, req)
	return rec
}

func decode(t *testing.T, rec *httptest.ResponseRecorder, v any) {
	t.Helper()
	if err := json.Unmarshal(rec.Body.Bytes(), v); err != nil {
		t.Fatalf("decode %q: %v", rec.Body.String(), err)
	}
}

func (e *env) audit(action string) []store.AuditEntry {
	e.t.Helper()
	return storetest.Audit(e.t, e.pool, action)
}

// canReadAudit is the check the audit service makes for the org's trail.
func (e *env) canReadAudit(user, org uuid.UUID) bool {
	return e.z.Enforcer().Allowed(authz.UserSubject(user), org, authz.ObjAudit, "audit.events.list")
}

func TestMe(t *testing.T) {
	e := newEnv(t)
	u, tok := e.user("alice")
	rec := e.do("GET", "/api/me", tok)
	if rec.Code != 200 {
		t.Fatalf("status %d", rec.Code)
	}
	var got struct {
		User struct {
			ID, Email, Name string
			PlatformAdmin   bool
		}
		Orgs []struct {
			ID, Name, Role string
			Personal       bool
		}
		DocsURL                string
		Version, LatestVersion string
	}
	decode(t, rec, &got)
	if got.DocsURL != "https://docs.test/" {
		t.Fatalf("docsURL: %q", got.DocsURL)
	}
	if got.Version != version.Version || got.LatestVersion != "v9.9.9" {
		t.Fatalf("version %q, latestVersion %q", got.Version, got.LatestVersion)
	}
	if got.User.ID != u.ID.String() || got.User.Email != "alice@x.io" || !got.User.PlatformAdmin {
		t.Fatalf("user: %+v", got.User)
	}
	if len(got.Orgs) != 1 || !got.Orgs[0].Personal || got.Orgs[0].Role != "owner" {
		t.Fatalf("orgs: %+v", got.Orgs)
	}
	if !strings.Contains(rec.Body.String(), `"platformAdmin"`) {
		t.Fatalf("not camelCase: %s", rec.Body.String())
	}
	if rec := e.do("GET", "/api/me", "nope"); rec.Code != 401 {
		t.Fatalf("bad token: %d", rec.Code)
	}
}

func TestSessionsListAndRevoke(t *testing.T) {
	e := newEnv(t)
	u, tok1 := e.user("alice")
	tok2 := e.session(u)
	_, bobTok := e.user("bob")

	rec := e.do("GET", "/api/me/sessions", tok1)
	var list []struct {
		ID      string
		Kind    string
		Current bool
	}
	decode(t, rec, &list)
	if len(list) != 2 {
		t.Fatalf("sessions: %s", rec.Body.String())
	}
	current := 0
	var other string
	for _, s := range list {
		if s.Current {
			current++
		} else {
			other = s.ID
		}
	}
	if current != 1 || other == "" {
		t.Fatalf("current flags: %s", rec.Body.String())
	}
	for _, k := range []string{"userAgent", "ip", "createdAt", "lastUsedAt", "expiresAt"} {
		if !strings.Contains(rec.Body.String(), `"`+k+`"`) {
			t.Fatalf("missing %s: %s", k, rec.Body.String())
		}
	}

	if rec := e.do("DELETE", "/api/me/sessions/"+other, tok1); rec.Code != 204 {
		t.Fatalf("revoke other: %d", rec.Code)
	}
	if es := e.audit("iam.sessions.revoke"); len(es) != 1 || es[0].Resource != "session/"+other || es[0].ActorID != u.ID.String() {
		t.Fatalf("audit: %+v", es)
	}
	if rec := e.do("GET", "/api/me", tok2); rec.Code != 401 {
		t.Fatalf("revoked token: %d", rec.Code)
	}

	var bobSessions []struct{ ID string }
	decode(t, e.do("GET", "/api/me/sessions", bobTok), &bobSessions)
	if rec := e.do("DELETE", "/api/me/sessions/"+bobSessions[0].ID, tok1); rec.Code != 404 {
		t.Fatalf("other user's session: %d", rec.Code)
	}
	if rec := e.do("DELETE", "/api/me/sessions/not-a-uuid", tok1); rec.Code != 404 {
		t.Fatalf("bad id: %d", rec.Code)
	}

	cur := []struct {
		ID      string
		Current bool
	}{}
	decode(t, e.do("GET", "/api/me/sessions", tok1), &cur)
	if rec := e.do("DELETE", "/api/me/sessions/"+cur[0].ID, tok1); rec.Code != 204 {
		t.Fatalf("revoke current: %d", rec.Code)
	}
	if rec := e.do("GET", "/api/me", tok1); rec.Code != 401 {
		t.Fatalf("after revoking current: %d", rec.Code)
	}
}

func TestAuthRoutesWinOverAPIMount(t *testing.T) {
	e := newEnv(t)
	a := &auth.Handler{Store: e.store, Issuer: "https://idp.test", PublicURL: &url.URL{Scheme: "https", Host: "hub.test"}}
	mux := http.NewServeMux()
	a.Routes(mux)
	mux.Handle("/api/", New(e.store, a, e.z, e.newFleet(), e.net, e.disks, Integrations{}, nil, nil, nil, nil, "", func() string { return "" }, "v1.18.0", nil, 14*24*time.Hour))
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest("GET", "/api/auth/config", nil))
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), "https://idp.test") {
		t.Fatalf("/api/auth/config: %d %s", rec.Code, rec.Body.String())
	}
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest("GET", "/api/me", nil))
	if rec.Code != 401 {
		t.Fatalf("/api/me unauthenticated: %d", rec.Code)
	}
}

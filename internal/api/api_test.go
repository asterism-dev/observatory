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
	"github.com/asterism-dev/observatory/internal/iam"
	"github.com/asterism-dev/observatory/internal/iaminternal"
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
	f, err := fleet.New(context.Background(), s, z, nh, iaminternal.Server{Store: s})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(f.Close)
	dh := disks.New(s, "storage-token", f.NotifyOrg)
	fv, fo, fg := &fakeVerifier{}, &fakeOAuth{}, &fakeGoogle{}
	h, handler := newAPI(s, iaminternal.Server{Store: s}, ResourceServer{Store: s}, a, z, f, nh, dh, Integrations{Verifier: fv, OAuth: fo, Google: fg}, nil, nil, nil, nil, "https://docs.test/", func() string { return "v9.9.9" }, "v1.18.0", nil, 14*24*time.Hour)
	e := &env{t: t, pool: pool, store: s, z: z, net: nh, disks: dh, api: h, iss: iss, verifier: fv, oauth: fo, google: fg}
	iamHandler, err := iam.Handler(iam.Deps{Store: s, Policy: z, Verifier: idv, Resources: ResourceServer{Store: s}, Notify: f.NotifyOrg, Pool: pool, PublicURL: a.PublicURL})
	if err != nil {
		t.Fatal(err)
	}
	// No mux: it would clean the paths the Grafana proxies must see raw.
	e.h = e.asEdge(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, iam.Prefix+"/") {
			iamHandler.ServeHTTP(w, r)
			return
		}
		handler.ServeHTTP(w, r)
	}))
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
	f, err := fleet.New(context.Background(), e.store, e.z, e.net, iaminternal.Server{Store: e.store})
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

func TestAuthRoutesWinOverAPIMount(t *testing.T) {
	e := newEnv(t)
	a := &auth.Handler{Store: e.store, Issuer: "https://idp.test", PublicURL: &url.URL{Scheme: "https", Host: "hub.test"}}
	mux := http.NewServeMux()
	a.Routes(mux)
	mux.Handle("/api/", New(e.store, e.api.iam, e.api.resources, a, e.z, e.newFleet(), e.net, e.disks, Integrations{}, nil, nil, nil, nil, "", func() string { return "" }, "v1.18.0", nil, 14*24*time.Hour))
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest("GET", "/api/auth/config", nil))
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), "https://idp.test") {
		t.Fatalf("/api/auth/config: %d %s", rec.Code, rec.Body.String())
	}
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest("GET", "/api/info", nil))
	if rec.Code != 401 {
		t.Fatalf("/api/info unauthenticated: %d", rec.Code)
	}
}

func TestInfoEndpoint(t *testing.T) {
	e := newEnv(t)
	_, tok := e.user("alice")
	rec := e.do("GET", "/api/info", tok)
	var got struct{ DocsURL, Version, LatestVersion string }
	decode(t, rec, &got)
	if rec.Code != 200 || got.DocsURL != "https://docs.test/" || got.Version != version.Version || got.LatestVersion != "v9.9.9" {
		t.Fatalf("info: %d %s", rec.Code, rec.Body.String())
	}
	if rec := e.do("GET", "/api/info", "nope"); rec.Code != 401 {
		t.Fatalf("bad token: %d", rec.Code)
	}
}

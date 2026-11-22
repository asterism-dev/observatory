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
	"github.com/asterism-dev/observatory/internal/platform/iamtest"
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
	iam   *iamtest.Server
	pol   *iamtest.Policy
	z     *authz.Service
	net   *network.Hub
	disks *disks.Hub
	api   *api
	h     http.Handler

	jwts     *identity.Verifier
	verifier *fakeVerifier
	oauth    *fakeOAuth
	google   *fakeGoogle
	roles    map[uuid.UUID]authz.CustomRole
}

const gatewayToken = "0123456789abcdef0123456789abcdef"

func newEnv(t *testing.T) *env {
	t.Helper()
	pool := dbtest.New(t)
	s := store.New(pool).WithMasterKey(seal.Key{1})
	fake := iamtest.New(t)
	pol := fake.Policy(t, s.OwnerRefs)
	z := pol.Service
	jwts, err := identity.NewVerifier(fake.URL(), iamtest.Issuer, fake.ServiceToken(), nil)
	if err != nil {
		t.Fatal(err)
	}
	a := &auth.Handler{PublicURL: &url.URL{Scheme: "https", Host: "hub.test"}, Verifier: jwts, IAM: fake.Client()}
	nh, err := network.Load(context.Background(), s, z, network.Config{GatewayHost: "hub.test", GatewayToken: gatewayToken})
	if err != nil {
		t.Fatal(err)
	}
	f, err := fleet.New(context.Background(), s, z, nh, fake.Client())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(f.Close)
	dh := disks.New(s, "storage-token", f.NotifyOrg)
	fv, fo, fg := &fakeVerifier{}, &fakeOAuth{}, &fakeGoogle{}
	h, handler := newAPI(s, fake.Client(), a, z, f, nh, dh, Integrations{Verifier: fv, OAuth: fo, Google: fg}, nil, nil, nil, nil, "https://docs.test/", func() string { return "v9.9.9" }, "v1.18.0", nil, 14*24*time.Hour)
	// No mux: it would clean the paths the Grafana proxies must see raw.
	return &env{t: t, pool: pool, store: s, iam: fake, pol: pol, z: z, net: nh, disks: dh, api: h, h: handler, jwts: jwts, verifier: fv, oauth: fo, google: fg, roles: map[uuid.UUID]authz.CustomRole{}}
}

func (e *env) newFleet() *fleet.Fleet {
	e.t.Helper()
	f, err := fleet.New(context.Background(), e.store, e.z, e.net, e.iam.Client())
	if err != nil {
		e.t.Fatal(err)
	}
	e.t.Cleanup(f.Close)
	return f
}

// user adds a user with a personal org to iam and returns it with a browser session's JWT.
func (e *env) user(sub string) (iamtest.User, string) {
	e.t.Helper()
	u := e.iam.AddUser(sub)
	e.reload()
	return u, e.session(u)
}

func (e *env) session(u iamtest.User) string {
	e.t.Helper()
	jwt, _ := e.iam.JWT(u.ID, "browser")
	return jwt
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

// The hub asks iam for the session on every request, so a revoke refuses before the JWT expires.
func TestRevokedSessionRefusedAtOnce(t *testing.T) {
	e := newEnv(t)
	alice, _ := e.user("alice")
	jwt, session := e.iam.JWT(alice.ID, "browser")
	if rec := e.do("GET", "/api/info", jwt); rec.Code != 200 {
		t.Fatalf("live session: %d", rec.Code)
	}
	e.iam.RevokeSession(session)
	if rec := e.do("GET", "/api/info", jwt); rec.Code != 401 {
		t.Fatalf("revoked session: %d %s", rec.Code, rec.Body.String())
	}
}

package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/asterism-dev/observatory/internal/auth"
	"github.com/asterism-dev/observatory/internal/authz"
	"github.com/asterism-dev/observatory/internal/db/dbtest"
	"github.com/asterism-dev/observatory/internal/disks"
	"github.com/asterism-dev/observatory/internal/fleet"
	"github.com/asterism-dev/observatory/internal/network"
	"github.com/asterism-dev/observatory/internal/seal"
	"github.com/asterism-dev/observatory/internal/store"
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
	a := &auth.Handler{Store: s, PublicURL: &url.URL{Scheme: "https", Host: "hub.test"}}
	nh, err := network.Load(context.Background(), s, z, network.Config{GatewayHost: "hub.test", GatewayToken: gatewayToken})
	if err != nil {
		t.Fatal(err)
	}
	f := fleet.New(s, z, nh)
	dh := disks.New(s, "storage-token", f.NotifyOrg)
	h, handler := newAPI(s, a, z, f, nh, dh)
	return &env{t: t, pool: pool, store: s, z: z, net: nh, disks: dh, api: h, h: handler}
}

// user logs an identity in and returns the user and a fresh session token.
func (e *env) user(sub string) (store.User, string) {
	e.t.Helper()
	ctx := context.Background()
	res, err := e.store.Login(ctx, store.Identity{Subject: sub, Email: sub + "@x.io", Name: sub})
	if err != nil {
		e.t.Fatal(err)
	}
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
	es, err := e.store.AuditEntries(context.Background(), store.AuditFilter{Action: action})
	if err != nil {
		e.t.Fatal(err)
	}
	return es
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
	}
	decode(t, rec, &got)
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
	if es := e.audit("session:revoke"); len(es) != 1 || es[0].Resource != "session/"+other || es[0].ActorID != u.ID.String() {
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

func TestAdminAudit(t *testing.T) {
	e := newEnv(t)
	_, adminTok := e.user("alice")
	bob, userTok := e.user("bob")

	for range 4 {
		if err := e.store.Audit(context.Background(), store.AuditEntry{ActorKind: store.ActorSystem, Action: "test:seed", Result: store.ResultAllowed}); err != nil {
			t.Fatal(err)
		}
	}
	var page struct {
		Entries []struct {
			ID     int64
			Action string
		}
		Next *int64
	}
	rec := e.do("GET", "/api/admin/audit?limit=2", adminTok)
	if rec.Code != 200 {
		t.Fatalf("admin: %d %s", rec.Code, rec.Body.String())
	}
	decode(t, rec, &page)
	if len(page.Entries) != 2 || page.Next == nil || *page.Next != page.Entries[1].ID {
		t.Fatalf("page 1: %s", rec.Body.String())
	}
	var page2 struct {
		Entries []struct{ ID int64 }
		Next    *int64
	}
	decode(t, e.do("GET", "/api/admin/audit?limit=2&before="+strconv.FormatInt(*page.Next, 10), adminTok), &page2)
	if len(page2.Entries) == 0 || page2.Entries[0].ID >= *page.Next {
		t.Fatalf("page 2: %+v", page2)
	}

	rec = e.do("GET", "/api/admin/audit?action=nothing", adminTok)
	if !strings.Contains(rec.Body.String(), `"entries":[]`) || !strings.Contains(rec.Body.String(), `"next":null`) {
		t.Fatalf("empty: %s", rec.Body.String())
	}

	rec = e.do("GET", "/api/admin/audit", userTok)
	if rec.Code != 403 {
		t.Fatalf("non-admin: %d", rec.Code)
	}
	es := e.audit("audit:read")
	if len(es) != 1 || es[0].Result != store.ResultDenied || es[0].ActorID != bob.ID.String() {
		t.Fatalf("denied audit: %+v", es)
	}

	for _, q := range []string{"limit=abc", "limit=0", "before=x", "from=yesterday", "org=zzz"} {
		if rec := e.do("GET", "/api/admin/audit?"+q, adminTok); rec.Code != 400 {
			t.Fatalf("%s: %d", q, rec.Code)
		}
	}
}

func TestAuthRoutesWinOverAPIMount(t *testing.T) {
	e := newEnv(t)
	a := &auth.Handler{Store: e.store, Issuer: "https://idp.test", PublicURL: &url.URL{Scheme: "https", Host: "hub.test"}}
	mux := http.NewServeMux()
	a.Routes(mux)
	mux.Handle("/api/", New(e.store, a, e.z, fleet.New(e.store, e.z, e.net), e.net, e.disks))
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

package iam

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync"
	"testing"
	"time"

	"connectrpc.com/connect"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	hubv1 "github.com/asterism-dev/observatory/gen/go/observatory/hub/v1"
	"github.com/asterism-dev/observatory/gen/go/observatory/hub/v1/hubv1connect"
	"github.com/asterism-dev/observatory/gen/go/observatory/iam/v1/iamv1connect"
	"github.com/asterism-dev/observatory/internal/auth"
	"github.com/asterism-dev/observatory/internal/authz"
	"github.com/asterism-dev/observatory/internal/db/dbtest"
	"github.com/asterism-dev/observatory/internal/platform/identity"
	"github.com/asterism-dev/observatory/internal/seal"
	"github.com/asterism-dev/observatory/internal/store"
	"github.com/asterism-dev/observatory/internal/store/storetest"
)

type env struct {
	t         *testing.T
	pool      *pgxpool.Pool
	store     *store.Store
	z         *authz.Service
	iss       *auth.Issuer
	srv       *httptest.Server
	client    iamv1connect.IAMServiceClient
	resources *fakeResources

	mu       sync.Mutex
	notified []uuid.UUID
}

// fakeResources reports the subjects in owners as owning VMs in any org.
type fakeResources struct {
	hubv1connect.ResourceServiceClient
	mu     sync.Mutex
	owners map[string]bool
}

func (f *fakeResources) CheckOwnership(_ context.Context, req *connect.Request[hubv1.CheckOwnershipRequest]) (*connect.Response[hubv1.CheckOwnershipResponse], error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return connect.NewResponse(&hubv1.CheckOwnershipResponse{OwnsVms: f.owners[req.Msg.Subject]}), nil
}

func (f *fakeResources) setOwner(subject string, owns bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.owners[subject] = owns
}

func newEnv(t *testing.T) *env {
	t.Helper()
	ctx := context.Background()
	pool := dbtest.New(t)
	s := store.New(pool).WithMasterKey(seal.Key{1})
	z, err := authz.NewService(ctx, s.PolicySnapshot)
	if err != nil {
		t.Fatal(err)
	}
	iss, err := auth.NewIssuer(ctx, s, "https://hub.test")
	if err != nil {
		t.Fatal(err)
	}
	jwks := httptest.NewServer(iss.JWKSHandler())
	t.Cleanup(jwks.Close)
	v, err := identity.NewVerifier(jwks.URL, "https://hub.test", "unused-service-token", nil)
	if err != nil {
		t.Fatal(err)
	}
	e := &env{t: t, pool: pool, store: s, z: z, iss: iss, resources: &fakeResources{owners: map[string]bool{}}}
	h, err := Handler(Deps{Store: s, Policy: z, Verifier: v, Resources: e.resources, Pool: pool,
		PublicURL: &url.URL{Scheme: "https", Host: "hub.test"}, Notify: e.notify})
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	mux.Handle("/api/iam/v1/", h)
	e.srv = httptest.NewServer(mux)
	t.Cleanup(e.srv.Close)
	e.client = iamv1connect.NewIAMServiceClient(e.srv.Client(), e.srv.URL+"/api/iam/v1")
	return e
}

func (e *env) notify(org uuid.UUID) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.notified = append(e.notified, org)
}

func (e *env) takeNotified() []uuid.UUID {
	e.mu.Lock()
	defer e.mu.Unlock()
	n := e.notified
	e.notified = nil
	return n
}

// user logs an identity in and returns the user and a JWT for a fresh session, as the edge would attach it.
func (e *env) user(sub string) (store.User, string) {
	e.t.Helper()
	res, err := e.store.Login(context.Background(), store.Identity{Subject: sub, Email: sub + "@x.io", Name: sub})
	if err != nil {
		e.t.Fatal(err)
	}
	e.reload()
	jwt, _ := e.session(res.User)
	return res.User, jwt
}

// session returns a JWT and the session token behind it.
func (e *env) session(u store.User) (string, string) {
	e.t.Helper()
	tok, sess, err := e.store.CreateSession(context.Background(), u.ID, store.SessionBrowser, time.Hour, "ua", "1.2.3.4")
	if err != nil {
		e.t.Fatal(err)
	}
	jwt, _, err := e.iss.Mint(u, sess)
	if err != nil {
		e.t.Fatal(err)
	}
	return jwt, tok
}

func (e *env) reload() {
	e.t.Helper()
	if err := e.z.Reload(context.Background()); err != nil {
		e.t.Fatal(err)
	}
}

func (e *env) exec(sql string, args ...any) {
	e.t.Helper()
	if _, err := e.pool.Exec(context.Background(), sql, args...); err != nil {
		e.t.Fatal(err)
	}
}

func (e *env) addMember(org uuid.UUID, u store.User, role store.Role) {
	e.t.Helper()
	e.exec(`INSERT INTO memberships (org_id, user_id, role) VALUES ($1, $2, $3)`, org, u.ID, role)
	e.reload()
}

func (e *env) role(org uuid.UUID, u store.User) store.Role {
	e.t.Helper()
	m, err := e.store.Membership(context.Background(), org, u.ID)
	if err != nil {
		return ""
	}
	return m.Role
}

func (e *env) personalOrg(u store.User) uuid.UUID {
	e.t.Helper()
	ms, err := e.store.Memberships(context.Background(), u.ID)
	if err != nil || len(ms) == 0 || !ms[0].Org.Personal {
		e.t.Fatalf("personal org: %+v %v", ms, err)
	}
	return ms[0].Org.ID
}

// bindRole creates a custom role with one rule in org and binds it to subject.
func (e *env) bindRole(org uuid.UUID, subject, effect, object, action string) {
	e.t.Helper()
	var role uuid.UUID
	if err := e.pool.QueryRow(context.Background(), `INSERT INTO roles (org_id, name) VALUES ($1, $2) RETURNING id`, org, uuid.NewString()[:8]).Scan(&role); err != nil {
		e.t.Fatal(err)
	}
	e.exec(`INSERT INTO role_rules (role_id, position, effect, object, action) VALUES ($1, 0, $2, $3, $4)`, role, effect, object, action)
	e.exec(`INSERT INTO role_bindings (org_id, subject, role_id) VALUES ($1, $2, $3)`, org, subject, role)
	e.reload()
}

func (e *env) audit(action string) []store.AuditEntry {
	e.t.Helper()
	return storetest.Audit(e.t, e.pool, action)
}

// denied counts all denied audit events.
func (e *env) denied() int {
	e.t.Helper()
	n := 0
	for _, a := range e.audit("") {
		if a.Result == store.ResultDenied {
			n++
		}
	}
	return n
}

func req[T any](jwt string, m *T) *connect.Request[T] {
	r := connect.NewRequest(m)
	r.Header().Set("Authorization", "Bearer "+jwt)
	return r
}

func wantCode(t *testing.T, err error, code connect.Code) {
	t.Helper()
	if connect.CodeOf(err) != code {
		t.Fatalf("got %v, want %v", err, code)
	}
}

func must[T any](t *testing.T) func(*connect.Response[T], error) *T {
	return func(res *connect.Response[T], err error) *T {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
		return res.Msg
	}
}

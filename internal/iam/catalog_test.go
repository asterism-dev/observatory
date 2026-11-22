package iam

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"
	"github.com/jackc/pgx/v5/pgxpool"

	iamv1 "github.com/asterism-dev/observatory/gen/go/observatory/iam/v1"
	"github.com/asterism-dev/observatory/gen/go/observatory/iam/v1/iamv1connect"
	"github.com/asterism-dev/observatory/internal/authz"
	"github.com/asterism-dev/observatory/internal/iam/store"
	"github.com/asterism-dev/observatory/internal/iam/store/storetest"
	platformauthz "github.com/asterism-dev/observatory/internal/platform/authz"
	"github.com/asterism-dev/observatory/internal/platform/catalog"
	"github.com/asterism-dev/observatory/internal/platform/servicetoken"
	"github.com/asterism-dev/observatory/internal/seal"
)

// catalogEnv is iam's internal API over the real store and policy, started as build starts it.
type catalogEnv struct {
	pool *pgxpool.Pool
	srv  *httptest.Server
}

func newCatalogEnv(t *testing.T, pool *pgxpool.Pool) *catalogEnv {
	t.Helper()
	ctx := context.Background()
	s := store.New(pool)
	z, err := authz.NewService(ctx, s.PolicySnapshot)
	if err != nil {
		t.Fatal(err)
	}
	if err := registerOwnCatalog(ctx, s, z); err != nil {
		t.Fatal(err)
	}
	iss, err := NewIssuer(ctx, s, seal.Key{1}, issuerURL)
	if err != nil {
		t.Fatal(err)
	}
	ps := platformauthz.NewPublisher()
	z.OnReload(func(_, p authz.Policy) { ps.Publish(p) })
	h := servicetoken.Middleware(map[string]string{"audit": otherTok, "hub": hubTok, "compute": computeTok})(
		internalHandler(iss, ps, internalServer{Store: s}, catalogServer{Store: s, Policy: z}))
	e := &catalogEnv{pool: pool, srv: httptest.NewServer(h)}
	t.Cleanup(e.srv.Close)
	return e
}

func (e *catalogEnv) register(tok string, c authz.Catalog) (string, error) {
	client := iamv1connect.NewCatalogInternalServiceClient(e.srv.Client(), e.srv.URL+"/internal/iam", connect.WithInterceptors(servicetoken.Bearer(tok)))
	resp, err := client.RegisterCatalog(context.Background(), connect.NewRequest(&iamv1.RegisterCatalogRequest{Catalog: catalog.ToProto(c)}))
	if err != nil {
		return "", err
	}
	return resp.Msg.GetDigest(), nil
}

// watch returns a policy stream as hub sees it.
func (e *catalogEnv) watch(t *testing.T) *connect.ServerStreamForClient[iamv1.Policy] {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)
	client := iamv1connect.NewPolicyServiceClient(e.srv.Client(), e.srv.URL+"/internal/iam", connect.WithInterceptors(servicetoken.Bearer(hubTok)))
	stream, err := client.WatchPolicy(ctx, connect.NewRequest(&iamv1.WatchPolicyRequest{}))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = stream.Close() })
	return stream
}

// waitCatalog reads stream until it carries service's catalog with digest.
func waitCatalog(t *testing.T, stream *connect.ServerStreamForClient[iamv1.Policy], service, digest string) {
	t.Helper()
	for stream.Receive() {
		for _, pc := range stream.Msg().GetCatalogs() {
			c, err := catalog.FromProto(pc)
			if err != nil {
				t.Fatal(err)
			}
			if c.Service == service && catalog.Digest(c) == digest {
				return
			}
		}
	}
	t.Fatalf("stream ended without %s's catalog %s: %v", service, digest, stream.Err())
}

func storedDigest(t *testing.T, pool *pgxpool.Pool, service string) string {
	t.Helper()
	var d string
	_ = pool.QueryRow(context.Background(), `SELECT digest FROM catalogs WHERE service = $1`, service).Scan(&d)
	return d
}

func auditCatalog(actions ...string) authz.Catalog {
	c := authz.Catalog{Service: "audit"}
	for _, a := range actions {
		c.Actions = append(c.Actions, authz.Action{Name: a, Object: "org/audit"})
	}
	return c
}

func TestRegisterCatalogRequiresServiceToken(t *testing.T) {
	e := newCatalogEnv(t, storetest.NewDB(t))
	for _, tok := range []string{"", "wrong-wrong-wrong-wrong-wrong-wrong", sessionToken, computeTok} {
		_, err := e.register(tok, auditCatalog("audit.events.list"))
		if connect.CodeOf(err) != connect.CodePermissionDenied {
			t.Errorf("token %q: %v", tok, err)
		}
	}
	if d := storedDigest(t, e.pool, "audit"); d != "" {
		t.Fatalf("catalog stored without a service token: %s", d)
	}
}

func TestRegisterForeignPrefixRejected(t *testing.T) {
	e := newCatalogEnv(t, storetest.NewDB(t))
	good := auditCatalog("audit.events.list")
	digest, err := e.register(otherTok, good)
	if err != nil {
		t.Fatal(err)
	}
	hubCat := authz.Catalog{Service: "hub", Actions: []authz.Action{{Name: "compute.instances.get", Object: "org/vms/*"}}}
	cases := map[string]struct {
		tok string
		c   authz.Catalog
	}{
		"foreign action":         {otherTok, auditCatalog("audit.events.list", "compute.instances.get")},
		"foreign role default":   {otherTok, authz.Catalog{Service: "audit", Actions: good.Actions, RoleDefaults: []authz.RoleDefault{{Role: "admin", Object: "org/members", Action: "iam.members.delete"}}}},
		"catalog of another svc": {otherTok, hubCat},
		"service not the caller": {hubTok, good},
		"hub claims iam":         {hubTok, authz.Catalog{Service: "hub", Actions: hubCat.Actions, RoleDefaults: []authz.RoleDefault{{Role: "member", Object: "org/members", Action: "iam.*"}}}},
	}
	for name, tc := range cases {
		if _, err := e.register(tc.tok, tc.c); connect.CodeOf(err) != connect.CodeInvalidArgument {
			t.Errorf("%s: %v", name, err)
		}
	}
	if d := storedDigest(t, e.pool, "audit"); d != digest {
		t.Fatalf("audit's catalog changed to %s", d)
	}
	if d := storedDigest(t, e.pool, "hub"); d != "" {
		t.Fatalf("hub's catalog stored by a rejected call: %s", d)
	}
}

func TestRegisterIdempotent(t *testing.T) {
	e := newCatalogEnv(t, storetest.NewDB(t))
	first, err := e.register(otherTok, auditCatalog("audit.events.list", "audit.events.get"))
	if err != nil {
		t.Fatal(err)
	}
	second, err := e.register(otherTok, auditCatalog("audit.events.get", "audit.events.list"))
	if err != nil {
		t.Fatal(err)
	}
	if first != second || storedDigest(t, e.pool, "audit") != first {
		t.Fatalf("digests %s, %s, stored %s", first, second, storedDigest(t, e.pool, "audit"))
	}
	if n := len(updates(t, e.pool, "audit")); n != 1 {
		t.Fatalf("%d catalog update events, want 1", n)
	}
}

func updates(t *testing.T, pool *pgxpool.Pool, service string) []store.AuditEntry {
	t.Helper()
	return slices.DeleteFunc(storetest.Audit(t, pool, store.ActionCatalogUpdate), func(a store.AuditEntry) bool { return a.ActorID != service })
}

func TestCatalogUpdateAudited(t *testing.T) {
	e := newCatalogEnv(t, storetest.NewDB(t))
	if _, err := e.register(otherTok, auditCatalog("audit.events.list", "audit.events.get")); err != nil {
		t.Fatal(err)
	}
	if _, err := e.register(otherTok, auditCatalog("audit.events.list", "audit.settings.update")); err != nil {
		t.Fatal(err)
	}
	ups := updates(t, e.pool, "audit")
	if len(ups) != 2 {
		t.Fatalf("%d update events, want 2", len(ups))
	}
	latest := ups[0]
	if latest.ActorKind != store.ActorService || latest.Result != store.ResultAllowed || latest.OrgID != nil || latest.Resource != "catalog/audit" {
		t.Fatalf("event: %+v", latest)
	}
	if got := latest.Details["added"]; !slices.Equal(anyStrings(got), []string{"audit.settings.update"}) {
		t.Errorf("added %v", got)
	}
	if got := latest.Details["removed"]; !slices.Equal(anyStrings(got), []string{"audit.events.get"}) {
		t.Errorf("removed %v", got)
	}
	if got := ups[1].Details["added"]; !slices.Equal(anyStrings(got), []string{"audit.events.get", "audit.events.list"}) {
		t.Errorf("first registration added %v", got)
	}
}

func anyStrings(v any) []string {
	var out []string
	for _, x := range v.([]any) {
		out = append(out, x.(string))
	}
	return out
}

func TestRegisterStreamsCatalog(t *testing.T) {
	e := newCatalogEnv(t, storetest.NewDB(t))
	stream := e.watch(t)
	own, err := Catalog()
	if err != nil {
		t.Fatal(err)
	}
	waitCatalog(t, stream, "iam", catalog.Digest(own))
	digest, err := e.register(otherTok, auditCatalog("audit.events.list"))
	if err != nil {
		t.Fatal(err)
	}
	waitCatalog(t, stream, "audit", digest)
}

func TestCatalogsSurviveRestart(t *testing.T) {
	pool := storetest.NewDB(t)
	digest, err := newCatalogEnv(t, pool).register(otherTok, auditCatalog("audit.events.list"))
	if err != nil {
		t.Fatal(err)
	}
	restarted := newCatalogEnv(t, pool)
	waitCatalog(t, restarted.watch(t), "audit", digest)
	// iam's own catalog is unchanged by the restart, so it is audited once.
	if n := len(updates(t, pool, "iam")); n != 1 {
		t.Fatalf("%d iam catalog events after a restart, want 1", n)
	}
}

func TestRegisterCatalogBodyLimit(t *testing.T) {
	e := newCatalogEnv(t, storetest.NewDB(t))
	body := bytes.NewReader(append([]byte(`{"catalog":{"service":"audit","actions":[{"name":"`), bytes.Repeat([]byte("a"), maxBody+1)...))
	r, err := http.NewRequest("POST", e.srv.URL+"/internal/iam"+iamv1connect.CatalogInternalServiceRegisterCatalogProcedure, body)
	if err != nil {
		t.Fatal(err)
	}
	r.Header.Set("Authorization", "Bearer "+otherTok)
	r.Header.Set("Content-Type", "application/json")
	resp, err := e.srv.Client().Do(r)
	if err == nil {
		defer resp.Body.Close()
		if resp.StatusCode == http.StatusOK {
			t.Fatal("oversized catalog accepted")
		}
	} else if !strings.Contains(err.Error(), "reset") && !strings.Contains(err.Error(), "broken pipe") {
		t.Fatal(err)
	}
	if d := storedDigest(t, e.pool, "audit"); d != "" {
		t.Fatalf("oversized catalog stored: %s", d)
	}
}

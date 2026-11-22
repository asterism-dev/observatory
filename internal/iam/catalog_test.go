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
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	iamv1 "github.com/asterism-dev/observatory/gen/go/observatory/iam/v1"
	"github.com/asterism-dev/observatory/gen/go/observatory/iam/v1/iamv1connect"
	"github.com/asterism-dev/observatory/internal/authz"
	"github.com/asterism-dev/observatory/internal/authz/authztest"
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
	s    *store.Store
	z    *authz.Service
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
	h := servicetoken.Middleware(map[string]string{"edge": svcToken, "audit": otherTok, "hub": hubTok, "compute": computeTok})(
		internalHandler(iss, ps, internalServer{Store: s}, catalogServer{Store: s, Policy: z}))
	e := &catalogEnv{pool: pool, srv: httptest.NewServer(h), s: s, z: z}
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

	// A change to role defaults only names no action, so the digests show it.
	c := auditCatalog("audit.events.list", "audit.settings.update")
	c.RoleDefaults = []authz.RoleDefault{{Role: "admin", Object: "org/audit", Action: "audit.events.list"}}
	digest, err := e.register(otherTok, c)
	if err != nil {
		t.Fatal(err)
	}
	ev := updates(t, e.pool, "audit")[0]
	if ev.Details["old_digest"] != ups[0].Details["new_digest"] || ev.Details["new_digest"] != digest || ev.Details["old_digest"] == digest {
		t.Fatalf("defaults-only change details: %v", ev.Details)
	}
	if ups[1].Details["old_digest"] != "" {
		t.Errorf("first registration old digest %v", ups[1].Details["old_digest"])
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

func TestOwnCatalogFailureFailsStart(t *testing.T) {
	pool := storetest.NewDB(t)
	s := store.New(pool)
	z, err := authz.NewService(context.Background(), s.PolicySnapshot)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(context.Background(), `DROP TABLE catalogs`); err != nil {
		t.Fatal(err)
	}
	if err := registerOwnCatalog(context.Background(), s, z); err == nil {
		t.Fatal("iam starts without storing its own catalog")
	}
}

func TestInternalBodyLimit(t *testing.T) {
	e := newCatalogEnv(t, storetest.NewDB(t))
	huge := strings.Repeat("a", maxBody)
	// Well-formed bodies, so only the cap explains the answer: uncapped, they would get 401 and invalid user_id (400).
	cases := []struct {
		path, tok, body string
		want            int
	}{
		{"/internal/iam/token", svcToken, `{"session_token":"` + huge + `"}`, http.StatusBadRequest},
		{"/internal/iam" + iamv1connect.IAMInternalServiceGetUsersProcedure, hubTok, `{"user_ids":["` + huge + `"]}`, http.StatusTooManyRequests},
	}
	for _, c := range cases {
		r, err := http.NewRequest("POST", e.srv.URL+c.path, strings.NewReader(c.body))
		if err != nil {
			t.Fatal(err)
		}
		r.Header.Set("Authorization", "Bearer "+c.tok)
		r.Header.Set("Content-Type", "application/json")
		resp, err := e.srv.Client().Do(r)
		if err != nil {
			t.Fatalf("%s: %v", c.path, err)
		}
		resp.Body.Close()
		if resp.StatusCode != c.want {
			t.Errorf("%s with an oversized body: %d, want %d", c.path, resp.StatusCode, c.want)
		}
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

// legacyCatalog is service's 0.x catalog without the actions of services extracted since, as the service registers it today.
func legacyCatalog(service string) authz.Catalog {
	all := authztest.Legacy()
	c := all[slices.IndexFunc(all, func(c authz.Catalog) bool { return c.Service == service })]
	foreign := func(action string) bool {
		prefix, _, _ := strings.Cut(action, ".")
		return !slices.Contains(catalog.AllowedPrefixes(service), prefix)
	}
	c.Actions = slices.DeleteFunc(slices.Clone(c.Actions), func(a authz.Action) bool { return foreign(a.Name) })
	c.RoleDefaults = slices.DeleteFunc(slices.Clone(c.RoleDefaults), func(d authz.RoleDefault) bool { return foreign(d.Action) })
	return c
}

func TestRemovedActionKeptAsUnknown(t *testing.T) {
	ctx := context.Background()
	e := newCatalogEnv(t, storetest.NewDB(t))
	hub := legacyCatalog("hub")
	if _, err := e.register(hubTok, hub); err != nil {
		t.Fatal(err)
	}
	login, err := e.s.Login(ctx, store.Identity{Subject: "bob", Email: "bob@x.io", Name: "bob"})
	if err != nil {
		t.Fatal(err)
	}
	bob := login.User.ID
	org, err := e.s.CreateOrg(ctx, "acme", bob)
	if err != nil {
		t.Fatal(err)
	}
	role, err := e.s.CreateRole(ctx, org.ID, authz.CustomRole{Name: "forker", Rules: []authz.Rule{{Effect: authz.Allow, Object: authz.ObjVMs + "/*", Action: "compute.instances.fork"}}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.s.Bind(ctx, org.ID, authz.UserSubject(bob), role.ID); err != nil {
		t.Fatal(err)
	}
	if err := e.z.Reload(ctx); err != nil {
		t.Fatal(err)
	}

	w, err := platformauthz.NewWatcher(e.srv.URL, hubTok, nil)
	if err != nil {
		t.Fatal(err)
	}
	wctx, cancel := context.WithCancel(ctx)
	t.Cleanup(cancel)
	go w.Run(wctx)
	vm := authz.VMObject(uuid.New())
	hubAllows := func() bool {
		p, _, _ := w.Policy()
		h, err := authz.New(p)
		if err != nil {
			t.Fatal(err)
		}
		return h.Allowed(authz.UserSubject(bob), org.ID, vm, "compute.instances.fork")
	}
	eventually := func(want bool) {
		t.Helper()
		for deadline := time.Now().Add(10 * time.Second); hubAllows() != want; time.Sleep(20 * time.Millisecond) {
			if time.Now().After(deadline) {
				t.Fatalf("hub allows fork: %v, want %v", !want, want)
			}
		}
	}
	eventually(true)

	hub.Actions = slices.DeleteFunc(slices.Clone(hub.Actions), func(a authz.Action) bool { return a.Name == "compute.instances.fork" })
	hub.Templates = nil
	if _, err := e.register(hubTok, hub); err != nil {
		t.Fatal(err)
	}
	eventually(false)
	roles, err := e.s.Roles(ctx, org.ID)
	if err != nil || len(roles) != 1 || !slices.Equal(roles[0].Rules, role.Rules) {
		t.Fatalf("stored rules changed: %+v %v", roles, err)
	}
	res, err := (&server{Deps: Deps{Policy: e.z}}).ListCatalog(ctx, connect.NewRequest(&iamv1.ListCatalogRequest{Org: org.ID.String()}))
	if err != nil {
		t.Fatal(err)
	}
	if got := res.Msg.GetUnknownActions(); !slices.Equal(got, []string{"compute.instances.fork"}) {
		t.Fatalf("unknown actions = %v", got)
	}
	if slices.ContainsFunc(res.Msg.GetEntries(), func(c *iamv1.CatalogEntry) bool { return c.Action == "compute.instances.fork" }) {
		t.Fatal("the removed action is still listed")
	}
}

func TestCreateRoleUnknownActionRejected(t *testing.T) {
	e := newEnv(t)
	_, ownerTok := e.user("alice")
	org := e.createOrg(ownerTok, "Acme")
	_, err := e.tryCreateRole(ownerTok, org, &iamv1.CreateRoleRequest{Name: "x", Rules: []*iamv1.RoleRule{rule("allow", "org/vms/*", "compute.instances.reboot")}})
	wantCode(t, err, connect.CodeInvalidArgument)
	if !strings.Contains(err.Error(), "invalid rule") {
		t.Fatalf("err = %v, want invalid rule", err)
	}
	if e.roleCount(org) != 0 {
		t.Fatal("role with an unknown action stored")
	}
}

func TestListCatalogPermission(t *testing.T) {
	e := newEnv(t)
	_, ownerTok := e.user("alice")
	member, memberTok := e.user("bob")
	_, strangerTok := e.user("carol")
	org := e.createOrg(ownerTok, "Acme")
	e.addMember(org, member, store.RoleMember)
	ctx := context.Background()
	res, err := e.client.ListCatalog(ctx, req(memberTok, &iamv1.ListCatalogRequest{Org: org.String()}))
	if err != nil {
		t.Fatal(err)
	}
	i := slices.IndexFunc(res.Msg.Entries, func(c *iamv1.CatalogEntry) bool { return c.Action == "audit.settings.update" })
	if i < 0 || res.Msg.Entries[i].Service != "audit" || !res.Msg.Entries[i].Write || res.Msg.Entries[i].Object != "org/audit" {
		t.Fatalf("audit.settings.update entry: %+v", res.Msg.Entries)
	}
	if len(res.Msg.UnknownActions) != 0 {
		t.Fatalf("unknown actions: %v", res.Msg.UnknownActions)
	}
	_, err = e.client.ListCatalog(ctx, req(strangerTok, &iamv1.ListCatalogRequest{Org: org.String()}))
	wantCode(t, err, connect.CodeNotFound)
}

func TestOrgPermissionsFromCatalogs(t *testing.T) {
	e := newEnv(t)
	_, ownerTok := e.user("alice")
	member, memberTok := e.user("bob")
	org := e.createOrg(ownerTok, "Acme")
	e.addMember(org, member, store.RoleMember)
	hub := legacyCatalog("hub")
	hub.Actions = append(hub.Actions, authz.Action{Name: "compute.instances.reboot", Object: "org/vms/*", Write: true})
	hub.RoleDefaults = append(hub.RoleDefaults, authz.RoleDefault{Role: "member", Object: "org/vms/*", Action: "compute.instances.reboot"})
	if _, _, err := e.store.PutCatalog(context.Background(), "hub", hub); err != nil {
		t.Fatal(err)
	}
	e.reload()
	got, err := e.getOrg(memberTok, org)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(got.Permissions, "compute.instances.reboot") || slices.Contains(got.Permissions, "compute.instances.delete") {
		t.Fatalf("member permissions = %v", got.Permissions)
	}
}

func TestListCatalogDedupesAcrossCatalogs(t *testing.T) {
	e := newEnv(t)
	_, ownerTok := e.user("alice")
	org := e.createOrg(ownerTok, "Acme")
	// During an extraction the hub and the new service both declare the action.
	compute := authz.Catalog{Service: "compute", Actions: []authz.Action{{Name: "compute.instances.get", Object: "org/vms/*"}}}
	if _, _, err := e.store.PutCatalog(context.Background(), "compute", compute); err != nil {
		t.Fatal(err)
	}
	e.reload()
	res, err := e.client.ListCatalog(context.Background(), req(ownerTok, &iamv1.ListCatalogRequest{Org: org.String()}))
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, c := range res.Msg.Entries {
		if c.Action == "compute.instances.get" && c.Object == "org/vms/*" {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("compute.instances.get@org/vms/* listed %d times", n)
	}
}

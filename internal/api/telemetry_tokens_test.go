package api

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/asterism-dev/observatory/internal/store"
)

func tokensPath(org uuid.UUID) string { return "/api/orgs/" + org.String() + "/telemetry-tokens" }

type telemetryTokenTest struct {
	ID, Name, Token, CreatedBy string
	LastUsedAt                 *time.Time
}

func (e *env) createTelemetryToken(tok string, org uuid.UUID, name string) telemetryTokenTest {
	e.t.Helper()
	rec := e.send("POST", tokensPath(org), tok, `{"name":"`+name+`"}`)
	if rec.Code != 201 {
		e.t.Fatalf("create token: %d %s", rec.Code, rec.Body.String())
	}
	var got telemetryTokenTest
	decode(e.t, rec, &got)
	return got
}

func (e *env) grafana(header string) (uuid.UUID, int) {
	req := httptest.NewRequest("GET", "/grafana/prometheus/api/v1/query", nil)
	if header != "" {
		req.Header.Set("Authorization", header)
	}
	org, _, status := e.api.grafanaAuth(req)
	return org, status
}

func TestTokenCreateNeedsOrgWideAccess(t *testing.T) {
	e := newEnv(t)
	_, ownerTok := e.user("alice")
	bob, bobTok := e.user("bob")
	carol, carolTok := e.user("carol")
	dave, daveTok := e.user("dave")
	org := e.createOrg(ownerTok, "Acme")
	e.addMember(org, bob, store.RoleMember)
	e.addMember(org, carol, store.RoleMember)
	e.addMember(org, dave, store.RoleAdmin)

	for _, rec := range []*httptest.ResponseRecorder{e.do("GET", tokensPath(org), bobTok), e.send("POST", tokensPath(org), bobTok, `{"name":"g"}`)} {
		if rec.Code != 403 {
			t.Fatalf("member: %d", rec.Code)
		}
	}

	role := e.createRole(ownerTok, org, `{"name":"tokens","rules":[{"effect":"allow","object":"org/settings","action":"telemetry.tokens.create"}]}`)
	e.bind(ownerTok, org, "user:"+carol.ID.String(), role)
	if rec := e.do("GET", tokensPath(org), carolTok); rec.Code != 200 {
		t.Fatalf("custom role list: %d", rec.Code)
	}
	if rec := e.send("POST", tokensPath(org), carolTok, `{"name":"g"}`); rec.Code != 403 {
		t.Fatalf("custom role create: %d %s", rec.Code, rec.Body.String())
	}

	deny := e.createRole(ownerTok, org, `{"name":"no-vm","rules":[{"effect":"deny","object":"org/vms/`+uuid.NewString()+`","action":"compute.instances.getMetrics"}]}`)
	e.bind(ownerTok, org, "user:"+dave.ID.String(), deny)
	if rec := e.send("POST", tokensPath(org), daveTok, `{"name":"g"}`); rec.Code != 403 {
		t.Fatalf("admin with deny: %d %s", rec.Code, rec.Body.String())
	}
	es := e.audit(store.ActionGrafanaTokenCreate)
	if len(es) != 2 {
		t.Fatalf("denied audits: %d", len(es))
	}
	for _, a := range es {
		if a.Result != store.ResultDenied {
			t.Fatalf("audit: %+v", a)
		}
	}
}

func TestTokenCreateAdmin(t *testing.T) {
	e := newEnv(t)
	alice, tok := e.user("alice")
	org := e.createOrg(tok, "Acme")

	created := e.createTelemetryToken(tok, org, "grafana-prod")
	if !strings.HasPrefix(created.Token, "obs_grafana_") || created.Name != "grafana-prod" || created.CreatedBy != alice.ID.String() {
		t.Fatalf("created: %+v", created)
	}
	for _, body := range []string{`{"name":""}`, `{"name":"` + strings.Repeat("a", 65) + `"}`, `{}`} {
		if rec := e.send("POST", tokensPath(org), tok, body); rec.Code != 400 {
			t.Fatalf("%s: %d", body, rec.Code)
		}
	}
	if rec := e.send("POST", tokensPath(org), tok, `{"name":"grafana-prod"}`); rec.Code != 409 {
		t.Fatalf("duplicate: %d", rec.Code)
	}

	rec := e.do("GET", tokensPath(org), tok)
	if rec.Code != 200 || strings.Contains(rec.Body.String(), created.Token) || strings.Contains(rec.Body.String(), `"token"`) {
		t.Fatalf("list: %d %s", rec.Code, rec.Body.String())
	}
	var list []telemetryTokenTest
	decode(t, rec, &list)
	if len(list) != 1 || list[0].ID != created.ID || list[0].Name != "grafana-prod" {
		t.Fatalf("list: %+v", list)
	}

	got, status := e.grafana("Bearer " + created.Token)
	if status != http.StatusOK || got != org {
		t.Fatalf("auth: %v %d", got, status)
	}
	decode(t, e.do("GET", tokensPath(org), tok), &list)
	if list[0].LastUsedAt == nil {
		t.Fatal("last used not touched")
	}
	if es := e.audit(store.ActionGrafanaTokenCreate); len(es) != 1 || es[0].Details["name"] != "grafana-prod" || es[0].Resource != "telemetry-token/"+created.ID {
		t.Fatalf("audit: %+v", es)
	}
}

func TestTokenRevoke(t *testing.T) {
	e := newEnv(t)
	_, tok := e.user("alice")
	_, otherTok := e.user("bob")
	org := e.createOrg(tok, "Acme")
	other := e.createOrg(otherTok, "Other")
	created := e.createTelemetryToken(tok, org, "g")
	if _, status := e.grafana("Bearer " + created.Token); status != 200 {
		t.Fatal(status)
	}

	if rec := e.do("DELETE", tokensPath(other)+"/"+created.ID, otherTok); rec.Code != 404 {
		t.Fatalf("other org: %d", rec.Code)
	}
	if rec := e.do("DELETE", tokensPath(org)+"/"+created.ID, tok); rec.Code != 204 {
		t.Fatalf("revoke: %d", rec.Code)
	}
	if rec := e.do("DELETE", tokensPath(org)+"/"+created.ID, tok); rec.Code != 404 {
		t.Fatalf("revoke again: %d", rec.Code)
	}
	if _, status := e.grafana("Bearer " + created.Token); status != 401 {
		t.Fatalf("revoked token: %d", status)
	}
	if rec := e.do("GET", tokensPath(org), tok); rec.Body.String() != "[]\n" {
		t.Fatalf("list: %s", rec.Body.String())
	}
	if es := e.audit(store.ActionGrafanaTokenRevoke); len(es) != 1 || es[0].ActorKind != store.ActorUser || es[0].Details["name"] != "g" {
		t.Fatalf("audit: %+v", es)
	}
	e.createTelemetryToken(tok, org, "g")
}

func TestTokenDiesWithCreatorAccess(t *testing.T) {
	e := newEnv(t)
	_, ownerTok := e.user("alice")
	bob, bobTok := e.user("bob")
	carol, carolTok := e.user("carol")
	dave, daveTok := e.user("dave")
	org := e.createOrg(ownerTok, "Acme")
	e.addMember(org, bob, store.RoleAdmin)
	e.addMember(org, carol, store.RoleAdmin)
	e.addMember(org, dave, store.RoleAdmin)
	now := time.Now()
	e.api.now = func() time.Time { return now }

	demoted := e.createTelemetryToken(bobTok, org, "bob")
	denied := e.createTelemetryToken(carolTok, org, "carol")
	removed := e.createTelemetryToken(daveTok, org, "dave")
	for _, tok := range []string{demoted.Token, denied.Token, removed.Token} {
		if _, status := e.grafana("Bearer " + tok); status != 200 {
			t.Fatal(status)
		}
	}

	if rec := e.send("PATCH", "/api/orgs/"+org.String()+"/members/"+bob.ID.String(), ownerTok, `{"role":"member"}`); rec.Code != 204 {
		t.Fatalf("demote: %d %s", rec.Code, rec.Body.String())
	}
	if rec := e.do("DELETE", "/api/orgs/"+org.String()+"/members/"+dave.ID.String(), ownerTok); rec.Code != 204 {
		t.Fatalf("remove: %d %s", rec.Code, rec.Body.String())
	}
	role := e.createRole(ownerTok, org, `{"name":"no-node","rules":[{"effect":"deny","object":"org/nodes/`+uuid.NewString()+`","action":"compute.nodes.getMetrics"}]}`)
	e.bind(ownerTok, org, "user:"+carol.ID.String(), role)
	if _, status := e.grafana("Bearer " + demoted.Token); status != 200 {
		t.Fatalf("within cache: %d", status)
	}

	now = now.Add(61 * time.Second)
	for _, tok := range []string{demoted.Token, denied.Token, removed.Token} {
		if _, status := e.grafana("Bearer " + tok); status != 401 {
			t.Fatalf("after expiry: %d", status)
		}
		if _, err := e.store.TelemetryTokenBySecret(context.Background(), tok); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("not revoked: %v", err)
		}
	}
	if es := e.audit(store.ActionGrafanaTokenRevoke); len(es) != 3 || es[0].ActorKind != store.ActorSystem {
		t.Fatalf("audit: %+v", es)
	}
}

func TestGrafanaAuthIgnoresCookies(t *testing.T) {
	e := newEnv(t)
	_, tok := e.user("alice")
	org := e.createOrg(tok, "Acme")
	created := e.createTelemetryToken(tok, org, "g")

	req := httptest.NewRequest("GET", "/grafana/prometheus/api/v1/query", nil)
	req.AddCookie(&http.Cookie{Name: "obs_session", Value: tok})
	if _, _, status := e.api.grafanaAuth(req); status != 401 {
		t.Fatalf("cookie: %d", status)
	}
	req.AddCookie(&http.Cookie{Name: "obs_session", Value: created.Token})
	if _, _, status := e.api.grafanaAuth(req); status != 401 {
		t.Fatalf("token cookie: %d", status)
	}
	for _, h := range []string{"", "Bearer " + tok, "Basic " + created.Token, created.Token, "Bearer"} {
		if _, status := e.grafana(h); status != 401 {
			t.Fatalf("%q: %d", h, status)
		}
	}
	if _, status := e.grafana("bearer " + created.Token); status != 200 {
		t.Fatalf("lower-case scheme: %d", status)
	}
}

func TestTokenRevokedWhenCreatorRemoved(t *testing.T) {
	e := newEnv(t)
	_, ownerTok := e.user("alice")
	bob, bobTok := e.user("bob")
	carol, carolTok := e.user("carol")
	org := e.createOrg(ownerTok, "Acme")
	e.addMember(org, bob, store.RoleAdmin)
	e.addMember(org, carol, store.RoleAdmin)
	removed := e.createTelemetryToken(bobTok, org, "bob")
	left := e.createTelemetryToken(carolTok, org, "carol")
	kept := e.createTelemetryToken(ownerTok, org, "alice")

	// Neither token is used before its creator goes, so only the membership change can revoke it.
	if rec := e.do("DELETE", "/api/orgs/"+org.String()+"/members/"+bob.ID.String(), ownerTok); rec.Code != 204 {
		t.Fatalf("remove: %d %s", rec.Code, rec.Body.String())
	}
	if rec := e.do("DELETE", "/api/orgs/"+org.String()+"/members/"+carol.ID.String(), carolTok); rec.Code != 204 {
		t.Fatalf("leave: %d %s", rec.Code, rec.Body.String())
	}
	var list []telemetryTokenTest
	decode(t, e.do("GET", tokensPath(org), ownerTok), &list)
	if len(list) != 1 || list[0].ID != kept.ID {
		t.Fatalf("list: %+v", list)
	}
	es := e.audit(store.ActionGrafanaTokenRevoke)
	if len(es) != 2 {
		t.Fatalf("audit: %+v", es)
	}
	for _, a := range es {
		if a.ActorKind != store.ActorSystem || (a.Details["name"] != "bob" && a.Details["name"] != "carol") {
			t.Fatalf("audit: %+v", a)
		}
	}

	e.addMember(org, bob, store.RoleAdmin)
	e.addMember(org, carol, store.RoleAdmin)
	for _, tok := range []string{removed.Token, left.Token} {
		if _, status := e.grafana("Bearer " + tok); status != 401 {
			t.Fatalf("revived after re-add: %d", status)
		}
	}
}

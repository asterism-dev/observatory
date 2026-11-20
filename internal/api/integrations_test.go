package api

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/asterism-dev/observatory/internal/authz"
	"github.com/asterism-dev/observatory/internal/integrations"
	"github.com/asterism-dev/observatory/internal/store"
)

const (
	dummyGitHub = "ghp_dummy0000000000000000"
	dummyAccess = "sk-ant-oat01-dummyaccess0000"
	dummyRefres = "sk-ant-ort01-dummyrefresh000"
)

type fakeVerifier struct {
	mu   sync.Mutex
	got  []string
	err  error
	name string
}

func (f *fakeVerifier) Verify(_ context.Context, _ integrations.Kind, _ integrations.Source, token string) (integrations.Account, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.got = append(f.got, token)
	if f.err != nil {
		return integrations.Account{}, f.err
	}
	name := f.name
	if name == "" {
		name = "octocat"
	}
	return integrations.Account{Name: name, Scopes: "repo"}, nil
}

type fakeOAuth struct {
	mu        sync.Mutex
	codes     []string
	noRefresh bool
}

func (*fakeOAuth) AuthorizeURL(state, challenge string) string {
	return "https://claude.test/authorize?state=" + state + "&code_challenge=" + challenge
}

func (f *fakeOAuth) Exchange(_ context.Context, code, _, verifier string) (integrations.OAuthTokens, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.codes = append(f.codes, code)
	if verifier == "" {
		return integrations.OAuthTokens{}, integrations.ErrRejected
	}
	refresh := dummyRefres
	if f.noRefresh {
		refresh = ""
	}
	return integrations.OAuthTokens{Access: dummyAccess, Refresh: refresh, ExpiresAt: time.Now().Add(8 * time.Hour), Email: "me@example.com"}, nil
}

func (*fakeOAuth) Refresh(context.Context, string) (integrations.OAuthTokens, error) {
	return integrations.OAuthTokens{}, integrations.ErrUnreachable
}

func integrationsPath(org uuid.UUID) string { return "/api/orgs/" + org.String() + "/integrations" }

type integrationListJSON struct {
	Integrations []struct {
		ID, Kind, Source, Scope, Mode, Account, Status string
		TeamID                                         *uuid.UUID
		TeamName                                       string
		WasInVM                                        bool `json:"wasInVm"`
		CanEdit, CanDelete                             bool
	}
	WritableScopes []struct {
		Scope     string
		TeamID    *uuid.UUID
		Connected []string
	}
}

func (e *env) listIntegrations(tok string, org uuid.UUID) integrationListJSON {
	e.t.Helper()
	rec := e.do("GET", integrationsPath(org), tok)
	if rec.Code != 200 {
		e.t.Fatalf("list integrations: %d %s", rec.Code, rec.Body.String())
	}
	var got integrationListJSON
	decode(e.t, rec, &got)
	return got
}

func (e *env) connect(tok string, org uuid.UUID, body string) string {
	e.t.Helper()
	rec := e.send("POST", integrationsPath(org), tok, body)
	if rec.Code != 201 {
		e.t.Fatalf("connect %s: %d %s", body, rec.Code, rec.Body.String())
	}
	var got struct{ ID string }
	decode(e.t, rec, &got)
	return got.ID
}

func githubBody(scope, extra string) string {
	return `{"kind":"github","source":"pat","scope":"` + scope + `","token":"` + dummyGitHub + `"` + extra + `}`
}

func captureLogs(t *testing.T) *bytes.Buffer {
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return &buf
}

func noSecrets(t *testing.T, what, s string) {
	t.Helper()
	for _, bad := range []string{dummyGitHub, dummyAccess, dummyRefres, "obsph_"} {
		if strings.Contains(s, bad) {
			t.Fatalf("%s contains %q: %s", what, bad, s)
		}
	}
}

func (e *env) allAudit() []store.AuditEntry {
	e.t.Helper()
	es, err := e.store.AuditEntries(context.Background(), store.AuditFilter{Limit: 1000})
	if err != nil {
		e.t.Fatal(err)
	}
	return es
}

func (e *env) noSecretsInAudit() {
	e.t.Helper()
	for _, a := range e.allAudit() {
		noSecrets(e.t, "audit "+a.Action, a.Resource+" "+strings.Join(detailStrings(a.Details), " "))
	}
}

func TestIntegrationsConnectAndList(t *testing.T) {
	logs := captureLogs(t)
	e := newEnv(t)
	_, ownerTok := e.user("alice")
	bob, bobTok := e.user("bob")
	org := e.createOrg(ownerTok, "Acme")
	e.addMember(org, bob, store.RoleMember)

	rec := e.send("POST", integrationsPath(org), bobTok, githubBody("user", ""))
	noSecrets(t, "connect", rec.Body.String())
	if rec.Code != 201 {
		t.Fatalf("connect: %d %s", rec.Code, rec.Body.String())
	}
	var created struct{ ID, Account string }
	decode(t, rec, &created)
	if created.Account != "octocat" {
		t.Fatalf("account: %+v", created)
	}
	rec = e.do("GET", integrationsPath(org), bobTok)
	noSecrets(t, "list", rec.Body.String())
	var list integrationListJSON
	decode(t, rec, &list)
	if len(list.Integrations) != 1 || list.Integrations[0].ID != created.ID || list.Integrations[0].Kind != "github" || list.Integrations[0].Scope != "user" ||
		list.Integrations[0].Mode != "proxy" || list.Integrations[0].Account != "octocat" || !list.Integrations[0].CanEdit || !list.Integrations[0].CanDelete {
		t.Fatalf("list: %s", rec.Body.String())
	}
	var private []string
	for _, s := range list.WritableScopes {
		if s.Scope == "user" {
			private = s.Connected
		}
	}
	if strings.Join(private, ",") != "github" {
		t.Fatalf("writable scopes: %s", rec.Body.String())
	}
	if got := e.listIntegrations(ownerTok, org); len(got.Integrations) != 0 {
		t.Fatalf("owner sees bob's private integration: %+v", got)
	}
	es := e.audit(store.ActionIntegrationSet)
	if len(es) != 1 || es[0].Resource != authz.UserIntegrationsObject(bob.ID) || es[0].Details["account"] != "octocat" || es[0].Details["created"] != true ||
		es[0].Details["kind"] != "github" || es[0].Details["mode"] != "proxy" {
		t.Fatalf("audit: %+v", es)
	}
	e.noSecretsInAudit()
	noSecrets(t, "logs", logs.String())
}

func TestConnectTrimsToken(t *testing.T) {
	e := newEnv(t)
	_, ownerTok := e.user("alice")
	org := e.createOrg(ownerTok, "Acme")
	rec := e.send("POST", integrationsPath(org), ownerTok, `{"kind":"github","source":"pat","scope":"user","token":" `+dummyGitHub+`\n"}`)
	if rec.Code != 201 || len(e.verifier.got) != 1 || e.verifier.got[0] != dummyGitHub {
		t.Fatalf("trim: %d %s %q", rec.Code, rec.Body.String(), e.verifier.got)
	}
	rec = e.send("POST", integrationsPath(org), ownerTok, `{"kind":"linear","source":"api-key","scope":"user","token":"ghp_dummy 000000000000"}`)
	if rec.Code != 400 || len(e.verifier.got) != 1 {
		t.Fatalf("inner space: %d %s %q", rec.Code, rec.Body.String(), e.verifier.got)
	}
	noSecrets(t, "error", rec.Body.String())
	for _, body := range []string{
		`{"kind":"nope","source":"pat","scope":"user","token":"` + dummyGitHub + `"}`,
		`{"kind":"github","source":"api-key","scope":"user","token":"` + dummyGitHub + `"}`,
		`{"kind":"claude","source":"oauth","scope":"user","token":"` + dummyAccess + `"}`,
		`{"kind":"linear","source":"api-key","scope":"user","mode":"both","token":"` + dummyGitHub + `"}`,
	} {
		if rec := e.send("POST", integrationsPath(org), ownerTok, body); rec.Code != 400 {
			t.Fatalf("%s: %d %s", body, rec.Code, rec.Body.String())
		}
	}
}

func TestIntegrationsVerifyFailures(t *testing.T) {
	e := newEnv(t)
	_, ownerTok := e.user("alice")
	org := e.createOrg(ownerTok, "Acme")
	for _, c := range []struct {
		err  error
		code int
		msg  string
	}{
		{integrations.ErrRejected, 400, "verification failed: provider rejected the token"},
		{integrations.ErrUnreachable, 502, "provider unreachable"},
	} {
		e.verifier.err = c.err
		rec := e.send("POST", integrationsPath(org), ownerTok, githubBody("user", ""))
		var got struct{ Error string }
		decode(t, rec, &got)
		if rec.Code != c.code || got.Error != c.msg {
			t.Fatalf("%v: %d %s", c.err, rec.Code, rec.Body.String())
		}
	}
	e.verifier.err = nil
	rec := e.send("POST", integrationsPath(org), ownerTok, githubBody("user", `,"dryRun":true`))
	var got struct{ Account, ID string }
	decode(t, rec, &got)
	if rec.Code != 200 || got.Account != "octocat" || got.ID != "" {
		t.Fatalf("dry run: %d %s", rec.Code, rec.Body.String())
	}
	if l := e.listIntegrations(ownerTok, org); len(l.Integrations) != 0 {
		t.Fatalf("stored: %+v", l)
	}
	if es := e.audit(store.ActionIntegrationSet); len(es) != 0 {
		t.Fatalf("audited: %+v", es)
	}
}

func TestIntegrationsPermissions(t *testing.T) {
	e := newEnv(t)
	_, ownerTok := e.user("alice")
	bob, bobTok := e.user("bob")
	admin, adminTok := e.user("adam")
	carol, carolTok := e.user("carol")
	org := e.createOrg(ownerTok, "Acme")
	e.addMember(org, bob, store.RoleMember)
	e.addMember(org, admin, store.RoleAdmin)
	e.addMember(org, carol, store.RoleMember)
	team := e.createTeam(ownerTok, org, "core")
	if rec := e.send("PUT", teamMemberPath(org, team, bob), ownerTok, `{"admin":false}`); rec.Code != 204 {
		t.Fatalf("team member: %d %s", rec.Code, rec.Body.String())
	}

	if rec := e.send("POST", integrationsPath(org), bobTok, githubBody("org", "")); rec.Code != 403 {
		t.Fatalf("member org: %d", rec.Code)
	}
	e.connect(adminTok, org, githubBody("org", ""))
	if rec := e.send("POST", integrationsPath(org), bobTok, githubBody("team", `,"teamId":"`+team.String()+`"`)); rec.Code != 403 {
		t.Fatalf("team member team: %d", rec.Code)
	}
	if rec := e.send("POST", integrationsPath(org), adminTok, githubBody("org", "")); rec.Code != 409 {
		t.Fatalf("second github: %d %s", rec.Code, rec.Body.String())
	}
	carolID := e.connect(carolTok, org, githubBody("user", ""))

	role := e.createRole(ownerTok, org, `{"name":"no private","rules":[{"effect":"deny","object":"org/user-integrations/*","action":"integration:set"}]}`)
	e.bind(ownerTok, org, authz.UserSubject(bob.ID), role)
	if rec := e.send("POST", integrationsPath(org), bobTok, githubBody("user", "")); rec.Code != 403 {
		t.Fatalf("deny private: %d", rec.Code)
	}
	for _, c := range []struct{ method, path, body string }{
		{"PATCH", integrationsPath(org) + "/" + carolID, `{"mode":"vm"}`},
		{"PUT", integrationsPath(org) + "/" + carolID + "/token", `{"token":"` + dummyGitHub + `"}`},
		{"DELETE", integrationsPath(org) + "/" + carolID, ""},
	} {
		for _, tok := range []string{bobTok, ownerTok} {
			if rec := e.send(c.method, c.path, tok, c.body); rec.Code != 404 {
				t.Fatalf("%s other's private: %d", c.method, rec.Code)
			}
		}
	}
	if rec := e.do("DELETE", integrationsPath(org)+"/"+carolID, carolTok); rec.Code != 204 {
		t.Fatalf("delete own: %d", rec.Code)
	}
	if es := e.audit(store.ActionIntegrationDelete); len(es) != 1 || es[0].Result != store.ResultAllowed || es[0].Details["kind"] != "github" {
		t.Fatalf("delete audit: %+v", es)
	}
}

func TestIntegrationReplaceAndMode(t *testing.T) {
	e := newEnv(t)
	_, ownerTok := e.user("alice")
	org := e.createOrg(ownerTok, "Acme")
	id := e.connect(ownerTok, org, githubBody("user", ""))
	path := integrationsPath(org) + "/" + id

	e.verifier.name = "hubot"
	rec := e.send("PUT", path+"/token", ownerTok, `{"token":"ghp_dummy1111111111111111"}`)
	var got struct{ Account string }
	decode(t, rec, &got)
	if rec.Code != 200 || got.Account != "hubot" {
		t.Fatalf("replace: %d %s", rec.Code, rec.Body.String())
	}
	if es := e.audit(store.ActionIntegrationSet); len(es) != 2 || es[0].Details["replaced"] != true || es[0].Details["account"] != "hubot" {
		t.Fatalf("replace audit: %+v", es)
	}
	if rec := e.send("PATCH", path, ownerTok, `{"mode":"vm"}`); rec.Code != 204 {
		t.Fatalf("mode: %d %s", rec.Code, rec.Body.String())
	}
	l := e.listIntegrations(ownerTok, org)
	if len(l.Integrations) != 1 || l.Integrations[0].Mode != "vm" || !l.Integrations[0].WasInVM || l.Integrations[0].Account != "hubot" {
		t.Fatalf("after mode: %+v", l)
	}
	if es := e.audit(store.ActionIntegrationSet); len(es) != 3 || es[0].Details["mode"] != "vm" || es[0].Details["previousMode"] != "proxy" {
		t.Fatalf("mode audit: %+v", es)
	}
	if rec := e.send("PATCH", path, ownerTok, `{"mode":"nope"}`); rec.Code != 400 {
		t.Fatalf("invalid mode: %d", rec.Code)
	}
	e.noSecretsInAudit()
}

func TestOAuthStateBoundToUser(t *testing.T) {
	e := newEnv(t)
	_, aliceTok := e.user("alice")
	bob, bobTok := e.user("bob")
	org := e.createOrg(aliceTok, "Acme")
	other := e.createOrg(aliceTok, "Other")
	e.addMember(org, bob, store.RoleMember)
	now := time.Now()
	e.api.signIns = integrations.NewSignIns[signIn](10*time.Minute, func() time.Time { return now })

	start := func(scope string) string {
		rec := e.send("POST", integrationsPath(org)+"/claude/sign-in", aliceTok, `{"scope":"`+scope+`"}`)
		var got struct{ URL, State string }
		decode(t, rec, &got)
		if rec.Code != 200 || got.State == "" || !strings.Contains(got.URL, got.State) {
			t.Fatalf("start: %d %s", rec.Code, rec.Body.String())
		}
		return got.State
	}
	finish := func(o uuid.UUID, tok, state, code string) int {
		rec := e.send("POST", integrationsPath(o)+"/claude/sign-in/finish", tok, `{"state":"`+state+`","code":"`+code+`"}`)
		noSecrets(t, "finish", rec.Body.String())
		return rec.Code
	}
	state := start("user")
	if c := finish(org, bobTok, state, "code1"); c != 400 {
		t.Fatalf("other user: %d", c)
	}
	if c := finish(other, aliceTok, state, "code1"); c != 400 {
		t.Fatalf("other org: %d", c)
	}
	if c := finish(org, aliceTok, state, "code1#wrongstate"); c != 400 {
		t.Fatalf("state mismatch: %d", c)
	}
	rec := e.send("POST", integrationsPath(org)+"/claude/sign-in/finish", aliceTok, `{"state":"`+state+`","code":"code1#`+state+`"}`)
	var got struct{ ID, Account string }
	decode(t, rec, &got)
	if rec.Code != 201 || got.Account != "me@example.com" {
		t.Fatalf("finish: %d %s", rec.Code, rec.Body.String())
	}
	if strings.Join(e.oauth.codes, ",") != "code1" {
		t.Fatalf("exchange codes: %q", e.oauth.codes)
	}
	if c := finish(org, aliceTok, state, "code1"); c != 400 {
		t.Fatalf("reused: %d", c)
	}
	l := e.listIntegrations(aliceTok, org)
	if len(l.Integrations) != 1 || l.Integrations[0].Kind != "claude" || l.Integrations[0].Source != "oauth" || l.Integrations[0].Account != "me@example.com" {
		t.Fatalf("list: %+v", l)
	}

	state = start("org")
	now = now.Add(11 * time.Minute)
	if c := finish(org, aliceTok, state, "code2"); c != 400 {
		t.Fatalf("expired: %d", c)
	}

	rec = e.send("POST", integrationsPath(org)+"/claude/sign-in", aliceTok, `{"scope":"user","replaceId":"`+got.ID+`"}`)
	var st struct{ State string }
	decode(t, rec, &st)
	if c := finish(org, aliceTok, st.State, "code3"); c != 200 {
		t.Fatalf("replace finish: %d", c)
	}
	if es := e.audit(store.ActionIntegrationSet); len(es) != 2 || es[0].Details["replaced"] != true || es[0].Details["source"] != "oauth" {
		t.Fatalf("audit: %+v", es)
	}
	e.noSecretsInAudit()
}

func TestVMIntegrationsLine(t *testing.T) {
	e := newEnv(t)
	_, ownerTok := e.user("alice")
	admin, adminTok := e.user("adam")
	bob, bobTok := e.user("bob")
	org := e.createOrg(ownerTok, "Acme")
	e.addMember(org, admin, store.RoleAdmin)
	e.addMember(org, bob, store.RoleMember)
	node := e.enrolledNode(ownerTok, org, "n1")
	vm := e.createVM(ownerTok, org, vmBody("v", userOwner(), node))
	e.connect(ownerTok, org, githubBody("user", ""))
	path := vmsPath(org) + "/" + vm.ID + "/integrations"

	get := func(tok string) string {
		rec := e.do("GET", path, tok)
		noSecrets(t, "vm integrations", rec.Body.String())
		if rec.Code != 200 {
			t.Fatalf("get: %d %s", rec.Code, rec.Body.String())
		}
		return strings.TrimSpace(rec.Body.String())
	}
	if got := get(ownerTok); got != `{"integrations":[{"kind":"github","scope":"user","mode":"proxy"}]}` {
		t.Fatalf("owner: %s", got)
	}
	if got := get(adminTok); got != `{"integrations":[{"kind":"github"}]}` {
		t.Fatalf("admin: %s", got)
	}
	if rec := e.do("GET", path, bobTok); rec.Code != 403 {
		t.Fatalf("no vm:read: %d", rec.Code)
	}
}

func TestVMEgressInfoCountsIntegrations(t *testing.T) {
	e := newEnv(t)
	_, ownerTok := e.user("alice")
	org := e.createOrg(ownerTok, "Acme")
	node := e.enrolledNode(ownerTok, org, "n1")
	vm := e.createVM(ownerTok, org, vmBody("v", userOwner(), node))
	id := e.connect(ownerTok, org, githubBody("user", ""))
	type info struct {
		Secrets int
		Hosts   []string
	}
	get := func() info {
		rec := e.do("GET", vmsPath(org)+"/"+vm.ID+"/egress", ownerTok)
		noSecrets(t, "egress", rec.Body.String())
		var got info
		decode(t, rec, &got)
		return got
	}
	if got := get(); got.Secrets != 1 || strings.Join(got.Hosts, ",") != "api.github.com,github.com,uploads.github.com" {
		t.Fatalf("proxy: %+v", got)
	}
	if rec := e.send("PATCH", integrationsPath(org)+"/"+id, ownerTok, `{"mode":"vm"}`); rec.Code != 204 {
		t.Fatalf("mode: %d", rec.Code)
	}
	if got := get(); got.Secrets != 0 {
		t.Fatalf("vm mode: %+v", got)
	}
}

func TestSignInStartAndFinishGuards(t *testing.T) {
	e := newEnv(t)
	_, aliceTok := e.user("alice")
	bob, bobTok := e.user("bob")
	org := e.createOrg(aliceTok, "Acme")
	e.addMember(org, bob, store.RoleMember)
	path := integrationsPath(org) + "/claude/sign-in"
	start := func(tok, body string) (int, string) {
		rec := e.send("POST", path, tok, body)
		var got struct{ State string }
		decode(t, rec, &got)
		return rec.Code, got.State
	}

	e.oauth.noRefresh = true
	_, state := start(aliceTok, `{"scope":"user"}`)
	rec := e.send("POST", path+"/finish", aliceTok, `{"state":"`+state+`","code":"c"}`)
	if rec.Code != 502 || len(e.listIntegrations(aliceTok, org).Integrations) != 0 || len(e.audit(store.ActionIntegrationSet)) != 0 {
		t.Fatalf("no refresh: %d %s", rec.Code, rec.Body.String())
	}
	noSecrets(t, "no refresh", rec.Body.String())
	e.oauth.noRefresh = false

	e.connect(aliceTok, org, `{"kind":"claude","source":"setup-token","scope":"user","token":"`+dummyAccess+`"}`)
	if c, _ := start(aliceTok, `{"scope":"user"}`); c != 409 {
		t.Fatalf("already connected: %d", c)
	}
	if c, _ := start(aliceTok, `{"scope":"org"}`); c != 200 {
		t.Fatalf("org scope free: %d", c)
	}

	orgGitHub := e.connect(aliceTok, org, githubBody("org", ""))
	if c, _ := start(aliceTok, `{"replaceId":"`+orgGitHub+`"}`); c != 400 {
		t.Fatalf("replace non-claude: %d", c)
	}
	if c, _ := start(bobTok, `{"replaceId":"`+orgGitHub+`"}`); c != 403 {
		t.Fatalf("replace non-claude without rights: %d", c)
	}
}

func claudeSetupBody(scope string) string {
	return `{"kind":"claude","source":"setup-token","scope":"` + scope + `","token":"` + dummyAccess + `"}`
}

type accountJSON struct {
	ID, Account, Source, Status string
	Position                    int
	Active                      bool
}

type claudeListJSON struct {
	Integrations []struct {
		ID         string
		Accounts   []accountJSON
		LastSwitch *struct{ FromID, ToID, From, To string }
	}
}

func (e *env) claudeAccounts(tok string, org uuid.UUID) ([]accountJSON, string) {
	e.t.Helper()
	rec := e.do("GET", integrationsPath(org), tok)
	if rec.Code != 200 {
		e.t.Fatalf("list: %d %s", rec.Code, rec.Body.String())
	}
	var got claudeListJSON
	decode(e.t, rec, &got)
	if len(got.Integrations) != 1 {
		e.t.Fatalf("list: %s", rec.Body.String())
	}
	return got.Integrations[0].Accounts, rec.Body.String()
}

func (e *env) addAccount(tok string, org uuid.UUID, id, name string) string {
	e.t.Helper()
	e.verifier.name = name
	rec := e.send("POST", integrationsPath(org)+"/"+id+"/accounts", tok, `{"source":"setup-token","token":"`+dummyAccess+`"}`)
	noSecrets(e.t, "add account", rec.Body.String())
	if rec.Code != 201 {
		e.t.Fatalf("add account: %d %s", rec.Code, rec.Body.String())
	}
	var got struct{ ID string }
	decode(e.t, rec, &got)
	return got.ID
}

func TestAddAccountOrgScopeRefused(t *testing.T) {
	e := newEnv(t)
	_, tok := e.user("alice")
	org := e.createOrg(tok, "Acme")
	id := e.connect(tok, org, claudeSetupBody("org"))
	rec := e.send("POST", integrationsPath(org)+"/"+id+"/accounts", tok, `{"source":"setup-token","token":"`+dummyAccess+`"}`)
	var got struct{ Error string }
	decode(t, rec, &got)
	if rec.Code != 409 || got.Error != store.ErrMultiAccountNotAllowed.Error() {
		t.Fatalf("org scope: %d %s", rec.Code, rec.Body.String())
	}
	gh := e.connect(tok, org, githubBody("user", ""))
	if rec := e.send("POST", integrationsPath(org)+"/"+gh+"/accounts", tok, `{"source":"setup-token","token":"`+dummyAccess+`"}`); rec.Code != 400 {
		t.Fatalf("github: %d %s", rec.Code, rec.Body.String())
	}
}

func TestAddAccountAuditsNoToken(t *testing.T) {
	logs := captureLogs(t)
	e := newEnv(t)
	_, tok := e.user("alice")
	org := e.createOrg(tok, "Acme")
	e.verifier.name = "first"
	id := e.connect(tok, org, claudeSetupBody("user"))
	if rec := e.send("POST", integrationsPath(org)+"/"+id+"/accounts", tok, `{"source":"oauth","token":"`+dummyAccess+`"}`); rec.Code != 400 {
		t.Fatalf("oauth paste: %d", rec.Code)
	}
	acc := e.addAccount(tok, org, id, "second")
	es := e.audit(store.ActionIntegrationSet)
	if len(es) != 2 || es[0].Details["action"] != "account-add" || es[0].Details["account"] != "second" || es[0].Details["accountId"] != acc {
		t.Fatalf("audit: %+v", es)
	}
	e.verifier.name = "second"
	if rec := e.send("POST", integrationsPath(org)+"/"+id+"/accounts", tok, `{"source":"setup-token","token":"`+dummyAccess+`"}`); rec.Code != 409 {
		t.Fatalf("same email: %d", rec.Code)
	}
	e.verifier.name = "renamed"
	rec := e.send("PUT", integrationsPath(org)+"/"+id+"/accounts/"+acc+"/token", tok, `{"source":"setup-token","token":"`+dummyAccess+`"}`)
	if rec.Code != 200 {
		t.Fatalf("replace: %d %s", rec.Code, rec.Body.String())
	}
	if es := e.audit(store.ActionIntegrationSet); es[0].Details["action"] != "account-replace" || es[0].Details["account"] != "renamed" {
		t.Fatalf("replace audit: %+v", es[0])
	}
	e.noSecretsInAudit()
	noSecrets(t, "logs", logs.String())
}

func TestListShowsAccountsWithoutSecrets(t *testing.T) {
	e := newEnv(t)
	_, tok := e.user("alice")
	org := e.createOrg(tok, "Acme")
	e.verifier.name = "first"
	id := e.connect(tok, org, claudeSetupBody("user"))
	acc := e.addAccount(tok, org, id, "second")
	list, body := e.claudeAccounts(tok, org)
	noSecrets(t, "list", body)
	if len(list) != 2 || list[0].ID != id || !list[0].Active || list[0].Account != "first" || list[0].Position != 0 ||
		list[1].ID != acc || list[1].Active || list[1].Account != "second" || list[1].Source != "setup-token" || list[1].Status != "ok" {
		t.Fatalf("accounts: %s", body)
	}
	if !strings.Contains(body, `"lastSwitch":null`) || !strings.Contains(body, `"usage":{"fiveHourPct":null`) {
		t.Fatalf("shape: %s", body)
	}
	until := time.Now().Add(time.Hour)
	if _, err := e.store.RecordUsage(context.Background(), store.UsageObservation{Account: uuid.MustParse(id), LimitedUntil: &until,
		Source: "proxy", ObservedAt: time.Now()}, time.Now()); err != nil {
		t.Fatal(err)
	}
	rec := e.do("GET", integrationsPath(org), tok)
	var got claudeListJSON
	decode(t, rec, &got)
	ls := got.Integrations[0].LastSwitch
	if ls == nil || ls.FromID != id || ls.From != "first" || ls.ToID != acc || ls.To != "second" {
		t.Fatalf("last switch: %s", rec.Body.String())
	}
}

func TestActivateAndOrder(t *testing.T) {
	e := newEnv(t)
	_, tok := e.user("alice")
	org := e.createOrg(tok, "Acme")
	e.verifier.name = "first"
	id := e.connect(tok, org, claudeSetupBody("user"))
	acc := e.addAccount(tok, org, id, "second")
	path := integrationsPath(org) + "/" + id + "/accounts"
	if rec := e.do("POST", path+"/"+acc+"/activate", tok); rec.Code != 204 {
		t.Fatalf("activate: %d %s", rec.Code, rec.Body.String())
	}
	if list, _ := e.claudeAccounts(tok, org); list[0].Active || !list[1].Active {
		t.Fatalf("after activate: %+v", list)
	}
	if es := e.audit(store.ActionIntegrationSet); es[0].Details["action"] != "account-activate" || es[0].Details["account"] != "second" {
		t.Fatalf("activate audit: %+v", es[0])
	}
	if rec := e.send("PUT", path+"/order", tok, `{"ids":["`+acc+`"]}`); rec.Code != 400 {
		t.Fatalf("incomplete order: %d", rec.Code)
	}
	if rec := e.send("PUT", path+"/order", tok, `{"ids":["`+acc+`","`+id+`"]}`); rec.Code != 204 {
		t.Fatalf("order: %d %s", rec.Code, rec.Body.String())
	}
	if list, _ := e.claudeAccounts(tok, org); list[0].ID != acc || list[1].ID != id {
		t.Fatalf("after order: %+v", list)
	}
	if es := e.audit(store.ActionIntegrationSet); es[0].Details["action"] != "account-order" {
		t.Fatalf("order audit: %+v", es[0])
	}
	if rec := e.do("DELETE", path+"/"+acc, tok); rec.Code != 204 {
		t.Fatalf("remove active: %d", rec.Code)
	}
	if list, _ := e.claudeAccounts(tok, org); len(list) != 1 || list[0].ID != id || !list[0].Active {
		t.Fatalf("after remove: %+v", list)
	}
	if es := e.audit(store.ActionIntegrationSet); es[0].Details["action"] != "account-remove" || es[0].Details["account"] != "second" {
		t.Fatalf("remove audit: %+v", es[0])
	}
	if rec := e.do("DELETE", path+"/"+id, tok); rec.Code != 204 {
		t.Fatalf("remove last: %d", rec.Code)
	}
	if l := e.listIntegrations(tok, org); len(l.Integrations) != 0 {
		t.Fatalf("integration kept: %+v", l)
	}
	if es := e.audit(store.ActionIntegrationDelete); len(es) != 1 || es[0].Details["action"] != "account-remove" || es[0].Details["account"] != "first" {
		t.Fatalf("last remove audit: %+v", es)
	}
}

func TestOtherMemberCannotTouchAccounts(t *testing.T) {
	e := newEnv(t)
	_, aliceTok := e.user("alice")
	bob, bobTok := e.user("bob")
	org := e.createOrg(aliceTok, "Acme")
	e.addMember(org, bob, store.RoleMember)
	e.verifier.name = "first"
	id := e.connect(aliceTok, org, claudeSetupBody("user"))
	acc := e.addAccount(aliceTok, org, id, "second")
	bobID := e.connect(bobTok, org, claudeSetupBody("user"))
	path := integrationsPath(org) + "/" + id + "/accounts"
	for _, c := range []struct{ method, path, body string }{
		{"POST", path, `{"source":"setup-token","token":"` + dummyAccess + `"}`},
		{"PUT", path + "/" + acc + "/token", `{"source":"setup-token","token":"` + dummyAccess + `"}`},
		{"POST", path + "/" + acc + "/activate", ""},
		{"PUT", path + "/order", `{"ids":["` + acc + `","` + id + `"]}`},
		{"DELETE", path + "/" + acc, ""},
		// An account of alice's integration addressed through bob's own one.
		{"POST", integrationsPath(org) + "/" + bobID + "/accounts/" + acc + "/activate", ""},
		{"DELETE", integrationsPath(org) + "/" + bobID + "/accounts/" + acc, ""},
	} {
		if rec := e.send(c.method, c.path, bobTok, c.body); rec.Code != 404 {
			t.Fatalf("%s %s: %d", c.method, c.path, rec.Code)
		}
	}
	for _, body := range []string{`{"addToId":"` + id + `"}`, `{"replaceAccountId":"` + acc + `"}`} {
		if rec := e.send("POST", integrationsPath(org)+"/claude/sign-in", bobTok, body); rec.Code != 404 {
			t.Fatalf("sign-in %s: %d", body, rec.Code)
		}
	}
	if list, _ := e.claudeAccounts(aliceTok, org); len(list) != 2 || !list[0].Active {
		t.Fatalf("changed: %+v", list)
	}
}

func TestModePatchRefusedWithTwoAccounts(t *testing.T) {
	e := newEnv(t)
	_, tok := e.user("alice")
	org := e.createOrg(tok, "Acme")
	e.verifier.name = "first"
	id := e.connect(tok, org, claudeSetupBody("user"))
	e.addAccount(tok, org, id, "second")
	rec := e.send("PATCH", integrationsPath(org)+"/"+id, tok, `{"mode":"vm"}`)
	var got struct{ Error string }
	decode(t, rec, &got)
	if rec.Code != 409 || got.Error != store.ErrMultiAccountNotAllowed.Error() {
		t.Fatalf("mode: %d %s", rec.Code, rec.Body.String())
	}
}

func TestAccountSignIn(t *testing.T) {
	e := newEnv(t)
	_, tok := e.user("alice")
	org := e.createOrg(tok, "Acme")
	e.verifier.name = "first"
	id := e.connect(tok, org, claudeSetupBody("user"))
	path := integrationsPath(org) + "/claude/sign-in"
	start := func(body string) (int, string) {
		rec := e.send("POST", path, tok, body)
		var got struct{ State string }
		decode(t, rec, &got)
		return rec.Code, got.State
	}
	finish := func(state string) (int, string) {
		rec := e.send("POST", path+"/finish", tok, `{"state":"`+state+`","code":"c"}`)
		noSecrets(t, "finish", rec.Body.String())
		var got struct{ ID string }
		decode(t, rec, &got)
		return rec.Code, got.ID
	}
	if c, _ := start(`{"addToId":"` + id + `","replaceId":"` + id + `"}`); c != 400 {
		t.Fatalf("both: %d", c)
	}
	c, state := start(`{"addToId":"` + id + `"}`)
	if c != 200 {
		t.Fatalf("add start: %d", c)
	}
	c, acc := finish(state)
	if c != 201 || acc == "" || acc == id {
		t.Fatalf("add finish: %d %s", c, acc)
	}
	list, _ := e.claudeAccounts(tok, org)
	if len(list) != 2 || list[1].ID != acc || list[1].Source != "oauth" || list[1].Account != "me@example.com" || list[1].Active {
		t.Fatalf("after add: %+v", list)
	}
	if es := e.audit(store.ActionIntegrationSet); es[0].Details["action"] != "account-add" || es[0].Details["account"] != "me@example.com" {
		t.Fatalf("add audit: %+v", es[0])
	}
	c, state = start(`{"replaceAccountId":"` + id + `"}`)
	if c != 200 {
		t.Fatalf("replace start: %d", c)
	}
	// The first account would take the email the added one already has.
	if c, _ := finish(state); c != 409 {
		t.Fatalf("replace duplicate email: %d", c)
	}
	if rec := e.do("DELETE", integrationsPath(org)+"/"+id+"/accounts/"+acc, tok); rec.Code != 204 {
		t.Fatalf("remove: %d", rec.Code)
	}
	_, state = start(`{"replaceAccountId":"` + id + `"}`)
	if c, _ := finish(state); c != 200 {
		t.Fatalf("replace finish: %d", c)
	}
	if list, _ := e.claudeAccounts(tok, org); len(list) != 1 || list[0].Source != "oauth" || list[0].Account != "me@example.com" {
		t.Fatalf("after replace: %+v", list)
	}
	if es := e.audit(store.ActionIntegrationSet); es[0].Details["action"] != "account-replace" {
		t.Fatalf("replace audit: %+v", es[0])
	}
	orgID := e.connect(tok, org, claudeSetupBody("org"))
	if c, _ := start(`{"addToId":"` + orgID + `"}`); c != 409 {
		t.Fatalf("add to org: %d", c)
	}
	e.noSecretsInAudit()
}

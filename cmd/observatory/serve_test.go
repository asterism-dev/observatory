package main

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"
	"github.com/jackc/pgx/v5/pgxpool"

	auditv1 "github.com/asterism-dev/observatory/gen/go/observatory/audit/v1"
	"github.com/asterism-dev/observatory/gen/go/observatory/audit/v1/auditv1connect"
	eventsv1 "github.com/asterism-dev/observatory/gen/go/observatory/events/v1"
	iamv1 "github.com/asterism-dev/observatory/gen/go/observatory/iam/v1"
	"github.com/asterism-dev/observatory/gen/go/observatory/iam/v1/iamv1connect"
	"github.com/asterism-dev/observatory/internal/db/dbtest"
	"github.com/asterism-dev/observatory/internal/edge"
	"github.com/asterism-dev/observatory/internal/seal"
	"github.com/asterism-dev/observatory/internal/store"
	"github.com/asterism-dev/observatory/internal/store/storetest"
)

func TestServeRejectsUnknownService(t *testing.T) {
	for _, args := range [][]string{{"serve", "nope"}, {"serve"}, {"serve", "hub", "edge"}} {
		c, _, stderr := newTestCLI(t, nil)
		if code := run(c, args); code != 2 || !strings.Contains(stderr.String(), "usage: observatory serve") {
			t.Errorf("%v: exit %d, stderr %q", args, code, stderr.String())
		}
	}
}

func TestServeEdgeRequiresConfig(t *testing.T) {
	c, _, stderr := newTestCLI(t, nil)
	c.getenv = func(k string) string { return map[string]string{"OBSERVATORY_EDGE_LISTEN": "127.0.0.1:0"}[k] }
	if code := run(c, []string{"serve", "edge"}); code != 1 ||
		!strings.Contains(stderr.String(), "OBSERVATORY_EDGE_TOKEN") || !strings.Contains(stderr.String(), "OBSERVATORY_EDGE_HUB_URL") ||
		!strings.Contains(stderr.String(), "OBSERVATORY_SERVICE_TOKEN") {
		t.Fatalf("exit %d, stderr %q", code, stderr.String())
	}
}

func TestServeAuditRequiresConfig(t *testing.T) {
	c, _, stderr := newTestCLI(t, nil)
	c.getenv = func(k string) string { return map[string]string{"OBSERVATORY_AUDIT_LISTEN": "127.0.0.1:0"}[k] }
	code := run(c, []string{"serve", "audit"})
	for _, k := range []string{"OBSERVATORY_DATABASE_URL", "OBSERVATORY_HUB_URL", "OBSERVATORY_PUBLIC_URL", "OBSERVATORY_SERVICE_TOKEN", "OBSERVATORY_NATS_URL", "OBSERVATORY_NATS_TOKEN"} {
		if code != 1 || !strings.Contains(stderr.String(), k) {
			t.Fatalf("exit %d, stderr %q; want %s reported missing", code, stderr.String(), k)
		}
	}
}

func TestServeAuditRejectsBadServiceTokens(t *testing.T) {
	c, _, stderr := newTestCLI(t, nil)
	env := map[string]string{"OBSERVATORY_AUDIT_LISTEN": "127.0.0.1:0", "OBSERVATORY_DATABASE_URL": "postgres://x", "OBSERVATORY_HUB_URL": "http://hub",
		"OBSERVATORY_PUBLIC_URL": "https://obs", "OBSERVATORY_SERVICE_TOKEN": strings.Repeat("s", 32), "OBSERVATORY_NATS_URL": "nats://x",
		"OBSERVATORY_NATS_TOKEN": strings.Repeat("n", 32), "OBSERVATORY_SERVICE_TOKENS": "compute=short"}
	c.getenv = func(k string) string { return env[k] }
	if code := run(c, []string{"serve", "audit"}); code != 1 || !strings.Contains(stderr.String(), "OBSERVATORY_SERVICE_TOKENS") {
		t.Fatalf("exit %d, stderr %q", code, stderr.String())
	}
}

func TestServeAllRequiresPersistentBus(t *testing.T) {
	c, _, stderr := newTestCLI(t, nil)
	c.getenv = func(k string) string { return map[string]string{"OBSERVATORY_EDGE_LISTEN": "127.0.0.1:0"}[k] }
	if code := run(c, []string{"serve", "all"}); code != 1 || !strings.Contains(stderr.String(), "OBSERVATORY_DATA_DIR") || !strings.Contains(stderr.String(), "OBSERVATORY_NATS_URL") {
		t.Fatalf("exit %d, stderr %q", code, stderr.String())
	}
}

func TestServeEdgeRejectsBadServices(t *testing.T) {
	c, _, stderr := newTestCLI(t, nil)
	c.getenv = func(k string) string {
		return map[string]string{"OBSERVATORY_EDGE_LISTEN": "127.0.0.1:0", "OBSERVATORY_EDGE_TOKEN": strings.Repeat("t", 32),
			"OBSERVATORY_EDGE_HUB_URL": "http://hub", "OBSERVATORY_SERVICE_TOKEN": strings.Repeat("s", 32), "OBSERVATORY_EDGE_SERVICES": "audit"}[k]
	}
	if code := run(c, []string{"serve", "edge"}); code != 1 || !strings.Contains(stderr.String(), "OBSERVATORY_EDGE_SERVICES") {
		t.Fatalf("exit %d, stderr %q", code, stderr.String())
	}
}

func TestServeRejectsOldListenVariable(t *testing.T) {
	for _, svc := range []string{"hub", "all"} {
		c, _, stderr := newTestCLI(t, nil)
		c.getenv = func(k string) string {
			return map[string]string{"OBSERVATORY_LISTEN": ":8091", "OBSERVATORY_EDGE_LISTEN": "127.0.0.1:0"}[k]
		}
		if code := run(c, []string{"serve", svc}); code != 1 || !strings.Contains(stderr.String(), "OBSERVATORY_HUB_LISTEN") {
			t.Errorf("serve %s: exit %d, stderr %q", svc, code, stderr.String())
		}
	}
}

func TestHubListenDefaults(t *testing.T) {
	unset := func(string) string { return "" }
	set := func(string) string { return "10.0.0.1:9000" }
	for _, tc := range []struct{ svc, want string }{{"hub", ":8091"}, {"all", "127.0.0.1:8091"}} {
		if got := hubListen(unset, tc.svc); got != tc.want {
			t.Errorf("serve %s default %q, want %q", tc.svc, got, tc.want)
		}
		if got := hubListen(set, tc.svc); got != "10.0.0.1:9000" {
			t.Errorf("serve %s ignores OBSERVATORY_HUB_LISTEN: %q", tc.svc, got)
		}
	}
}

func TestServeHubListensOnHubListen(t *testing.T) {
	taken, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer taken.Close()
	c, _, stderr := newTestCLI(t, nil)
	c.getenv = func(k string) string { return map[string]string{"OBSERVATORY_HUB_LISTEN": taken.Addr().String()}[k] }
	if code := run(c, []string{"serve", "hub"}); code != 1 || !strings.Contains(stderr.String(), taken.Addr().String()) {
		t.Fatalf("exit %d, stderr %q; want a bind error on the taken OBSERVATORY_HUB_LISTEN address", code, stderr.String())
	}
}

func serveAllEnv(t *testing.T, pool *pgxpool.Pool, edgeLn net.Listener) map[string]string {
	t.Helper()
	idp := httptest.NewServer(nil)
	t.Cleanup(idp.Close)
	idp.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/.well-known/openid-configuration" {
			http.NotFound(w, r)
			return
		}
		io.WriteString(w, `{"issuer":"`+idp.URL+`","authorization_endpoint":"`+idp.URL+`/auth","token_endpoint":"`+idp.URL+`/token","jwks_uri":"`+idp.URL+`/keys"}`)
	})
	key := make([]byte, 32)
	rand.Read(key)
	env := map[string]string{
		"OBSERVATORY_DATABASE_URL":       pool.Config().ConnString(),
		"OBSERVATORY_PUBLIC_URL":         "http://" + edgeLn.Addr().String(),
		"OBSERVATORY_OIDC_ISSUER":        idp.URL,
		"OBSERVATORY_OIDC_WEB_CLIENT_ID": "web",
		"OBSERVATORY_OIDC_CLI_CLIENT_ID": "cli",
		"OBSERVATORY_GATEWAY_TOKEN":      strings.Repeat("g", 32),
		"OBSERVATORY_STORAGE_TOKEN":      strings.Repeat("s", 32),
		"OBSERVATORY_SECRETS_KEY":        base64.StdEncoding.EncodeToString(key),
		"OBSERVATORY_NO_UPDATE_CHECK":    "1",
		"OBSERVATORY_NODE_BINARY":        t.TempDir() + "/none",
		"OBSERVATORY_HUB_LISTEN":         "127.0.0.1:0",
		"OBSERVATORY_DATA_DIR":           t.TempDir(),
		// Invalid on purpose: serve all must replace it with the tokens it generates, or the hub would refuse to start.
		"OBSERVATORY_SERVICE_TOKENS": "invalid",
	}
	return env
}

// TestServeAllRecordsClientIPThroughEdge runs the real edge, hub, bus and audit service: the client reaches the edge over ::1 while the edge reaches the hub over 127.0.0.1, so an audited ::1 proves the hub trusts the edge's header.
func TestServeAllRecordsClientIPThroughEdge(t *testing.T) {
	edgeLn, err := net.Listen("tcp", "[::1]:0")
	if err != nil {
		t.Skipf("no IPv6 loopback: %v", err)
	}
	pool := dbtest.New(t)
	env := serveAllEnv(t, pool, edgeLn)
	// Created before start: the hub loads its policy snapshot then and is not told about later direct writes.
	s := store.New(pool)
	alice, err := s.Login(t.Context(), store.Identity{Subject: "alice", Email: "alice@x.io", Name: "alice"})
	if err != nil {
		t.Fatal(err)
	}
	org, err := s.CreateOrg(t.Context(), "acme", alice.User.ID)
	if err != nil {
		t.Fatal(err)
	}
	session, _, err := s.CreateSession(t.Context(), alice.User.ID, store.SessionCLI, time.Hour, "test", "127.0.0.1")
	if err != nil {
		t.Fatal(err)
	}
	bob, err := s.Login(t.Context(), store.Identity{Subject: "bob", Email: "bob@x.io", Name: "bob"})
	if err != nil {
		t.Fatal(err)
	}
	inv, _, err := s.CreateInvite(t.Context(), org.ID, store.RoleMember, alice.User.ID, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.AcceptInvite(t.Context(), inv, bob.User.ID); err != nil {
		t.Fatal(err)
	}
	bobSession, _, err := s.CreateSession(t.Context(), bob.User.ID, store.SessionCLI, time.Hour, "test", "127.0.0.1")
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- serveAll(ctx, func(k string) string { return env[k] }, edgeLn) }()

	client := &http.Client{Timeout: 30 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	for _, spoof := range []bool{false, true} {
		req, _ := http.NewRequest("GET", "http://"+edgeLn.Addr().String()+"/auth/callback", nil)
		if spoof {
			req.Header.Set(edge.HeaderClientIP, "203.0.113.9")
			req.Header.Set(edge.HeaderEdge, "guessed")
		}
		resp, err := client.Do(req)
		if err != nil {
			cancel()
			t.Fatalf("request through the edge: %v (serve all: %v)", err, <-done)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusFound {
			t.Fatalf("callback status %d", resp.StatusCode)
		}
	}

	ips := []string{}
	for _, e := range storetest.Audit(t, pool, store.ActionAuthLogin) {
		ips = append(ips, e.IP)
	}
	if len(ips) != 2 || ips[0] != "::1" || ips[1] != "::1" {
		t.Fatalf("audited IPs %v, want the edge's ::1 twice", ips)
	}

	invite := func(token string) int {
		req, _ := http.NewRequest("POST", "http://"+edgeLn.Addr().String()+"/api/iam/v1/orgs/"+org.ID.String()+"/invites", strings.NewReader(`{"role":"member"}`))
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("X-Observatory", "1")
		resp, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}
	if code := invite(session); code != http.StatusOK {
		t.Fatalf("owner invite: %d", code)
	}
	if code := invite(bobSession); code != http.StatusForbidden {
		t.Fatalf("member invite: %d", code)
	}

	// The hub's events reach the audit service through its outbox, the relay and the bus.
	auditClient := auditv1connect.NewAuditServiceClient(client, "http://"+edgeLn.Addr().String()+"/api/audit/v1")
	var allowed, denied *eventsv1.Event
	deadline := time.Now().Add(15 * time.Second)
	for allowed == nil || denied == nil {
		req := connect.NewRequest(&auditv1.ListEventsRequest{Org: org.ID.String(), Filter: &auditv1.EventFilter{Type: store.ActionMemberInvite}})
		req.Header().Set("Authorization", "Bearer "+session)
		resp, err := auditClient.ListEvents(t.Context(), req)
		// Unavailable until the audit service has its first policy snapshot from the hub.
		if err != nil && connect.CodeOf(err) != connect.CodeUnavailable {
			cancel()
			t.Fatalf("audit through the edge: %v", err)
		}
		if err == nil {
			for _, e := range resp.Msg.GetEvents() {
				switch e.GetResult() {
				case eventsv1.Result_RESULT_ALLOWED:
					allowed = e
				case eventsv1.Result_RESULT_DENIED:
					denied = e
				}
			}
		}
		if time.Now().After(deadline) {
			cancel()
			t.Fatalf("invite events not in the audit trail: allowed %v, denied %v", allowed, denied)
		}
		time.Sleep(50 * time.Millisecond)
	}
	if allowed.GetActor().GetId() != alice.User.ID.String() || allowed.GetOrg() != org.ID.String() || allowed.GetClientIp() != "::1" {
		t.Fatalf("allowed invite event %v", allowed)
	}
	if denied.GetActor().GetId() != bob.User.ID.String() || denied.GetOrg() != org.ID.String() || denied.GetClientIp() != "::1" {
		t.Fatalf("denied invite event %v", denied)
	}

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("serve all did not stop")
	}
	if c, err := net.Dial("tcp", edgeLn.Addr().String()); err == nil {
		c.Close()
		t.Fatal("edge still accepts connections")
	}
}

func TestServeAllIAMBoundary(t *testing.T) {
	edgeLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	pool := dbtest.New(t)
	env := serveAllEnv(t, pool, edgeLn)
	master, err := seal.ParseKey(env["OBSERVATORY_SECRETS_KEY"])
	if err != nil {
		t.Fatal(err)
	}
	ctx := t.Context()
	s := store.New(pool).WithMasterKey(master)
	alice, err := s.Login(ctx, store.Identity{Subject: "alice", Email: "alice@x.io", Name: "alice"})
	if err != nil {
		t.Fatal(err)
	}
	org, err := s.CreateOrg(ctx, "acme", alice.User.ID)
	if err != nil {
		t.Fatal(err)
	}
	aliceToken, _, err := s.CreateSession(ctx, alice.User.ID, store.SessionCLI, time.Hour, "test", "127.0.0.1")
	if err != nil {
		t.Fatal(err)
	}
	bob, err := s.Login(ctx, store.Identity{Subject: "bob", Email: "bob@x.io", Name: "bob"})
	if err != nil {
		t.Fatal(err)
	}
	inv, _, err := s.CreateInvite(ctx, org.ID, store.RoleMember, alice.User.ID, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.AcceptInvite(ctx, inv, bob.User.ID); err != nil {
		t.Fatal(err)
	}
	bobToken, _, err := s.CreateSession(ctx, bob.User.ID, store.SessionCLI, time.Hour, "test", "127.0.0.1")
	if err != nil {
		t.Fatal(err)
	}
	node, enroll, err := s.CreateNode(ctx, store.Node{OrgID: org.ID, Name: "n1", CreatedBy: alice.User.ID})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.EnrollNode(ctx, enroll); err != nil {
		t.Fatal(err)
	}
	vm, err := s.CreateVM(ctx, store.VM{OrgID: org.ID, NodeID: node.ID, UserID: &bob.User.ID, Name: "bobvm", CPUs: 1, MemoryMB: 128, DiskGB: 1, CreatedBy: bob.User.ID})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateEnvVar(ctx, store.EnvVar{OrgID: org.ID, Kind: store.EnvPlain, Scope: store.EnvScopeUser, OwnerID: &bob.User.ID, Name: "BOB_PRIVATE", CreatedBy: bob.User.ID, UpdatedBy: bob.User.ID}, "x"); err != nil {
		t.Fatal(err)
	}

	var envRows int
	if err := pool.QueryRow(ctx, "SELECT count(*) FROM env_vars WHERE org_id = $1 AND user_id = $2", org.ID, bob.User.ID).Scan(&envRows); err != nil || envRows != 1 {
		t.Fatalf("bob's env var rows before removal: %d, %v", envRows, err)
	}

	runCtx, cancel := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() { done <- serveAll(runCtx, func(k string) string { return env[k] }, edgeLn) }()
	defer func() {
		cancel()
		if err := <-done; err != nil {
			t.Error(err)
		}
	}()

	client := &http.Client{Timeout: 30 * time.Second}
	base := "http://" + edgeLn.Addr().String()
	call := func(method, path, token string) (int, string) {
		req, _ := http.NewRequest(method, base+path, nil)
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		req.Header.Set("X-Observatory", "1")
		resp, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		body, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(body)
	}

	iam := iamv1connect.NewIAMServiceClient(client, base+"/api/iam/v1")
	me, err := iam.GetMe(ctx, bearer(&iamv1.GetMeRequest{}, aliceToken))
	if err != nil || me.Msg.GetUser().GetName() != "alice" {
		t.Fatalf("GetMe with a CLI session: %v %v", me, err)
	}
	// A non-session bearer is not swapped at the edge, so the 401 comes from the node auth in the hub.
	if code, body := call("GET", "/api/nodes/health", "not-a-session-token"); code != http.StatusUnauthorized || body != "Unauthorized\n" {
		t.Fatalf("/api/nodes/health: %d %s", code, body)
	}
	// The query guard answers before authentication.
	if code, body := call("GET", "/api/iam/v1/me?org=x", ""); code != http.StatusBadRequest {
		t.Fatalf("REST query without a session: %d %s", code, body)
	}

	_, err = iam.DeleteMember(ctx, bearer(&iamv1.DeleteMemberRequest{Org: org.ID.String(), User: bob.User.ID.String()}, aliceToken))
	if connect.CodeOf(err) != connect.CodeFailedPrecondition {
		t.Fatalf("remove a member that owns a VM: %v", err)
	}
	if _, err := pool.Exec(ctx, "DELETE FROM vms WHERE id = $1", vm.ID); err != nil {
		t.Fatal(err)
	}
	member := "/api/iam/v1/orgs/" + org.ID.String() + "/members/"
	if code, body := call("DELETE", member+bob.User.ID.String(), ""); code != http.StatusUnauthorized {
		t.Fatalf("REST remove without a session: %d %s", code, body)
	}
	if code, body := call("DELETE", member+alice.User.ID.String(), bobToken); code != http.StatusForbidden {
		t.Fatalf("REST remove by a member: %d %s", code, body)
	}
	if code, body := call("DELETE", member+bob.User.ID.String(), aliceToken); code != http.StatusOK {
		t.Fatalf("REST remove by the owner: %d %s", code, body)
	}

	// The owner's removal is one allowed iam.members.delete event, which the hub's cleanup acts on.
	auditClient := auditv1connect.NewAuditServiceClient(client, base+"/api/audit/v1")
	deadline := time.Now().Add(15 * time.Second)
	for {
		var n int
		if err := pool.QueryRow(ctx, "SELECT count(*) FROM env_vars WHERE org_id = $1 AND user_id = $2", org.ID, bob.User.ID).Scan(&n); err != nil {
			t.Fatal(err)
		}
		var allowed, denied, events []*eventsv1.Event
		resp, err := auditClient.ListEvents(ctx, bearer(&auditv1.ListEventsRequest{Org: org.ID.String(), Filter: &auditv1.EventFilter{Type: store.ActionMemberRemove}}, aliceToken))
		// Unavailable until the audit service has its first policy snapshot from the hub.
		if err != nil && connect.CodeOf(err) != connect.CodeUnavailable {
			t.Fatalf("audit through the edge: %v", err)
		}
		if err == nil {
			events = resp.Msg.GetEvents()
		}
		for _, e := range events {
			switch e.GetResult() {
			case eventsv1.Result_RESULT_ALLOWED:
				allowed = append(allowed, e)
			case eventsv1.Result_RESULT_DENIED:
				denied = append(denied, e)
			}
		}
		if n == 0 && len(allowed) > 0 && len(denied) > 0 {
			if len(allowed) != 1 || allowed[0].GetResource() != "org/members/"+bob.User.ID.String() || allowed[0].GetActor().GetId() != alice.User.ID.String() {
				t.Fatalf("allowed iam.members.delete events %v", allowed)
			}
			if len(denied) != 1 || denied[0].GetActor().GetId() != bob.User.ID.String() {
				t.Fatalf("denied iam.members.delete events %v", denied)
			}
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("after the removal: %d private env vars of bob, allowed %v, denied %v", n, allowed, denied)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

func bearer[T any](msg *T, token string) *connect.Request[T] {
	req := connect.NewRequest(msg)
	req.Header().Set("Authorization", "Bearer "+token)
	return req
}

// TestServeAllRefusesEndedSessionsAtOnce warms the edge's JWT cache, then ends the session: hub routes must refuse it on the next request.
func TestServeAllRefusesEndedSessionsAtOnce(t *testing.T) {
	edgeLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	pool := dbtest.New(t)
	env := serveAllEnv(t, pool, edgeLn)
	ctx := t.Context()
	s := store.New(pool)
	alice, err := s.Login(ctx, store.Identity{Subject: "alice", Email: "alice@x.io", Name: "alice"})
	if err != nil {
		t.Fatal(err)
	}
	newSession := func(kind store.SessionKind) (string, store.Session) {
		tok, sess, err := s.CreateSession(ctx, alice.User.ID, kind, time.Hour, "test", "127.0.0.1")
		if err != nil {
			t.Fatal(err)
		}
		return tok, sess
	}
	web, _ := newSession(store.SessionBrowser)
	cli, _ := newSession(store.SessionCLI)
	other, otherSess := newSession(store.SessionCLI)

	runCtx, cancel := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() { done <- serveAll(runCtx, func(k string) string { return env[k] }, edgeLn) }()
	defer func() {
		cancel()
		if err := <-done; err != nil {
			t.Error(err)
		}
	}()

	client := &http.Client{Timeout: 30 * time.Second}
	call := func(method, path, token string, cookie bool) int {
		req, _ := http.NewRequest(method, "http://"+edgeLn.Addr().String()+path, nil)
		if cookie {
			req.AddCookie(&http.Cookie{Name: "obs_session", Value: token})
		} else {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		req.Header.Set("X-Observatory", "1")
		resp, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}

	for _, c := range []struct {
		token  string
		cookie bool
	}{{web, true}, {other, false}} {
		if code := call("GET", "/api/info", c.token, c.cookie); code != http.StatusOK {
			t.Fatalf("/api/info before: %d", code)
		}
	}
	// The real CLI transport, so a request the edge or the transcoder misroutes fails here.
	c, stdout, stderr := newTestCLI(t, nil)
	t.Setenv("OBSERVATORY_HUB", "http://"+edgeLn.Addr().String())
	t.Setenv("OBSERVATORY_TOKEN", other)
	if code := run(c, []string{"whoami"}); code != 0 || !strings.Contains(stdout.String(), "alice") {
		t.Fatalf("whoami: exit %d, stdout %q, stderr %q", code, stdout, stderr)
	}
	if code := call("POST", "/auth/logout", web, true); code != http.StatusNoContent {
		t.Fatalf("logout: %d", code)
	}
	if code := call("GET", "/api/iam/v1/me", web, true); code != http.StatusUnauthorized {
		t.Fatalf("GetMe with the logged-out cookie: %d", code)
	}
	if code := call("GET", "/api/info", web, true); code != http.StatusUnauthorized {
		t.Fatalf("/api/info with the logged-out cookie: %d", code)
	}
	if code := call("DELETE", "/api/iam/v1/me/sessions/"+otherSess.ID.String(), cli, false); code != http.StatusOK {
		t.Fatalf("revoke: %d", code)
	}
	// The edge still caches this session's JWT, so the 401 must come from iam's own session check.
	if code := run(c, []string{"whoami"}); code != 3 {
		t.Fatalf("whoami with the revoked session: exit %d, stderr %q", code, stderr)
	}
	if code := call("GET", "/api/iam/v1/me", other, false); code != http.StatusUnauthorized {
		t.Fatalf("GetMe with the revoked session: %d", code)
	}
	if code := call("GET", "/api/info", other, false); code != http.StatusUnauthorized {
		t.Fatalf("/api/info with the revoked session: %d", code)
	}
}

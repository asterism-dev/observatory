package main

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"connectrpc.com/connect"
	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	auditv1 "github.com/asterism-dev/observatory/gen/go/observatory/audit/v1"
	"github.com/asterism-dev/observatory/gen/go/observatory/audit/v1/auditv1connect"
	eventsv1 "github.com/asterism-dev/observatory/gen/go/observatory/events/v1"
	iamv1 "github.com/asterism-dev/observatory/gen/go/observatory/iam/v1"
	"github.com/asterism-dev/observatory/gen/go/observatory/iam/v1/iamv1connect"
	"github.com/asterism-dev/observatory/internal/authz"
	"github.com/asterism-dev/observatory/internal/db/dbtest"
	"github.com/asterism-dev/observatory/internal/edge"
	iamstore "github.com/asterism-dev/observatory/internal/iam/store"
	iamstoretest "github.com/asterism-dev/observatory/internal/iam/store/storetest"
	"github.com/asterism-dev/observatory/internal/platform/bus"
	"github.com/asterism-dev/observatory/internal/quota"
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
	code := run(c, []string{"serve", "edge"})
	for _, k := range []string{"OBSERVATORY_EDGE_TOKEN", "OBSERVATORY_EDGE_HUB_URL", "OBSERVATORY_EDGE_IAM_URL", "OBSERVATORY_SERVICE_TOKEN"} {
		if code != 1 || !strings.Contains(stderr.String(), k) {
			t.Fatalf("exit %d, stderr %q; want %s reported missing", code, stderr.String(), k)
		}
	}
}

func TestServeIAMMissingEnv(t *testing.T) {
	c, _, stderr := newTestCLI(t, nil)
	c.getenv = func(k string) string { return map[string]string{"OBSERVATORY_IAM_LISTEN": "127.0.0.1:0"}[k] }
	code := run(c, []string{"serve", "iam"})
	for _, k := range []string{"OBSERVATORY_DATABASE_URL", "OBSERVATORY_PUBLIC_URL", "OBSERVATORY_OIDC_ISSUER", "OBSERVATORY_OIDC_WEB_CLIENT_ID",
		"OBSERVATORY_OIDC_CLI_CLIENT_ID", "OBSERVATORY_SECRETS_KEY", "OBSERVATORY_EDGE_TOKEN", "OBSERVATORY_SERVICE_TOKEN", "OBSERVATORY_HUB_URL",
		"OBSERVATORY_AUDIT_URL", "OBSERVATORY_NATS_URL", "OBSERVATORY_NATS_TOKEN"} {
		if code != 1 || !strings.Contains(stderr.String(), k) {
			t.Fatalf("exit %d, stderr %q; want %s reported missing", code, stderr.String(), k)
		}
	}
}

func TestServeIAMRejectsBadSecretsKey(t *testing.T) {
	c, _, stderr := newTestCLI(t, nil)
	env := map[string]string{"OBSERVATORY_IAM_LISTEN": "127.0.0.1:0", "OBSERVATORY_SECRETS_KEY": "short"}
	c.getenv = func(k string) string { return env[k] }
	if code := run(c, []string{"serve", "iam"}); code != 1 || !strings.Contains(stderr.String(), "OBSERVATORY_SECRETS_KEY:") {
		t.Fatalf("exit %d, stderr %q", code, stderr.String())
	}
}

func TestServeAuditRequiresConfig(t *testing.T) {
	c, _, stderr := newTestCLI(t, nil)
	c.getenv = func(k string) string { return map[string]string{"OBSERVATORY_AUDIT_LISTEN": "127.0.0.1:0"}[k] }
	code := run(c, []string{"serve", "audit"})
	for _, k := range []string{"OBSERVATORY_DATABASE_URL", "OBSERVATORY_IAM_URL", "OBSERVATORY_PUBLIC_URL", "OBSERVATORY_SERVICE_TOKEN", "OBSERVATORY_NATS_URL", "OBSERVATORY_NATS_TOKEN"} {
		if code != 1 || !strings.Contains(stderr.String(), k) {
			t.Fatalf("exit %d, stderr %q; want %s reported missing", code, stderr.String(), k)
		}
	}
}

func TestServeAuditRejectsBadServiceTokens(t *testing.T) {
	c, _, stderr := newTestCLI(t, nil)
	env := map[string]string{"OBSERVATORY_AUDIT_LISTEN": "127.0.0.1:0", "OBSERVATORY_DATABASE_URL": "postgres://x", "OBSERVATORY_IAM_URL": "http://iam",
		"OBSERVATORY_PUBLIC_URL": "https://obs", "OBSERVATORY_SERVICE_TOKEN": strings.Repeat("s", 32), "OBSERVATORY_NATS_URL": "nats://x",
		"OBSERVATORY_NATS_TOKEN": strings.Repeat("n", 32), "OBSERVATORY_SERVICE_TOKENS": "compute=short"}
	c.getenv = func(k string) string { return env[k] }
	if code := run(c, []string{"serve", "audit"}); code != 1 || !strings.Contains(stderr.String(), "OBSERVATORY_SERVICE_TOKENS") {
		t.Fatalf("exit %d, stderr %q", code, stderr.String())
	}
}

func TestServeQuotaRequiresConfig(t *testing.T) {
	c, _, stderr := newTestCLI(t, nil)
	c.getenv = func(k string) string { return map[string]string{"OBSERVATORY_QUOTA_LISTEN": "127.0.0.1:0"}[k] }
	code := run(c, []string{"serve", "quota"})
	for _, k := range []string{"OBSERVATORY_QUOTA_DATABASE_URL or OBSERVATORY_DATABASE_URL", "OBSERVATORY_IAM_URL", "OBSERVATORY_PUBLIC_URL", "OBSERVATORY_SERVICE_TOKEN", "OBSERVATORY_NATS_URL", "OBSERVATORY_NATS_TOKEN", "OBSERVATORY_AUDIT_URL"} {
		if code != 1 || !strings.Contains(stderr.String(), k) {
			t.Fatalf("exit %d, stderr %q; want %s reported missing", code, stderr.String(), k)
		}
	}
}

func TestServeQuotaRequiresHubServiceToken(t *testing.T) {
	env := map[string]string{"OBSERVATORY_QUOTA_LISTEN": "127.0.0.1:0", "OBSERVATORY_DATABASE_URL": "postgres://x", "OBSERVATORY_IAM_URL": "http://iam",
		"OBSERVATORY_PUBLIC_URL": "https://obs", "OBSERVATORY_SERVICE_TOKEN": strings.Repeat("s", 32), "OBSERVATORY_NATS_URL": "nats://x",
		"OBSERVATORY_NATS_TOKEN": strings.Repeat("n", 32), "OBSERVATORY_AUDIT_URL": "http://audit"}
	for _, tokens := range []string{"", "hub=short", "compute=" + strings.Repeat("c", 32)} {
		c, _, stderr := newTestCLI(t, nil)
		env["OBSERVATORY_SERVICE_TOKENS"] = tokens
		c.getenv = func(k string) string { return env[k] }
		if code := run(c, []string{"serve", "quota"}); code != 1 || !strings.Contains(stderr.String(), "OBSERVATORY_SERVICE_TOKENS") {
			t.Fatalf("%q: exit %d, stderr %q", tokens, code, stderr.String())
		}
	}
}

func TestServeAllRequiresPersistentBus(t *testing.T) {
	c, _, stderr := newTestCLI(t, nil)
	c.getenv = func(k string) string { return map[string]string{"OBSERVATORY_EDGE_LISTEN": "127.0.0.1:0"}[k] }
	if code := run(c, []string{"serve", "all"}); code != 1 || !strings.Contains(stderr.String(), "OBSERVATORY_DATA_DIR") || !strings.Contains(stderr.String(), "OBSERVATORY_NATS_URL") {
		t.Fatalf("exit %d, stderr %q", code, stderr.String())
	}
}

func TestServeAllNamesMissingOIDC(t *testing.T) {
	c, _, stderr := newTestCLI(t, nil)
	env := map[string]string{"OBSERVATORY_EDGE_LISTEN": "127.0.0.1:0", "OBSERVATORY_DATA_DIR": t.TempDir()}
	c.getenv = func(k string) string { return env[k] }
	code := run(c, []string{"serve", "all"})
	for _, k := range []string{"OBSERVATORY_OIDC_ISSUER", "OBSERVATORY_OIDC_WEB_CLIENT_ID", "OBSERVATORY_OIDC_CLI_CLIENT_ID"} {
		if code != 1 || !strings.Contains(stderr.String(), k) {
			t.Fatalf("exit %d, stderr %q; want %s reported missing", code, stderr.String(), k)
		}
	}
}

func TestServeEdgeRejectsBadServices(t *testing.T) {
	c, _, stderr := newTestCLI(t, nil)
	c.getenv = func(k string) string {
		return map[string]string{"OBSERVATORY_EDGE_LISTEN": "127.0.0.1:0", "OBSERVATORY_EDGE_TOKEN": strings.Repeat("t", 32),
			"OBSERVATORY_EDGE_HUB_URL": "http://hub", "OBSERVATORY_EDGE_IAM_URL": "http://iam", "OBSERVATORY_SERVICE_TOKEN": strings.Repeat("s", 32), "OBSERVATORY_EDGE_SERVICES": "audit"}[k]
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

// TestServeAllStartsIAM: the hub no longer serves login routes, so a 200 here comes from iam behind the edge.
func TestServeAllStartsIAM(t *testing.T) {
	edgeLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	env := serveAllEnv(t, dbtest.New(t), edgeLn)
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- serveAll(ctx, func(k string) string { return env[k] }, edgeLn) }()
	defer func() {
		cancel()
		if err := <-done; err != nil {
			t.Error(err)
		}
	}()
	resp, err := (&http.Client{Timeout: 30 * time.Second}).Get("http://" + edgeLn.Addr().String() + "/api/auth/config")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("/api/auth/config: %d", resp.StatusCode)
	}
}

// TestServeAllIAMDataEvents: iam follows the audit service's data events setting, so its read calls reach the trail once an admin turns it on.
func TestServeAllIAMDataEvents(t *testing.T) {
	edgeLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	pool := dbtest.New(t)
	env := serveAllEnv(t, pool, edgeLn)
	s, _ := iamStore(t, pool)
	alice, err := s.Login(t.Context(), iamstore.Identity{Subject: "alice", Email: "alice@x.io", Name: "alice"})
	if err != nil {
		t.Fatal(err)
	}
	org, err := s.CreateOrg(t.Context(), "acme", alice.User.ID)
	if err != nil {
		t.Fatal(err)
	}
	token, _, err := s.CreateSession(t.Context(), alice.User.ID, iamstore.SessionCLI, time.Hour, "test", "127.0.0.1")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- serveAll(ctx, func(k string) string { return env[k] }, edgeLn) }()
	defer func() {
		cancel()
		if err := <-done; err != nil {
			t.Error(err)
		}
	}()

	client := &http.Client{Timeout: 30 * time.Second}
	base := "http://" + edgeLn.Addr().String()
	auditClient := auditv1connect.NewAuditServiceClient(client, base+"/api/audit/v1")
	iamClient := iamv1connect.NewIAMServiceClient(client, base+"/api/iam/v1")
	deadline := time.Now().Add(15 * time.Second)
	for {
		_, err := auditClient.UpdateSettings(t.Context(), bearer(&auditv1.UpdateSettingsRequest{Org: org.ID.String(), DataEvents: map[string]bool{"iam": true}}, token))
		if err == nil {
			break
		}
		if connect.CodeOf(err) != connect.CodeUnavailable || time.Now().After(deadline) {
			t.Fatalf("turn on iam data events: %v", err)
		}
		time.Sleep(50 * time.Millisecond)
	}
	for {
		if _, err := iamClient.ListMembers(t.Context(), bearer(&iamv1.ListMembersRequest{Org: org.ID.String()}, token)); err != nil {
			t.Fatalf("list members: %v", err)
		}
		resp, err := auditClient.ListEvents(t.Context(), bearer(&auditv1.ListEventsRequest{Org: org.ID.String(), Filter: &auditv1.EventFilter{Type: "iam.members.list"}}, token))
		if err != nil {
			t.Fatalf("audit: %v", err)
		}
		if len(resp.Msg.GetEvents()) > 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("iam's read calls never reached the audit trail")
		}
		time.Sleep(100 * time.Millisecond)
	}
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
	s, ipool := iamStore(t, pool)
	alice, err := s.Login(t.Context(), iamstore.Identity{Subject: "alice", Email: "alice@x.io", Name: "alice"})
	if err != nil {
		t.Fatal(err)
	}
	org, err := s.CreateOrg(t.Context(), "acme", alice.User.ID)
	if err != nil {
		t.Fatal(err)
	}
	session, _, err := s.CreateSession(t.Context(), alice.User.ID, iamstore.SessionCLI, time.Hour, "test", "127.0.0.1")
	if err != nil {
		t.Fatal(err)
	}
	bob, err := s.Login(t.Context(), iamstore.Identity{Subject: "bob", Email: "bob@x.io", Name: "bob"})
	if err != nil {
		t.Fatal(err)
	}
	inv, _, err := s.CreateInvite(t.Context(), org.ID, iamstore.RoleMember, alice.User.ID, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.AcceptInvite(t.Context(), inv, bob.User.ID); err != nil {
		t.Fatal(err)
	}
	bobSession, _, err := s.CreateSession(t.Context(), bob.User.ID, iamstore.SessionCLI, time.Hour, "test", "127.0.0.1")
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
	for _, e := range iamstoretest.Audit(t, ipool, iamstore.ActionAuthLogin) {
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
		req := connect.NewRequest(&auditv1.ListEventsRequest{Org: org.ID.String(), Filter: &auditv1.EventFilter{Type: iamstore.ActionMemberInvite}})
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

// fakeIdP is an OIDC provider whose authorization code is the subject to log in, so a test drives the real login routes.
type fakeIdP struct {
	*httptest.Server
	key *rsa.PrivateKey
}

func newFakeIdP(t *testing.T) *fakeIdP {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	idp := &fakeIdP{Server: httptest.NewUnstartedServer(nil), key: key}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /.well-known/openid-configuration", func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"issuer":"`+idp.URL+`","authorization_endpoint":"`+idp.URL+`/auth","token_endpoint":"`+idp.URL+`/token","jwks_uri":"`+idp.URL+`/keys"}`)
	})
	mux.HandleFunc("GET /keys", func(w http.ResponseWriter, r *http.Request) {
		b64 := base64.RawURLEncoding.EncodeToString
		_ = json.NewEncoder(w).Encode(map[string]any{"keys": []map[string]string{{"kty": "RSA", "kid": "k", "alg": "RS256", "use": "sig",
			"n": b64(key.N.Bytes()), "e": b64(big.NewInt(int64(key.E)).Bytes())}}})
	})
	mux.HandleFunc("POST /token", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{"access_token": "at", "token_type": "Bearer", "id_token": idp.idToken(t, r.FormValue("code"), "web")})
	})
	idp.Config.Handler = mux
	idp.Start()
	t.Cleanup(idp.Close)
	return idp
}

func (idp *fakeIdP) idToken(t *testing.T, subject, audience string) string {
	tok := jwt.NewWithClaims(jwt.SigningMethodRS256, jwt.MapClaims{"iss": idp.URL, "aud": audience, "sub": subject, "email": subject + "@x.io", "name": subject,
		"iat": time.Now().Unix(), "exp": time.Now().Add(time.Hour).Unix()})
	tok.Header["kid"] = "k"
	raw, err := tok.SignedString(idp.key)
	if err != nil {
		t.Error(err)
	}
	return raw
}

// TestServeAllIAMService runs iam as its own service behind the edge: login through the IdP, the hub following iam's policy, member removal and session checks per registry use.
func TestServeAllIAMService(t *testing.T) {
	edgeLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	pool := dbtest.New(t)
	env := serveAllEnv(t, pool, edgeLn)
	idp := newFakeIdP(t)
	env["OBSERVATORY_OIDC_ISSUER"] = idp.URL
	// An external bus the test can stop; serve all then skips its embedded one.
	natsURL, natsToken, stopNATS, err := bus.Embedded(t.Context(), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	stopNATS = sync.OnceFunc(stopNATS)
	defer stopNATS()
	env["OBSERVATORY_NATS_URL"], env["OBSERVATORY_NATS_TOKEN"] = natsURL, natsToken
	env["OBSERVATORY_REGISTRY_URL"] = "http://127.0.0.1:1"
	env["OBSERVATORY_REGISTRY_KEY_DIR"] = t.TempDir()
	env["OBSERVATORY_REGISTRY_EVENTS_SECRET"] = strings.Repeat("e", 32)
	master, err := seal.ParseKey(env["OBSERVATORY_SECRETS_KEY"])
	if err != nil {
		t.Fatal(err)
	}
	ctx := t.Context()
	hs := storetest.New(t, pool).WithMasterKey(master)

	runCtx, cancel := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() { done <- serveAll(runCtx, func(k string) string { return env[k] }, edgeLn) }()
	defer func() {
		cancel()
		if err := <-done; err != nil {
			t.Error(err)
		}
	}()

	client := &http.Client{Timeout: 30 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	base := "http://" + edgeLn.Addr().String()
	type auth func(*http.Request)
	cookie := func(token string) auth {
		return func(r *http.Request) { r.AddCookie(&http.Cookie{Name: "obs_session", Value: token}) }
	}
	bearerAuth := func(token string) auth {
		return func(r *http.Request) { r.Header.Set("Authorization", "Bearer "+token) }
	}
	do := func(method, path, body string, a auth) *http.Response {
		t.Helper()
		req, err := http.NewRequest(method, base+path, strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		if a != nil {
			a(req)
		}
		req.Header.Set("X-Observatory", "1")
		resp, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		return resp
	}
	call := func(method, path, body string, a auth) (int, string) {
		t.Helper()
		resp := do(method, path, body, a)
		defer resp.Body.Close()
		raw, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(raw)
	}
	within := func(d time.Duration, what string, ok func() (bool, string)) {
		t.Helper()
		deadline := time.Now().Add(d)
		for {
			done, state := ok()
			if done {
				return
			}
			if time.Now().After(deadline) {
				t.Fatalf("%s not within %s: %s", what, d, state)
			}
			time.Sleep(20 * time.Millisecond)
		}
	}
	rpc := func(a auth) connect.ClientOption {
		return connect.WithInterceptors(connect.UnaryInterceptorFunc(func(next connect.UnaryFunc) connect.UnaryFunc {
			return func(ctx context.Context, req connect.AnyRequest) (connect.AnyResponse, error) {
				r := &http.Request{Header: req.Header()}
				a(r)
				req.Header().Set("X-Observatory", "1")
				return next(ctx, req)
			}
		}))
	}

	// Browser login: the edge sends /auth/ to iam, iam to the IdP and back.
	start := do("GET", "/auth/login", "", nil)
	start.Body.Close()
	loc, err := url.Parse(start.Header.Get("Location"))
	if err != nil || start.StatusCode != http.StatusFound || !strings.HasPrefix(loc.String(), idp.URL+"/auth") {
		t.Fatalf("login: %d %v", start.StatusCode, loc)
	}
	cb := do("GET", "/auth/callback?code=alice&state="+url.QueryEscape(loc.Query().Get("state")), "", func(r *http.Request) {
		for _, c := range start.Cookies() {
			r.AddCookie(c)
		}
	})
	cb.Body.Close()
	var aliceToken string
	for _, c := range cb.Cookies() {
		if c.Name == "obs_session" {
			aliceToken = c.Value
		}
	}
	if cb.StatusCode != http.StatusFound || cb.Header.Get("Location") != "/" || aliceToken == "" {
		t.Fatalf("callback: %d %q, session cookie %t", cb.StatusCode, cb.Header.Get("Location"), aliceToken != "")
	}
	alice := cookie(aliceToken)
	aliceIAM := iamv1connect.NewIAMServiceClient(client, base+"/api/iam/v1", rpc(alice))
	me, err := aliceIAM.GetMe(ctx, connect.NewRequest(&iamv1.GetMeRequest{}))
	if err != nil || me.Msg.GetUser().GetName() != "alice" || len(me.Msg.GetOrgs()) != 1 || !me.Msg.GetOrgs()[0].GetPersonal() {
		t.Fatalf("GetMe after login: %v %v", me, err)
	}
	// A non-session bearer is not swapped at the edge, so the 401 comes from the node auth in the hub.
	if code, body := call("GET", "/api/nodes/health", "", bearerAuth("not-a-session-token")); code != http.StatusUnauthorized || body != "Unauthorized\n" {
		t.Fatalf("/api/nodes/health: %d %s", code, body)
	}
	// The edge refuses a session-less call to iam's API; with a session, iam's query guard answers.
	if code, body := call("GET", "/api/iam/v1/me?org=x", "", nil); code != http.StatusUnauthorized {
		t.Fatalf("REST query without a session: %d %s", code, body)
	}
	if code, body := call("GET", "/api/iam/v1/me?org=x", "", alice); code != http.StatusBadRequest {
		t.Fatalf("REST query with a session: %d %s", code, body)
	}
	// Login waits for the hub to have the new personal org, so the first hub call already succeeds.
	if code, body := call("GET", "/api/orgs/"+me.Msg.GetOrgs()[0].GetId()+"/vms", "", alice); code != http.StatusOK {
		t.Fatalf("personal org on the hub right after login: %d %s", code, body)
	}

	created, err := aliceIAM.CreateOrg(ctx, connect.NewRequest(&iamv1.CreateOrgRequest{Name: "acme"}))
	if err != nil {
		t.Fatal(err)
	}
	org := created.Msg.GetOrg().GetId()
	within(2*time.Second, "new org on the hub", func() (bool, string) {
		code, body := call("GET", "/api/orgs/"+org+"/vms", "", alice)
		return code == http.StatusOK, body
	})

	// Unclean paths under iam's prefixes reach no handler with the raw cookie: iam's mux redirects to the clean path or answers 404, and the edge refuses /internal.
	for _, c := range []struct{ method, path, clean string }{
		{"GET", "/auth/../api/orgs/" + org + "/vms", "/api/orgs/" + org + "/vms"},
		{"GET", "/auth/%2e%2e/api/orgs/" + org + "/vms", ""},
		{"POST", "/api/auth/../iam/v1/observatory.iam.v1.IAMService/GetMe", "/api/iam/v1/observatory.iam.v1.IAMService/GetMe"},
		{"GET", "/auth/../internal/iam/jwks", ""},
	} {
		resp := do(c.method, c.path, "{}", alice)
		raw, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if c.clean == "" {
			if resp.StatusCode != http.StatusNotFound {
				t.Fatalf("%s: %d %s", c.path, resp.StatusCode, raw)
			}
			continue
		}
		if resp.StatusCode != http.StatusTemporaryRedirect || resp.Header.Get("Location") != c.clean || strings.Contains(string(raw), "alice") {
			t.Fatalf("%s: %d %q %s", c.path, resp.StatusCode, resp.Header.Get("Location"), raw)
		}
	}

	// CLI login: an ID token for the CLI client, exchanged at iam through the edge.
	cliLogin := func(subject string) (string, string) {
		t.Helper()
		body, _ := json.Marshal(map[string]string{"idToken": idp.idToken(t, subject, "cli"), "accessToken": "at"})
		code, raw := call("POST", "/api/auth/cli-token", string(body), nil)
		var res struct{ Token string }
		if code != http.StatusCreated || json.Unmarshal([]byte(raw), &res) != nil {
			t.Fatalf("cli login: %d %s", code, raw)
		}
		sessions, err := iamv1connect.NewIAMServiceClient(client, base+"/api/iam/v1", rpc(bearerAuth(res.Token))).ListSessions(ctx, connect.NewRequest(&iamv1.ListSessionsRequest{}))
		if err != nil {
			t.Fatal(err)
		}
		for _, s := range sessions.Msg.GetSessions() {
			if s.GetCurrent() {
				return res.Token, s.GetId()
			}
		}
		t.Fatal("no current session")
		return "", ""
	}
	bobToken, _ := cliLogin("bob")
	bob := bearerAuth(bobToken)
	bobIAM := iamv1connect.NewIAMServiceClient(client, base+"/api/iam/v1", rpc(bob))
	inv, err := aliceIAM.CreateInvite(ctx, connect.NewRequest(&iamv1.CreateInviteRequest{Org: org, Role: "member"}))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := bobIAM.AcceptInvite(ctx, connect.NewRequest(&iamv1.AcceptInviteRequest{Token: inv.Msg.GetToken()})); err != nil {
		t.Fatal(err)
	}
	bobMe, err := bobIAM.GetMe(ctx, connect.NewRequest(&iamv1.GetMeRequest{}))
	if err != nil {
		t.Fatal(err)
	}
	bobID := bobMe.Msg.GetUser().GetId()
	within(5*time.Second, "bob's private env var", func() (bool, string) {
		code, body := call("POST", "/api/orgs/"+org+"/env", `{"name":"BOB_PRIVATE","value":"x","kind":"env","scope":"user"}`, bob)
		return code == http.StatusCreated, body
	})

	orgID, userID := uuid.MustParse(org), uuid.MustParse(bobID)
	node, enroll, err := hs.CreateNode(ctx, store.Node{OrgID: orgID, Name: "n1", CreatedBy: userID})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := hs.EnrollNode(ctx, enroll); err != nil {
		t.Fatal(err)
	}
	vm, err := hs.CreateVM(ctx, store.VM{OrgID: orgID, NodeID: node.ID, UserID: &userID, Name: "bobvm", CPUs: 1, MemoryMB: 128, DiskGB: 1, CreatedBy: userID})
	if err != nil {
		t.Fatal(err)
	}
	// REST answers 400 with failed precondition (code 9) here; 0.x answered 409.
	if code, body := call("DELETE", "/api/iam/v1/orgs/"+org+"/members/"+bobID, "", alice); code != http.StatusBadRequest || !strings.Contains(body, `"code":9,`) {
		t.Fatalf("REST remove a member that owns a VM: %d %s", code, body)
	}
	_, err = aliceIAM.DeleteMember(ctx, connect.NewRequest(&iamv1.DeleteMemberRequest{Org: org, User: bobID}))
	if connect.CodeOf(err) != connect.CodeFailedPrecondition {
		t.Fatalf("remove a member that owns a VM: %v", err)
	}
	if _, err := pool.Exec(ctx, "DELETE FROM vms WHERE id = $1", vm.ID); err != nil {
		t.Fatal(err)
	}
	if code, body := call("DELETE", "/api/iam/v1/orgs/"+org+"/members/"+me.Msg.GetUser().GetId(), "", bob); code != http.StatusForbidden {
		t.Fatalf("REST remove by a member: %d %s", code, body)
	}
	if _, err := aliceIAM.DeleteMember(ctx, connect.NewRequest(&iamv1.DeleteMemberRequest{Org: org, User: bobID})); err != nil {
		t.Fatal(err)
	}
	within(10*time.Second, "purge of bob's env var", func() (bool, string) {
		var n int
		if err := pool.QueryRow(ctx, "SELECT count(*) FROM env_vars WHERE org_id = $1 AND user_id = $2", orgID, userID).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n == 0, fmt.Sprintf("%d rows", n)
	})
	within(10*time.Second, "allowed and denied removals in the audit trail", func() (bool, string) {
		var allowed, denied int
		if err := pool.QueryRow(ctx, `SELECT count(*) FILTER (WHERE result = 'allowed'), count(*) FILTER (WHERE result = 'denied')
			FROM audit.events WHERE type = $1 AND org = $2`, iamstore.ActionMemberRemove, orgID).Scan(&allowed, &denied); err != nil {
			t.Fatal(err)
		}
		return allowed == 1 && denied >= 1, fmt.Sprintf("allowed %d, denied %d", allowed, denied)
	})

	// A registry credential is checked against iam on every use, so revoking its session refuses it at once, with or without the bus.
	registryToken := func(secret string) int {
		t.Helper()
		req, _ := http.NewRequest("GET", base+"/v2/token", nil)
		req.SetBasicAuth("observatory", secret)
		resp, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}
	revokeRefuses := func(what string) {
		t.Helper()
		token, session := cliLogin("bob")
		code, raw := call("POST", "/api/registry/credentials", "", bearerAuth(token))
		var cred struct{ Secret string }
		if code != http.StatusCreated || json.Unmarshal([]byte(raw), &cred) != nil {
			t.Fatalf("registry credential: %d %s", code, raw)
		}
		if code := registryToken(cred.Secret); code != http.StatusOK {
			t.Fatalf("%s: registry token before the revoke: %d", what, code)
		}
		if _, err := bobIAM.RevokeSession(ctx, connect.NewRequest(&iamv1.RevokeSessionRequest{Session: session})); err != nil {
			t.Fatal(err)
		}
		if code := registryToken(cred.Secret); code != http.StatusUnauthorized {
			t.Fatalf("%s: registry token after the revoke: %d", what, code)
		}
	}
	revokeRefuses("bus up")
	stopNATS()
	nats, err := url.Parse(natsURL)
	if err != nil {
		t.Fatal(err)
	}
	if conn, err := net.DialTimeout("tcp", nats.Host, time.Second); err == nil {
		conn.Close()
		t.Fatal("the bus still accepts connections after stopNATS")
	}
	revokeRefuses("bus down")
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
	s, _ := iamStore(t, pool)
	alice, err := s.Login(ctx, iamstore.Identity{Subject: "alice", Email: "alice@x.io", Name: "alice"})
	if err != nil {
		t.Fatal(err)
	}
	newSession := func(kind iamstore.SessionKind) (string, iamstore.Session) {
		tok, sess, err := s.CreateSession(ctx, alice.User.ID, kind, time.Hour, "test", "127.0.0.1")
		if err != nil {
			t.Fatal(err)
		}
		return tok, sess
	}
	web, _ := newSession(iamstore.SessionBrowser)
	cli, _ := newSession(iamstore.SessionCLI)
	other, otherSess := newSession(iamstore.SessionCLI)

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

// TestHubPolicyFromStream removes a member through iam's API: the hub must refuse them once iam's stream reaches it, with no reload of its own.
func TestHubPolicyFromStream(t *testing.T) {
	edgeLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	pool := dbtest.New(t)
	env := serveAllEnv(t, pool, edgeLn)
	ctx := t.Context()
	s, _ := iamStore(t, pool)
	alice, err := s.Login(ctx, iamstore.Identity{Subject: "alice", Email: "alice@x.io", Name: "alice"})
	if err != nil {
		t.Fatal(err)
	}
	bob, err := s.Login(ctx, iamstore.Identity{Subject: "bob", Email: "bob@x.io", Name: "bob"})
	if err != nil {
		t.Fatal(err)
	}
	org, err := s.CreateOrg(ctx, "acme", alice.User.ID)
	if err != nil {
		t.Fatal(err)
	}
	inv, _, err := s.CreateInvite(ctx, org.ID, iamstore.RoleMember, alice.User.ID, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.AcceptInvite(ctx, inv, bob.User.ID); err != nil {
		t.Fatal(err)
	}
	aliceToken, _, err := s.CreateSession(ctx, alice.User.ID, iamstore.SessionCLI, time.Hour, "test", "127.0.0.1")
	if err != nil {
		t.Fatal(err)
	}
	bobToken, _, err := s.CreateSession(ctx, bob.User.ID, iamstore.SessionCLI, time.Hour, "test", "127.0.0.1")
	if err != nil {
		t.Fatal(err)
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
	call := func(method, path, token string) int {
		req, _ := http.NewRequest(method, "http://"+edgeLn.Addr().String()+path, nil)
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("X-Observatory", "1")
		resp, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}
	vms := "/api/orgs/" + org.ID.String() + "/vms"
	if code := call("GET", vms, bobToken); code != http.StatusOK {
		t.Fatalf("bob as a member: %d", code)
	}
	if code := call("DELETE", "/api/iam/v1/orgs/"+org.ID.String()+"/members/"+bob.User.ID.String(), aliceToken); code != http.StatusOK {
		t.Fatalf("remove bob: %d", code)
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		code := call("GET", vms, bobToken)
		if code == http.StatusNotFound {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("bob after the removal: %d, want 404", code)
		}
		time.Sleep(20 * time.Millisecond)
	}
	if code := call("GET", vms, aliceToken); code != http.StatusOK {
		t.Fatalf("alice after bob's removal: %d", code)
	}
}

// TestServeAllCatalog: every service registers its catalog at start, the role editor's catalog lists them through the edge, and permissions stay as in 0.x.
func TestServeAllCatalog(t *testing.T) {
	edgeLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	pool := dbtest.New(t)
	env := serveAllEnv(t, pool, edgeLn)
	ctx := t.Context()
	s, _ := iamStore(t, pool)
	alice, err := s.Login(ctx, iamstore.Identity{Subject: "alice", Email: "alice@x.io", Name: "alice"})
	if err != nil {
		t.Fatal(err)
	}
	bob, err := s.Login(ctx, iamstore.Identity{Subject: "bob", Email: "bob@x.io", Name: "bob"})
	if err != nil {
		t.Fatal(err)
	}
	org, err := s.CreateOrg(ctx, "acme", alice.User.ID)
	if err != nil {
		t.Fatal(err)
	}
	inv, _, err := s.CreateInvite(ctx, org.ID, iamstore.RoleMember, alice.User.ID, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.AcceptInvite(ctx, inv, bob.User.ID); err != nil {
		t.Fatal(err)
	}
	aliceToken, _, err := s.CreateSession(ctx, alice.User.ID, iamstore.SessionCLI, time.Hour, "test", "127.0.0.1")
	if err != nil {
		t.Fatal(err)
	}
	bobToken, _, err := s.CreateSession(ctx, bob.User.ID, iamstore.SessionCLI, time.Hour, "test", "127.0.0.1")
	if err != nil {
		t.Fatal(err)
	}

	// Stored before start: owners hold fork only on their own VMs, so the API would refuse to grant it org-wide.
	forker, err := s.CreateRole(ctx, org.ID, authz.CustomRole{Name: "forker", Rules: []authz.Rule{{Effect: authz.Allow, Object: "org/vms/*", Action: "compute.instances.fork"}}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Bind(ctx, org.ID, authz.UserSubject(alice.User.ID), forker.ID); err != nil {
		t.Fatal(err)
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

	iam := iamv1connect.NewIAMServiceClient(&http.Client{Timeout: 30 * time.Second}, "http://"+edgeLn.Addr().String()+"/api/iam/v1")
	// The hub registers after iam is up, so its actions arrive a moment later.
	deadline := time.Now().Add(30 * time.Second)
	for {
		res, err := iam.ListCatalog(ctx, bearer(&iamv1.ListCatalogRequest{Org: org.ID.String()}, bobToken))
		entries := map[string]bool{}
		if err == nil {
			for _, e := range res.Msg.GetEntries() {
				entries[e.GetAction()] = true
			}
		}
		if entries["audit.settings.update"] && entries["compute.instances.create"] {
			if len(res.Msg.GetUnknownActions()) != 0 {
				t.Fatalf("unknown actions once the hub registered: %v", res.Msg.GetUnknownActions())
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("catalog through the edge: %v, %d entries", err, len(entries))
		}
		time.Sleep(50 * time.Millisecond)
	}

	golden, err := os.ReadFile("../../internal/authz/testdata/legacy_decisions.golden")
	if err != nil {
		t.Fatal(err)
	}
	var want []string
	for line := range strings.Lines(string(golden)) {
		if f := strings.Fields(line); len(f) == 5 && f[0] == "role" && f[1] == "member" && f[2] == "allow" {
			want = append(want, f[4])
		}
	}
	slices.Sort(want)
	want = slices.Compact(want)
	got, err := iam.GetOrg(ctx, bearer(&iamv1.GetOrgRequest{Org: org.ID.String()}, bobToken))
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(got.Msg.GetPermissions(), want) {
		t.Fatalf("member permissions:\n got %v\nwant %v", got.Msg.GetPermissions(), want)
	}

	got, err = iam.GetOrg(ctx, bearer(&iamv1.GetOrgRequest{Org: org.ID.String()}, aliceToken))
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(got.Msg.GetPermissions(), "compute.instances.fork") {
		t.Fatalf("owner's permissions with the fork role: %v", got.Msg.GetPermissions())
	}
}

// iamStore opens iam's schema in the hub's database, as the embedded iam will.
func iamStore(t *testing.T, pool *pgxpool.Pool) (*iamstore.Store, *pgxpool.Pool) {
	t.Helper()
	ipool, err := iamstore.Open(t.Context(), pool.Config().ConnString())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(ipool.Close)
	return iamstore.New(ipool), ipool
}

// quotaRig runs serve all with alice owning org acme and bob a member, both with CLI sessions, and returns an HTTP caller through the edge.
type quotaRig struct {
	t                *testing.T
	pool             *pgxpool.Pool
	org, alice, bob  uuid.UUID
	aliceTok, bobTok string
	base             string
	client           *http.Client
}

func newQuotaRig(t *testing.T) *quotaRig {
	t.Helper()
	edgeLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	pool := dbtest.New(t)
	env := serveAllEnv(t, pool, edgeLn)
	ctx := t.Context()
	s, _ := iamStore(t, pool)
	alice, err := s.Login(ctx, iamstore.Identity{Subject: "alice", Email: "alice@x.io", Name: "alice"})
	if err != nil {
		t.Fatal(err)
	}
	bob, err := s.Login(ctx, iamstore.Identity{Subject: "bob", Email: "bob@x.io", Name: "bob"})
	if err != nil {
		t.Fatal(err)
	}
	org, err := s.CreateOrg(ctx, "acme", alice.User.ID)
	if err != nil {
		t.Fatal(err)
	}
	inv, _, err := s.CreateInvite(ctx, org.ID, iamstore.RoleMember, alice.User.ID, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.AcceptInvite(ctx, inv, bob.User.ID); err != nil {
		t.Fatal(err)
	}
	r := &quotaRig{t: t, pool: pool, org: org.ID, alice: alice.User.ID, bob: bob.User.ID, base: "http://" + edgeLn.Addr().String(), client: &http.Client{Timeout: 30 * time.Second}}
	for _, u := range []struct {
		id  uuid.UUID
		tok *string
	}{{alice.User.ID, &r.aliceTok}, {bob.User.ID, &r.bobTok}} {
		if *u.tok, _, err = s.CreateSession(ctx, u.id, iamstore.SessionCLI, time.Hour, "test", "127.0.0.1"); err != nil {
			t.Fatal(err)
		}
	}
	runCtx, cancel := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() { done <- serveAll(runCtx, func(k string) string { return env[k] }, edgeLn) }()
	t.Cleanup(func() {
		cancel()
		if err := <-done; err != nil {
			t.Error(err)
		}
	})
	return r
}

func (r *quotaRig) call(method, path, token, body string) (int, string) {
	r.t.Helper()
	req, err := http.NewRequest(method, r.base+path, strings.NewReader(body))
	if err != nil {
		r.t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("X-Observatory", "1")
	resp, err := r.client.Do(req)
	if err != nil {
		r.t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(raw)
}

// within polls ok every 50 ms until it holds or d has passed.
func within(t *testing.T, d time.Duration, what string, ok func() (bool, string)) {
	t.Helper()
	deadline := time.Now().Add(d)
	for {
		done, state := ok()
		if done {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s not within %s: %s", what, d, state)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// setLimits sets scope's limits through the edge once quota answers.
func (r *quotaRig) setLimits(token, scope, limits string) (int, string) {
	r.t.Helper()
	var code int
	var body string
	within(r.t, 30*time.Second, "quota up", func() (bool, string) {
		code, body = r.call("PUT", "/api/quota/v1/orgs/"+r.org.String()+"/limits", token, `{"scope":"`+scope+`","limits":`+limits+`}`)
		return code != http.StatusServiceUnavailable && code != http.StatusBadGateway, body
	})
	return code, body
}

func TestServeAllQuotaLimits(t *testing.T) {
	r := newQuotaRig(t)
	if code, body := r.setLimits(r.aliceTok, "user:"+r.bob.String(), `{"vcpu":2}`); code != http.StatusOK {
		t.Fatalf("admin sets a member limit: %d %s", code, body)
	}
	if code, body := r.setLimits(r.bobTok, "user:"+r.bob.String(), `{"vcpu":8}`); code != http.StatusForbidden {
		t.Fatalf("member sets a limit: %d %s", code, body)
	}
	code, body := r.call("GET", "/api/quota/v1/orgs/"+r.org.String()+"/limits", r.bobTok, "")
	if code != http.StatusOK || compactJSON(t, body) != `{"org":{},"teams":{},"members":{"`+r.bob.String()+`":{"vcpu":2}}}` {
		t.Fatalf("member reads the limits: %d %s", code, body)
	}
}

// quotaGate runs quota on the listener serve all gives it and can stop and restart it there; while stopped, connections are refused at once.
type quotaGate struct {
	mu      sync.Mutex
	cur     *chanListener
	stop    context.CancelFunc
	stopped chan struct{}
	start   chan struct{}
}

func newQuotaGate(t *testing.T) *quotaGate {
	g := &quotaGate{start: make(chan struct{})}
	prev := runQuota
	t.Cleanup(func() { runQuota = prev })
	runQuota = g.run
	return g
}

func (g *quotaGate) run(ctx context.Context, cfg quota.Config, ln net.Listener) error {
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			g.mu.Lock()
			l := g.cur
			g.mu.Unlock()
			if l == nil {
				c.Close()
				continue
			}
			select {
			case l.conns <- c:
			case <-l.done:
				c.Close()
			}
		}
	}()
	defer ln.Close()
	for {
		l := &chanListener{addr: ln.Addr(), conns: make(chan net.Conn), done: make(chan struct{})}
		runCtx, stop := context.WithCancel(ctx)
		stopped := make(chan struct{})
		g.mu.Lock()
		g.cur, g.stop, g.stopped = l, stop, stopped
		g.mu.Unlock()
		err := quota.Run(runCtx, cfg, l)
		g.mu.Lock()
		g.cur = nil
		g.mu.Unlock()
		close(stopped)
		stop()
		if err != nil || ctx.Err() != nil {
			return err
		}
		select {
		case <-g.start:
		case <-ctx.Done():
			return nil
		}
	}
}

// Stop stops quota and waits until it has returned.
func (g *quotaGate) Stop() {
	g.mu.Lock()
	stop, stopped := g.stop, g.stopped
	g.mu.Unlock()
	stop()
	<-stopped
}

func (g *quotaGate) Start() { g.start <- struct{}{} }

type chanListener struct {
	addr  net.Addr
	conns chan net.Conn
	done  chan struct{}
	once  sync.Once
}

func (l *chanListener) Accept() (net.Conn, error) {
	select {
	case c := <-l.conns:
		return c, nil
	case <-l.done:
		return nil, net.ErrClosed
	}
}

func (l *chanListener) Close() error {
	l.once.Do(func() { close(l.done) })
	return nil
}

func (l *chanListener) Addr() net.Addr { return l.addr }

// TestServeAllQuotaConverges: a VM deleted while quota is down frees its usage through the deleted event once quota is back, and no create is admitted unchecked meanwhile.
func TestServeAllQuotaConverges(t *testing.T) {
	gate := newQuotaGate(t)
	r := newQuotaRig(t)
	ctx := t.Context()
	hs := storetest.New(t, r.pool)
	node, enroll, err := hs.CreateNode(ctx, store.Node{OrgID: r.org, Name: "n1", CreatedBy: r.alice})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := hs.EnrollNode(ctx, enroll); err != nil {
		t.Fatal(err)
	}
	if code, body := r.setLimits(r.aliceTok, "user:"+r.alice.String(), `{"vmCount":1}`); code != http.StatusOK {
		t.Fatalf("set limit: %d %s", code, body)
	}
	vms := "/api/orgs/" + r.org.String() + "/vms"
	create := func(name string) (int, string) {
		return r.call("POST", vms, r.aliceTok, `{"name":"`+name+`","owner":{"kind":"user"},"nodeId":"`+node.ID.String()+`","cpus":1,"memoryMB":512,"diskGB":10}`)
	}
	code, body := create("a")
	if code != http.StatusCreated {
		t.Fatalf("create: %d %s", code, body)
	}
	var vm struct{ ID string }
	if err := json.Unmarshal([]byte(body), &vm); err != nil {
		t.Fatal(err)
	}
	// The created and committed events have landed: the allocation is live and no hold is left.
	usage := func() (bool, string) {
		var live, holds int
		if err := r.pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM quota.allocations WHERE resource = $1 AND NOT deleted),
			(SELECT count(*) FROM quota.reservations)`, "vm/"+vm.ID).Scan(&live, &holds); err != nil {
			t.Fatal(err)
		}
		return live == 1 && holds == 0, fmt.Sprintf("live %d, holds %d", live, holds)
	}
	within(t, 10*time.Second, "allocation from the created event", usage)
	if code, body := create("b"); code != http.StatusConflict {
		t.Fatalf("create at the limit: %d %s", code, body)
	}

	gate.Stop()
	if code, body := r.call("DELETE", vms+"/"+vm.ID, r.aliceTok, ""); code != http.StatusNoContent {
		t.Fatalf("delete while quota is down: %d %s", code, body)
	}
	if code, body := create("b"); code != http.StatusServiceUnavailable || !strings.Contains(body, "quota service unavailable") {
		t.Fatalf("create while quota is down: %d %s", code, body)
	}
	if ok, state := usage(); !ok {
		t.Fatalf("usage changed while quota was down: %s", state)
	}

	gate.Start()
	within(t, 10*time.Second, "usage freed by the deleted event", func() (bool, string) {
		var deleted bool
		if err := r.pool.QueryRow(ctx, "SELECT deleted FROM quota.allocations WHERE resource = $1", "vm/"+vm.ID).Scan(&deleted); err != nil {
			t.Fatal(err)
		}
		return deleted, "allocation still live"
	})
	if code, body := create("b"); code != http.StatusCreated {
		t.Fatalf("create at the old limit after quota is back: %d %s", code, body)
	}
}

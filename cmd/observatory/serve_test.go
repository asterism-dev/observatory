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

	"github.com/asterism-dev/observatory/internal/db/dbtest"
	"github.com/asterism-dev/observatory/internal/edge"
	"github.com/asterism-dev/observatory/internal/store"
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
		!strings.Contains(stderr.String(), "OBSERVATORY_EDGE_TOKEN") || !strings.Contains(stderr.String(), "OBSERVATORY_EDGE_HUB_URL") {
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

// TestServeAllRecordsClientIPThroughEdge runs the real edge and hub: the client reaches the edge over ::1 while the edge reaches the hub over 127.0.0.1, so an audited ::1 proves the hub trusts the edge's header.
func TestServeAllRecordsClientIPThroughEdge(t *testing.T) {
	edgeLn, err := net.Listen("tcp", "[::1]:0")
	if err != nil {
		t.Skipf("no IPv6 loopback: %v", err)
	}
	pool := dbtest.New(t)
	idp := httptest.NewServer(nil)
	defer idp.Close()
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

	rows, err := pool.Query(t.Context(), "SELECT ip FROM audit_log WHERE action = $1 ORDER BY id", store.ActionAuthLogin)
	if err != nil {
		t.Fatal(err)
	}
	var ips []string
	for rows.Next() {
		var ip string
		if err := rows.Scan(&ip); err != nil {
			t.Fatal(err)
		}
		ips = append(ips, ip)
	}
	if len(ips) != 2 || ips[0] != "::1" || ips[1] != "::1" {
		t.Fatalf("audited IPs %v, want the edge's ::1 twice", ips)
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

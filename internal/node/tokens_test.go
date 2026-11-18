package node

import (
	"bufio"
	"bytes"
	"context"
	"io"
	"log"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/asterism-dev/observatory/internal/proto"
)

const dummyGitHubToken = "ghp_dummy0000000000000000"

func tokenSocket(t *testing.T, vmID string, lookup TokenLookup) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "tok")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	uds := filepath.Join(dir, "v.sock_1051")
	l, err := net.Listen("unix", uds)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() { ServeTokens(l, vmID, lookup); close(done) }()
	t.Cleanup(func() { l.Close(); <-done })
	return uds
}

func askToken(t *testing.T, uds, line string) string {
	t.Helper()
	c, err := net.Dial("unix", uds)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	c.SetDeadline(time.Now().Add(5 * time.Second))
	if _, err := io.WriteString(c, line); err != nil {
		t.Fatal(err)
	}
	b, err := io.ReadAll(c)
	if err != nil {
		t.Fatal(err)
	}
	return strings.TrimSpace(string(b))
}

func TestServeTokensProtocol(t *testing.T) {
	exp := time.Date(2026, 11, 18, 12, 0, 0, 0, time.UTC)
	uds := tokenSocket(t, "x", func(vmID, kind string) (proto.IntegrationToken, string) {
		switch kind {
		case "github":
			return proto.IntegrationToken{Kind: kind, Token: "obsph_x"}, ""
		case "claude":
			return proto.IntegrationToken{Kind: kind, Token: "obsph_c", ExpiresAt: &exp}, ""
		}
		return proto.IntegrationToken{}, TokenNotConnected
	})
	notConnected := `{"ok":false,"error":"not-connected"}`
	for _, c := range []struct{ req, want string }{
		{`{"op":"token","kind":"github"}` + "\n", `{"ok":true,"token":"obsph_x","expiresAt":null}`},
		{`{"op":"token","kind":"claude"}` + "\n", `{"ok":true,"token":"obsph_c","expiresAt":"2026-11-18T12:00:00Z"}`},
		{`{"op":"token","kind":"linear"}` + "\n", notConnected},
		{`{"op":"x"}` + "\n", notConnected},
		{"not json\n", notConnected},
		{strings.Repeat("a", 5<<10) + "\n", notConnected},
	} {
		if got := askToken(t, uds, c.req); got != c.want {
			t.Errorf("%.40q → %s, want %s", c.req, got, c.want)
		}
	}

	silent, err := net.Dial("unix", uds)
	if err != nil {
		t.Fatal(err)
	}
	defer silent.Close()
	start := time.Now()
	silent.SetReadDeadline(time.Now().Add(5 * time.Second))
	if _, err := silent.Read(make([]byte, 1)); err != io.EOF {
		t.Fatalf("silent client: %v", err)
	}
	if d := time.Since(start); d < 1500*time.Millisecond || d > 3*time.Second {
		t.Fatalf("silent client closed after %v", d)
	}

	var held []net.Conn
	for range 16 {
		c, err := net.Dial("unix", uds)
		if err != nil {
			t.Fatal(err)
		}
		held = append(held, c)
		defer c.Close()
	}
	// Gives the server time to accept all 16.
	time.Sleep(100 * time.Millisecond)
	extra, err := net.Dial("unix", uds)
	if err != nil {
		t.Fatal(err)
	}
	defer extra.Close()
	extra.SetReadDeadline(time.Now().Add(time.Second))
	if n, err := extra.Read(make([]byte, 64)); err != io.EOF || n != 0 {
		t.Fatalf("17th connection: n=%d err=%v", n, err)
	}
	// Held connections are still served.
	io.WriteString(held[0], `{"op":"token","kind":"github"}`+"\n")
	held[0].SetReadDeadline(time.Now().Add(time.Second))
	line, _ := bufio.NewReader(held[0]).ReadString('\n')
	if !strings.Contains(line, "obsph_x") {
		t.Fatalf("held connection got %q", line)
	}
}

func TestServeTokensPerVM(t *testing.T) {
	lookup := func(vmID, kind string) (proto.IntegrationToken, string) {
		return proto.IntegrationToken{Kind: kind, Token: "obsph_" + vmID}, ""
	}
	x, y := tokenSocket(t, "X", lookup), tokenSocket(t, "Y", lookup)
	for range 3 {
		if got := askToken(t, x, `{"op":"token","kind":"github"}`+"\n"); !strings.Contains(got, `"obsph_X"`) {
			t.Fatalf("X got %s", got)
		}
		if got := askToken(t, y, `{"op":"token","kind":"github"}`+"\n"); !strings.Contains(got, `"obsph_Y"`) {
			t.Fatalf("Y got %s", got)
		}
	}
}

func withGitHub(d proto.DesiredVM) proto.DesiredVM {
	d.Boot = &proto.Boot{}
	d.Integrations = []proto.IntegrationToken{{Kind: "github", Token: dummyGitHubToken}}
	return d
}

func wantToken(t *testing.T, a *Agent, want string) {
	t.Helper()
	eventually(t, "token "+want, func() bool {
		tok, res := a.integrationToken("a", "github")
		if want == "ok" {
			return res == "" && tok.Token == dummyGitHubToken
		}
		return res == want
	})
}

func TestIntegrationTokenLookup(t *testing.T) {
	a, _ := newReconcileAgent(t)
	wantToken(t, a, TokenUnavailable)
	a.reconcile(context.Background(), 0, []proto.DesiredVM{withGitHub(dv("a", "a", proto.PowerRunning, 1))})
	waitState(t, a, "a", proto.StateRunning, 1)
	wantToken(t, a, "ok")
	without := dv("a", "a", proto.PowerRunning, 1)
	without.Boot = &proto.Boot{}
	a.reconcile(context.Background(), 0, []proto.DesiredVM{without})
	wantToken(t, a, TokenNotConnected)
	a.reconcile(context.Background(), 0, []proto.DesiredVM{withGitHub(dv("a", "a", proto.PowerRunning, 1))})
	wantToken(t, a, "ok")

	a.stopAll()
	b := &Agent{NodeID: "n1", VMsDir: a.VMsDir, Provider: newFakeProvider(), vms: map[string]*vmEntry{}}
	t.Cleanup(b.stopAll)
	if err := b.restoreAll(context.Background()); err != nil {
		t.Fatal(err)
	}
	waitState(t, b, "a", proto.StateRunning, 1)
	wantToken(t, b, TokenUnavailable)
	b.reconcile(context.Background(), 0, []proto.DesiredVM{withGitHub(dv("a", "a", proto.PowerRunning, 1))})
	wantToken(t, b, "ok")
	b.reconcile(context.Background(), 0, nil)
	wantToken(t, b, TokenUnavailable)
}

func TestIntegrationsNeverPersisted(t *testing.T) {
	dir := t.TempDir()
	f := cachedVM("a", "a")
	f.Boot = &proto.Boot{}
	f.Integrations = []proto.IntegrationToken{{Kind: "github", Token: dummyGitHubToken}}
	if err := SaveVM(dir, f); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(filepath.Join(dir, "a", "vm.json"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), dummyGitHubToken) {
		t.Fatalf("vm.json = %s", b)
	}
	for name, content := range GuestSeedFiles(f, PrimaryNet{}) {
		if bytes.Contains(content, []byte(dummyGitHubToken)) {
			t.Fatalf("seed file %s holds the token", name)
		}
	}
}

type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string { s.mu.Lock(); defer s.mu.Unlock(); return s.b.String() }

func TestTokenLogNoSecrets(t *testing.T) {
	var buf syncBuffer
	log.SetOutput(&buf)
	t.Cleanup(func() { log.SetOutput(os.Stderr) })
	uds := tokenSocket(t, "vm1", func(vmID, kind string) (proto.IntegrationToken, string) {
		if kind == "github" {
			return proto.IntegrationToken{Kind: kind, Token: dummyGitHubToken}, ""
		}
		return proto.IntegrationToken{}, TokenNotConnected
	})
	askToken(t, uds, `{"op":"token","kind":"github"}`+"\n")
	askToken(t, uds, `{"op":"token","kind":"Linear\n`+dummyGitHubToken+`"}`+"\n")
	askToken(t, uds, "garbage "+dummyGitHubToken+"\n")
	eventually(t, "three log lines", func() bool { return strings.Count(buf.String(), "token vm=vm1") == 3 })
	out := buf.String()
	if !strings.Contains(out, "token vm=vm1 kind=github result=ok") || !strings.Contains(out, "kind=? result=not-connected") {
		t.Fatalf("log = %s", out)
	}
	if strings.Contains(out, dummyGitHubToken) {
		t.Fatalf("token logged: %s", out)
	}
}

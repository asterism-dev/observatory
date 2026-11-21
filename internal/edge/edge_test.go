package edge

import (
	"bufio"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/coder/websocket"
)

const (
	testToken        = "0123456789abcdef0123456789abcdef"
	testServiceToken = "fedcba9876543210fedcba9876543210"
)

var assets = fstest.MapFS{
	"index.html":  {Data: []byte("<html>spa</html>")},
	"assets/a.js": {Data: []byte("console.log(1)")},
	"fonts.v1/x":  {Data: []byte("font")},
}

type seen struct {
	path, rawPath, host, clientIP, edge string
	header                              http.Header
}

func recorder(t *testing.T, name string) (*httptest.Server, chan seen) {
	t.Helper()
	ch := make(chan seen, 16)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ch <- seen{r.URL.Path, r.URL.RawPath, r.Host, r.Header.Get(HeaderClientIP), r.Header.Get(HeaderEdge), r.Header.Clone()}
		_, _ = io.WriteString(w, name)
	}))
	t.Cleanup(srv.Close)
	return srv, ch
}

func newEdge(t *testing.T, hubURL string, services map[string]string) *httptest.Server {
	t.Helper()
	h, err := New(Config{HubURL: hubURL, Token: testToken, ServiceToken: testServiceToken, Services: services}, assets)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return srv
}

func get(t *testing.T, req *http.Request) (int, string) {
	t.Helper()
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

func TestEdgeRoutesToHub(t *testing.T) {
	hub, ch := recorder(t, "hub")
	edge := newEdge(t, hub.URL, nil)
	for _, p := range []string{"/api/me", "/auth/login", "/v2/", "/otlp/v1/logs", "/node/install.sh", "/grafana/api/x%2Fy"} {
		req, _ := http.NewRequest("GET", edge.URL+p, nil)
		code, body := get(t, req)
		if code != 200 || body != "hub" {
			t.Fatalf("%s: %d %q", p, code, body)
		}
		s := <-ch
		got := s.path
		if s.rawPath != "" {
			got = s.rawPath
		}
		if got != p {
			t.Fatalf("hub saw path %q, want %q", got, p)
		}
		if s.host != strings.TrimPrefix(edge.URL, "http://") {
			t.Fatalf("hub saw host %q, want the edge's", s.host)
		}
	}
}

func TestEdgeRoutesToService(t *testing.T) {
	hub, _, _ := issuingHub(t, http.StatusOK)
	iam, ch := recorder(t, "iam")
	edge := newEdge(t, hub.URL, map[string]string{"iam": iam.URL})
	req, _ := http.NewRequest("GET", edge.URL+"/api/iam/v1/x", nil)
	req.Header.Set("Authorization", "Bearer "+validSession)
	if code, body := get(t, req); code != 200 || body != "iam" {
		t.Fatalf("%d %q", code, body)
	}
	if s := <-ch; s.path != "/api/iam/v1/x" {
		t.Fatalf("iam saw %q", s.path)
	}
	req, _ = http.NewRequest("GET", edge.URL+"/api/iam/x", nil)
	if _, body := get(t, req); body != "hub" {
		t.Fatalf("non-v1 path went to %q", body)
	}
}

func TestEdgeBlocksInternal(t *testing.T) {
	hub, ch := recorder(t, "hub")
	edge := newEdge(t, hub.URL, nil)
	for _, p := range []string{"/internal/registry/events", "/internal", "/api/../internal/x", "/grafana/%2e%2e/internal/x", "/internal%2Fx", "//internal/x"} {
		req, _ := http.NewRequest("GET", edge.URL+p, nil)
		if !strings.HasPrefix(p, "//") { // an Opaque of //x would be sent as an absolute URL
			req.URL.Opaque = p // send the path verbatim, unclean
		}
		if code, _ := get(t, req); code != http.StatusNotFound {
			t.Fatalf("%s: got %d, want 404", p, code)
		}
	}
	select {
	case s := <-ch:
		t.Fatalf("hub was reached: %+v", s)
	default:
	}
}

func TestEdgeStampsClientIP(t *testing.T) {
	hub, ch := recorder(t, "hub")
	edge := newEdge(t, hub.URL, nil)
	req, _ := http.NewRequest("GET", edge.URL+"/api/me", nil)
	req.Header.Set(HeaderClientIP, "6.6.6.6")
	req.Header.Set(HeaderEdge, "spoofed")
	get(t, req)
	s := <-ch
	if s.clientIP != "127.0.0.1" || s.edge != testToken {
		t.Fatalf("upstream saw client ip %q, edge %q", s.clientIP, s.edge)
	}
}

func TestEdgeTrustedHeadersCannotBeSpoofed(t *testing.T) {
	hub, ch := recorder(t, "hub")
	edge := newEdge(t, hub.URL, nil)
	for name, set := range map[string]func(http.Header){
		"connection hop-by-hop": func(h http.Header) {
			h.Set("Connection", HeaderEdge+", "+HeaderClientIP)
		},
		"lower case": func(h http.Header) {
			h["x-observatory-client-ip"] = []string{"6.6.6.6"}
			h["x-observatory-edge"] = []string{"spoofed"}
		},
		"multi value": func(h http.Header) {
			h[HeaderClientIP] = []string{"6.6.6.6", "7.7.7.7"}
			h[HeaderEdge] = []string{"spoofed", testToken}
		},
		"forwarded for": func(h http.Header) {
			h.Set("X-Forwarded-For", "6.6.6.6")
		},
	} {
		req, _ := http.NewRequest("GET", edge.URL+"/api/me", nil)
		set(req.Header)
		get(t, req)
		s := <-ch
		if ips, edges := s.header.Values(HeaderClientIP), s.header.Values(HeaderEdge); len(ips) != 1 || ips[0] != "127.0.0.1" || len(edges) != 1 || edges[0] != testToken {
			t.Errorf("%s: upstream saw client ip %q, edge %q", name, ips, edges)
		}
		if xff := s.header.Values("X-Forwarded-For"); len(xff) != 0 {
			t.Errorf("%s: upstream saw X-Forwarded-For %q", name, xff)
		}
	}
}

func TestEdgeSSEStreams(t *testing.T) {
	release := make(chan struct{})
	hub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: first\n\n")
		w.(http.Flusher).Flush()
		<-release
		_, _ = io.WriteString(w, "data: second\n\n")
	}))
	t.Cleanup(hub.Close)
	t.Cleanup(func() {
		select {
		case <-release:
		default:
			close(release)
		}
	})
	edge := newEdge(t, hub.URL, nil)
	resp, err := http.Get(edge.URL + "/api/builds/1/logs")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	lines := make(chan string, 1)
	go func() {
		line, _ := bufio.NewReader(resp.Body).ReadString('\n')
		lines <- line
	}()
	select {
	case line := <-lines:
		if line != "data: first\n" {
			t.Fatalf("got %q", line)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("first event not flushed before the response ended")
	}
	close(release)
}

func TestEdgeWebSocketUpgrade(t *testing.T) {
	hub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer c.CloseNow()
		typ, b, err := c.Read(r.Context())
		if err != nil {
			return
		}
		_ = c.Write(r.Context(), typ, b)
	}))
	t.Cleanup(hub.Close)
	edge := newEdge(t, hub.URL, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	c, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(edge.URL, "http")+"/api/vms/1/console", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer c.CloseNow()
	if err := c.Write(ctx, websocket.MessageText, []byte("ping")); err != nil {
		t.Fatal(err)
	}
	_, b, err := c.Read(ctx)
	if err != nil || string(b) != "ping" {
		t.Fatalf("echo %q, %v", b, err)
	}
}

func TestEdgeServesSPA(t *testing.T) {
	hub, _ := recorder(t, "hub")
	edge := newEdge(t, hub.URL, nil)
	// Directories without an index fall back to the app instead of a listing.
	for p, want := range map[string]string{"/vms/x": "<html>spa</html>", "/assets/a.js": "console.log(1)", "/": "<html>spa</html>",
		"/assets/": "<html>spa</html>", "/assets": "<html>spa</html>", "/fonts.v1/": "<html>spa</html>"} {
		req, _ := http.NewRequest("GET", edge.URL+p, nil)
		if code, body := get(t, req); code != 200 || body != want {
			t.Fatalf("%s: %d %q", p, code, body)
		}
	}
}

func TestNewRejectsBadConfig(t *testing.T) {
	for name, cfg := range map[string]Config{
		"missing hub":         {Token: testToken, ServiceToken: testServiceToken},
		"short token":         {HubURL: "http://hub", Token: "short", ServiceToken: testServiceToken},
		"short service token": {HubURL: "http://hub", Token: testToken, ServiceToken: "short"},
		"bad service":         {HubURL: "http://hub", Token: testToken, ServiceToken: testServiceToken, Services: map[string]string{"iam": "::"}},
	} {
		if _, err := New(cfg, assets); err == nil {
			t.Fatalf("%s: want error", name)
		}
	}
}

func TestParseServices(t *testing.T) {
	got, err := ParseServices(" audit=http://audit:8092 , iam=http://iam:8093/")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got["audit"] != "http://audit:8092" || got["iam"] != "http://iam:8093/" {
		t.Fatalf("got %v", got)
	}
	if got, err := ParseServices(""); err != nil || len(got) != 0 {
		t.Fatalf("empty: %v, %v", got, err)
	}
	for _, env := range []string{"audit", "=http://a", "audit=", "audit=audit:8092", "Audit=http://a", "a/b=http://a", "audit=http://a,audit=http://b"} {
		if _, err := ParseServices(env); err == nil {
			t.Errorf("%q accepted", env)
		}
	}
}

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

const testToken = "0123456789abcdef0123456789abcdef"

var assets = fstest.MapFS{
	"index.html":  {Data: []byte("<html>spa</html>")},
	"assets/a.js": {Data: []byte("console.log(1)")},
}

type seen struct {
	path, rawPath, host, clientIP, edge string
}

func recorder(t *testing.T, name string) (*httptest.Server, chan seen) {
	t.Helper()
	ch := make(chan seen, 16)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ch <- seen{r.URL.Path, r.URL.RawPath, r.Host, r.Header.Get(HeaderClientIP), r.Header.Get(HeaderEdge)}
		_, _ = io.WriteString(w, name)
	}))
	t.Cleanup(srv.Close)
	return srv, ch
}

func newEdge(t *testing.T, hubURL string, services map[string]string) *httptest.Server {
	t.Helper()
	h, err := New(Config{HubURL: hubURL, Token: testToken, Services: services}, assets)
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
	hub, _ := recorder(t, "hub")
	iam, ch := recorder(t, "iam")
	edge := newEdge(t, hub.URL, map[string]string{"iam": iam.URL})
	req, _ := http.NewRequest("GET", edge.URL+"/api/iam/v1/x", nil)
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
	for _, p := range []string{"/internal/registry/events", "/internal", "/api/../internal/x", "/grafana/%2e%2e/internal/x"} {
		req, _ := http.NewRequest("GET", edge.URL+p, nil)
		req.URL.Opaque = p // send the path verbatim, unclean
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
	for p, want := range map[string]string{"/vms/x": "<html>spa</html>", "/assets/a.js": "console.log(1)", "/": "<html>spa</html>"} {
		req, _ := http.NewRequest("GET", edge.URL+p, nil)
		if code, body := get(t, req); code != 200 || body != want {
			t.Fatalf("%s: %d %q", p, code, body)
		}
	}
}

func TestNewRejectsBadConfig(t *testing.T) {
	for name, cfg := range map[string]Config{
		"missing hub": {Token: testToken},
		"short token": {HubURL: "http://hub", Token: "short"},
		"bad service": {HubURL: "http://hub", Token: testToken, Services: map[string]string{"iam": "::"}},
	} {
		if _, err := New(cfg, assets); err == nil {
			t.Fatalf("%s: want error", name)
		}
	}
}

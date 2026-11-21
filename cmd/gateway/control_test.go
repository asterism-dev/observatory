package main

import (
	"bufio"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/asterism-dev/observatory/internal/proto"
)

func controlFixture(t *testing.T, h http.HandlerFunc) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	var calls atomic.Int32
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		h(w, r)
	}))
	t.Cleanup(up.Close)
	u, _ := url.Parse(up.URL)
	gw := httptest.NewServer(controlProxy(u, "gw-token"))
	t.Cleanup(gw.Close)
	return gw, &calls
}

func TestControlProxyStampsPeer(t *testing.T) {
	var peer, gw string
	srv, _ := controlFixture(t, func(w http.ResponseWriter, r *http.Request) {
		peer, gw = r.Header.Get(proto.HeaderUnderlayPeer), r.Header.Get(proto.HeaderGateway)
	})
	req, _ := http.NewRequest("GET", srv.URL+"/api/nodes/health", nil)
	req.Header.Set(proto.HeaderUnderlayPeer, "10.198.0.99")
	req.Header.Set(proto.HeaderGateway, "forged")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if peer != "127.0.0.1" || gw != "gw-token" {
		t.Errorf("upstream saw peer %q gateway %q", peer, gw)
	}
}

func TestControlProxyOnlyNodePaths(t *testing.T) {
	srv, calls := controlFixture(t, func(http.ResponseWriter, *http.Request) {})
	for _, p := range []string{"/api/orgs", "/api/nodes/connect/x", "/api/nodes/healthz", "/"} {
		resp, err := http.Get(srv.URL + p)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != 404 {
			t.Errorf("%s: status %d", p, resp.StatusCode)
		}
	}
	if calls.Load() != 0 {
		t.Errorf("upstream called %d times", calls.Load())
	}
}

func TestControlProxyUpgrade(t *testing.T) {
	var upgrade string
	srv, _ := controlFixture(t, func(w http.ResponseWriter, r *http.Request) {
		upgrade = r.Header.Get("Upgrade")
		w.Header().Set("Connection", "Upgrade")
		w.Header().Set("Upgrade", "websocket")
		w.WriteHeader(http.StatusSwitchingProtocols)
	})
	conn, err := net.Dial("tcp", strings.TrimPrefix(srv.URL, "http://"))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	conn.Write([]byte("GET /api/nodes/connect HTTP/1.1\r\nHost: x\r\nConnection: Upgrade\r\nUpgrade: websocket\r\n\r\n"))
	line, _ := bufio.NewReader(conn).ReadString('\n')
	if !strings.Contains(line, "101") || upgrade != "websocket" {
		t.Errorf("status line %q, upstream upgrade %q", line, upgrade)
	}
}

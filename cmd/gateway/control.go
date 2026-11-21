package main

import (
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"time"

	"github.com/asterism-dev/observatory/internal/proto"
)

// controlProxy forwards node control traffic to the hub and stamps the WireGuard peer's address; the hub trusts the stamp only with the gateway token.
func controlProxy(hub *url.URL, gatewayToken string) http.Handler {
	rp := &httputil.ReverseProxy{Rewrite: func(r *httputil.ProxyRequest) {
		r.SetURL(hub)
		r.Out.Header.Del(proto.HeaderUnderlayPeer)
		r.Out.Header.Del(proto.HeaderGateway)
		host, _, _ := net.SplitHostPort(r.In.RemoteAddr)
		r.Out.Header.Set(proto.HeaderUnderlayPeer, host)
		r.Out.Header.Set(proto.HeaderGateway, gatewayToken)
	}}
	mux := http.NewServeMux()
	mux.Handle("/api/nodes/connect", rp)
	mux.Handle("/api/nodes/health", rp)
	return mux
}

func newControlServer(hub *url.URL, gatewayToken string) *http.Server {
	return &http.Server{Handler: controlProxy(hub, gatewayToken), ReadHeaderTimeout: 10 * time.Second}
}

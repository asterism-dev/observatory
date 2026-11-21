package edge

import (
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
)

// proxy forwards to target unchanged except for the trusted headers, which only the edge may set.
func proxy(target *url.URL, token string, withIdentity bool) http.Handler {
	return &httputil.ReverseProxy{
		Rewrite: func(r *httputil.ProxyRequest) {
			r.SetURL(target)
			// Upstreams build public URLs and cookies from Host.
			r.Out.Host = r.In.Host
			r.Out.Header.Del(HeaderClientIP)
			r.Out.Header.Del(HeaderEdge)
			host, _, err := net.SplitHostPort(r.In.RemoteAddr)
			if err != nil {
				host = r.In.RemoteAddr
			}
			r.Out.Header.Set(HeaderClientIP, host)
			r.Out.Header.Set(HeaderEdge, token)
			if withIdentity {
				r.Out.Header.Del("Cookie")
				r.Out.Header.Del("Authorization")
				// Set here rather than on the inbound request so a client's Connection header cannot strip it.
				if jwt, ok := r.In.Context().Value(jwtKey{}).(string); ok {
					r.Out.Header.Set("Authorization", "Bearer "+jwt)
				}
			}
		},
		FlushInterval: -1,
	}
}

package edge

import (
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
)

// proxy forwards to target unchanged except for the trusted headers, which only the edge may set.
func proxy(target *url.URL, token string, withIdentity bool) http.Handler {
	return &httputil.ReverseProxy{
		Rewrite: func(r *httputil.ProxyRequest) {
			r.SetURL(target)
			// Upstreams build public URLs and cookies from Host.
			r.Out.Host = r.In.Host
			r.Out.Header.Del(HeaderEdge)
			host, _, err := net.SplitHostPort(r.In.RemoteAddr)
			if err != nil {
				host = r.In.RemoteAddr
			}
			// Set, never added, so a client-sent copy cannot pass.
			r.Out.Header.Set(HeaderClientIP, host)
			if withIdentity {
				// Services learn who the user is from the JWT alone; the edge token is for the hub only.
				r.Out.Header.Del("Cookie")
				r.Out.Header.Del("Authorization")
				// Set here rather than on the inbound request so a client's Connection header cannot strip it.
				if jwt, ok := r.In.Context().Value(jwtKey{}).(string); ok {
					r.Out.Header.Set("Authorization", "Bearer "+jwt)
				}
				return
			}
			if hid, ok := r.In.Context().Value(hubIdentityKey{}).(hubIdentity); ok {
				// The hub authenticates users by JWT alone and must never see the session token.
				dropSessionCookie(r.Out.Header)
				if hid.swapped {
					r.Out.Header.Del("Authorization")
				}
				if hid.jwt != "" {
					r.Out.Header.Set("Authorization", "Bearer "+hid.jwt)
				}
			}
			r.Out.Header.Set(HeaderEdge, token)
		},
		FlushInterval: -1,
	}
}

func dropSessionCookie(h http.Header) {
	cookies := (&http.Request{Header: http.Header{"Cookie": h.Values("Cookie")}}).Cookies()
	h.Del("Cookie")
	var kept []string
	for _, c := range cookies {
		if c.Name != sessionCookie {
			kept = append(kept, c.String())
		}
	}
	if len(kept) > 0 {
		h.Set("Cookie", strings.Join(kept, "; "))
	}
}

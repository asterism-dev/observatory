// Package clientip resolves the client IP of a request that may have come through the edge.
package clientip

import (
	"crypto/subtle"
	"net"
	"net/http"
)

const (
	HeaderClientIP = "X-Observatory-Client-IP"
	HeaderEdge     = "X-Observatory-Edge"
)

// From trusts HeaderClientIP only when HeaderEdge carries edgeToken; otherwise the peer address is the client.
func From(r *http.Request, edgeToken string) string {
	if ip := r.Header.Get(HeaderClientIP); ip != "" && edgeToken != "" &&
		subtle.ConstantTimeCompare([]byte(r.Header.Get(HeaderEdge)), []byte(edgeToken)) == 1 {
		return ip
	}
	if host, _, err := net.SplitHostPort(r.RemoteAddr); err == nil {
		return host
	}
	return r.RemoteAddr
}

package clientip

import (
	"net/http/httptest"
	"testing"
)

func TestClientIPFrom(t *testing.T) {
	const token = "0123456789abcdef0123456789abcdef"
	cases := []struct {
		name, edge, headerIP, edgeToken, want string
	}{
		{"missing token", "", "1.2.3.4", token, "10.0.0.1"},
		{"wrong token", "nope", "1.2.3.4", token, "10.0.0.1"},
		{"right token", token, "1.2.3.4", token, "1.2.3.4"},
		{"right token without header ip", token, "", token, "10.0.0.1"},
		{"no edge token configured", "", "1.2.3.4", "", "10.0.0.1"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := httptest.NewRequest("GET", "/", nil)
			r.RemoteAddr = "10.0.0.1:5555"
			if c.edge != "" {
				r.Header.Set(HeaderEdge, c.edge)
			}
			if c.headerIP != "" {
				r.Header.Set(HeaderClientIP, c.headerIP)
			}
			if got := From(r, c.edgeToken); got != c.want {
				t.Fatalf("From = %q, want %q", got, c.want)
			}
		})
	}
}

func TestClientIPFromRemoteAddrWithoutPort(t *testing.T) {
	r := httptest.NewRequest("GET", "/", nil)
	r.RemoteAddr = "10.0.0.1"
	if got := From(r, ""); got != "10.0.0.1" {
		t.Fatalf("From = %q", got)
	}
}

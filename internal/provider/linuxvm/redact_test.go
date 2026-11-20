package linuxvm

import "testing"

func TestRedactEgress(t *testing.T) {
	for _, c := range []struct{ in, want string }{
		{"egress vm=v1 host=api.x method=GET path=/v1 status=200 bytes=12 dur=3ms vars=GITHUB_TOKEN,NPM", "egress vm=v1 host=api.x method=GET path=/v1 status=200 bytes=12 dur=3ms vars="},
		{"egress vm=v1 host=api.x method=GET path=/v1 status=502 bytes=0 dur=3ms vars=A err=dial tcp: refused", "egress vm=v1 host=api.x method=GET path=/v1 status=502 bytes=0 dur=3ms vars= err=dial tcp: refused"},
		{"egress vm=v1 host=api.x method=GET path=/v1 status=200 bytes=1 dur=1ms vars=", "egress vm=v1 host=api.x method=GET path=/v1 status=200 bytes=1 dur=1ms vars="},
		{"egress vm=v1 host=api.x account=me@x.com limited until=2026-11-20T10:00:00Z", "egress vm=v1 host=api.x account= limited until=2026-11-20T10:00:00Z"},
		{"proxy listening on 10.0.0.1:443", "proxy listening on 10.0.0.1:443"},
	} {
		if got := RedactEgress(c.in); got != c.want {
			t.Errorf("RedactEgress(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

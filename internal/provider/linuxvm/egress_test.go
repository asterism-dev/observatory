package linuxvm

import (
	"strings"
	"testing"

	"github.com/asterism-dev/observatory/internal/egress"
)

func TestEgressSlot(t *testing.T) {
	uid, port, mark, err := egressSlot("192.168.64.7")
	if err != nil || uid != 71007 || port != 15007 || mark != 0x4f0007 {
		t.Fatalf("slot = %d, %d, %#x, %v", uid, port, mark, err)
	}
	for _, ip := range []string{"192.168.64.254", "10.0.0.1", "192.168.64.1", "fe80::1", "x"} {
		if _, _, _, err := egressSlot(ip); err == nil {
			t.Errorf("%s: no error", ip)
		}
	}
}

func TestEgressRules(t *testing.T) {
	s := egressRules()
	for _, want := range []string{"table bridge obs_egress", "table inet obs_egress", "redirect to :meta mark map @ports"} {
		if !strings.Contains(s, want) {
			t.Errorf("rules lack %q", want)
		}
	}
	set := func(name string) string {
		_, rest, ok := strings.Cut(s, "set "+name+" {")
		if !ok {
			t.Fatalf("no set %s", name)
		}
		body, _, _ := strings.Cut(rest, "}")
		return body
	}
	deny4, deny6 := set("deny4"), set("deny6")
	for _, p := range egress.Forbidden {
		in, other := deny4, deny6
		if p.Addr().Is6() {
			in, other = deny6, deny4
		}
		if !strings.Contains(in, p.String()+",") || strings.Contains(other, p.String()+",") {
			t.Errorf("%s not exactly in its family's set", p)
		}
	}
}

package linuxvm

import (
	"net/netip"
	"os"
	"path/filepath"
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
	s := egressRules(nil)
	if strings.Contains(s, "dport 53") {
		t.Error("DNS accept without nameservers")
	}
	for _, want := range []string{"table bridge obs_egress", "table inet obs_egress", "redirect to :meta mark map @ports",
		`iifname "vmm0" meta mark & 0xffff0000 == 0x4f0000 ip daddr != 192.168.64.0/24 tcp dport 443`,
		`iifname "vmm0" meta mark . tcp dport @allowed accept`,
		`meta skuid 71002-71253 ct direction reply oifname "vmm0" accept`} {
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

func TestEgressRulesDNS(t *testing.T) {
	s := egressRules([]netip.Addr{netip.MustParseAddr("127.0.0.53"), netip.MustParseAddr("10.0.0.2"), netip.MustParseAddr("::1")})
	accept4 := "meta skuid 71002-71253 ip daddr { 127.0.0.53, 10.0.0.2 } meta l4proto { tcp, udp } th dport 53 accept"
	accept6 := "meta skuid 71002-71253 ip6 daddr { ::1 } meta l4proto { tcp, udp } th dport 53 accept"
	reject := strings.Index(s, "reject")
	for _, want := range []string{accept4, accept6} {
		if i := strings.Index(s, want); i < 0 || i > reject {
			t.Errorf("%q missing or after the first reject:\n%s", want, s)
		}
	}
}

func TestNameservers(t *testing.T) {
	p := filepath.Join(t.TempDir(), "resolv.conf")
	os.WriteFile(p, []byte("# x\nsearch lan\nnameserver 127.0.0.53\nnameserver fe80::1%eth0\nnameserver bogus\noptions edns0\n"), 0o644)
	got := nameservers(p)
	if len(got) != 2 || got[0].String() != "127.0.0.53" || got[1].String() != "fe80::1" {
		t.Fatalf("nameservers = %v", got)
	}
	if got := nameservers(filepath.Join(t.TempDir(), "missing")); got != nil {
		t.Fatalf("missing file: %v", got)
	}
}

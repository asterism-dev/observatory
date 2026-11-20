package linuxvm

import (
	"fmt"
	"log/slog"
	"net/netip"
	"os"
	"strings"

	"github.com/asterism-dev/observatory/internal/egress"
)

// egressOutputLog keeps the proxy's decrypted per-request log on the node's stderr, out of the telemetry fan-out.
var egressOutputLog = slog.New(slog.NewTextHandler(os.Stderr, nil))

// The proxy's UID sits next to the jail UIDs (70000+) and gets the same last-octet offset.
const (
	egressUIDBase  = 71000
	egressPortBase = 15000
	egressMarkBase = 0x4f0000
)

// egressSlot derives a VM's proxy UID, port and packet mark from its NAT address.
func egressSlot(natIP string) (uid, port int, mark uint32, err error) {
	ip, err := netip.ParseAddr(natIP)
	if err != nil || !ip.Is4() || !netip.MustParsePrefix("192.168.64.0/24").Contains(ip) {
		return 0, 0, 0, fmt.Errorf("NAT address %q is not in 192.168.64.0/24", natIP)
	}
	o := int(ip.As4()[3])
	if o < 2 || o > 253 {
		return 0, 0, 0, fmt.Errorf("NAT address %q has no egress slot", natIP)
	}
	return egressUIDBase + o, egressPortBase + o, egressMarkBase | uint32(o), nil
}

// nameservers reads resolv.conf; a missing or unreadable file yields none.
func nameservers(resolvConf string) []netip.Addr {
	b, _ := os.ReadFile(resolvConf)
	var out []netip.Addr
	for _, line := range strings.Split(string(b), "\n") {
		f := strings.Fields(line)
		if len(f) < 2 || f[0] != "nameserver" {
			continue
		}
		if ip, err := netip.ParseAddr(f[1]); err == nil {
			out = append(out, ip.Unmap().WithZone(""))
		}
	}
	return out
}

// egressRules recreates the node's egress tables atomically and lets proxy DNS to the node's nameservers (e.g. 127.0.0.53) pass before the rejects.
func egressRules(dns []netip.Addr) string {
	var deny4, deny6 strings.Builder
	for _, p := range egress.Forbidden {
		b := &deny4
		if p.Addr().Is6() {
			b = &deny6
		}
		fmt.Fprintf(b, "\t\t\t%s,\n", p)
	}
	uids := fmt.Sprintf("meta skuid %d-%d", egressUIDBase+2, egressUIDBase+253)
	var dnsRules strings.Builder
	for _, fam := range []string{"ip", "ip6"} {
		var ips []string
		for _, ip := range dns {
			if ip.Is4() == (fam == "ip") {
				ips = append(ips, ip.String())
			}
		}
		if len(ips) > 0 {
			fmt.Fprintf(&dnsRules, "\t\t%s %s daddr { %s } meta l4proto { tcp, udp } th dport 53 accept\n", uids, fam, strings.Join(ips, ", "))
		}
	}
	return fmt.Sprintf(`table bridge obs_egress {}
delete table bridge obs_egress
table bridge obs_egress {
	map marks { type ifname : mark; }
	chain prerouting {
		type filter hook prerouting priority filter;
		meta mark set iifname map @marks
	}
}
table inet obs_egress {}
delete table inet obs_egress
table inet obs_egress {
	map ports { type mark : inet_service; }
	set allowed { type mark . inet_service; }
	set deny4 {
		type ipv4_addr; flags interval;
		elements = {
%s		}
	}
	set deny6 {
		type ipv6_addr; flags interval;
		elements = {
%s		}
	}
	chain prerouting {
		type nat hook prerouting priority dstnat;
		iifname "vmm0" meta mark & 0xffff0000 == %#x ip daddr != 192.168.64.0/24 tcp dport 443 fib daddr type != local redirect to :meta mark map @ports
	}
	chain input {
		type filter hook input priority filter;
		iifname "vmm0" meta mark . tcp dport @allowed accept
		tcp dport %d-%d drop
	}
	chain output {
		type filter hook output priority filter;
		%[6]s ct direction reply oifname "vmm0" accept
%[7]s		%[6]s ip daddr @deny4 reject
		%[6]s ip6 daddr @deny6 reject
		%[6]s fib daddr type local reject
	}
}
`, deny4.String(), deny6.String(), egressMarkBase, egressPortBase+2, egressPortBase+253, uids, dnsRules.String())
}

// egressElements adds a VM's redirect elements, or deletes them by key.
func egressElements(add bool, tap string, port int, mark uint32) string {
	op, markVal, portVal := "delete", "", ""
	if add {
		op, markVal, portVal = "add", fmt.Sprintf(" : %#x", mark), fmt.Sprintf(" : %d", port)
	}
	return fmt.Sprintf("%[1]s element bridge obs_egress marks { %[2]q%[3]s }\n"+
		"%[1]s element inet obs_egress ports { %#[4]x%[5]s }\n"+
		"%[1]s element inet obs_egress allowed { %#[4]x . %[6]d }\n", op, tap, markVal, mark, portVal, port)
}

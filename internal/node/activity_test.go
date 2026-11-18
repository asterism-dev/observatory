package node

import (
	"encoding/binary"
	"testing"
)

func ethFrame(etherType uint16, payload []byte) []byte {
	f := make([]byte, 14, 14+len(payload))
	binary.BigEndian.PutUint16(f[12:], etherType)
	return append(f, payload...)
}

func ipv4(proto byte, l4 []byte) []byte {
	h := make([]byte, 20)
	h[0], h[9] = 0x45, proto
	return ethFrame(0x0800, append(h, l4...))
}

func ipv6(next byte, l4 []byte) []byte {
	h := make([]byte, 40)
	h[0], h[6] = 0x60, next
	return ethFrame(0x86dd, append(h, l4...))
}

func ports(src, dst uint16) []byte {
	b := make([]byte, 20)
	binary.BigEndian.PutUint16(b, src)
	binary.BigEndian.PutUint16(b[2:], dst)
	return b
}

func TestIsActivity(t *testing.T) {
	fragment := ipv4(17, []byte{1, 2})
	fragment[14+6], fragment[14+7] = 0x00, 0xb9 // offset 185 * 8, no ports in this fragment
	tests := []struct {
		name  string
		frame []byte
		want  bool
	}{
		{"arp", ethFrame(0x0806, make([]byte, 28)), false},
		{"udp 53", ipv4(17, ports(40000, 53)), false},
		{"udp 67", ipv4(17, ports(68, 67)), false},
		{"udp 68", ipv4(17, ports(67, 68)), false},
		{"udp 123", ipv4(17, ports(40000, 123)), false},
		{"udp 5353", ipv4(17, ports(5353, 5353)), false},
		{"udp 5355", ipv4(17, ports(40000, 5355)), false},
		{"ipv6 udp 546", ipv6(17, ports(547, 546)), false},
		{"ipv6 udp 547", ipv6(17, ports(546, 547)), false},
		{"tcp 53", ipv4(6, ports(40000, 53)), false},
		{"icmp echo", ipv4(1, []byte{8, 0, 0, 0, 0, 1, 0, 1}), false},
		{"icmpv6 neighbour solicitation", ipv6(58, []byte{135, 0, 0, 0, 0, 0, 0, 0}), false},
		{"igmp", ipv4(2, []byte{0x16, 0, 0, 0, 224, 0, 0, 251}), false},
		{"vlan", ethFrame(0x8100, append([]byte{0, 1, 0x08, 0}, ipv4(6, ports(40000, 22))[14:]...)), false},
		{"tcp syn 22", ipv4(6, ports(40000, 22)), true},
		{"udp 51820", ipv4(17, ports(51820, 51820)), true},
		{"ipv6 tcp 443", ipv6(6, ports(40000, 443)), true},
		{"ipv4 non-first fragment", fragment, true},
		{"truncated", ipv4(6, ports(40000, 22))[:20], false},
	}
	for _, tt := range tests {
		if got := IsActivity(tt.frame); got != tt.want {
			t.Errorf("%s: IsActivity = %v, want %v", tt.name, got, tt.want)
		}
	}
}

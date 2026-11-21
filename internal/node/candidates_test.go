package node

import (
	"net"
	"slices"
	"testing"
)

func TestCandidatesOrder(t *testing.T) {
	up := net.FlagUp
	ifaces := []net.Interface{
		{Name: "tailscale0", Flags: up}, {Name: "eth1", Flags: up}, {Name: "eth0", Flags: up}, {Name: "docker0", Flags: up},
		{Name: "wg-underlay", Flags: up}, {Name: "vmm0", Flags: up}, {Name: "lo", Flags: up | net.FlagLoopback},
	}
	ips := map[string]string{"eth0": "192.168.1.5", "eth1": "203.0.113.9", "tailscale0": "100.101.1.2", "docker0": "172.17.0.1",
		"wg-underlay": "10.198.0.12", "vmm0": "192.168.64.1", "lo": "127.0.0.1"}
	addrs := func(i net.Interface) ([]net.Addr, error) {
		return []net.Addr{&net.IPNet{IP: net.ParseIP(ips[i.Name]), Mask: net.CIDRMask(24, 32)}}, nil
	}
	got := candidates(ifaces, addrs, 51822)
	want := []string{"192.168.1.5:51822", "203.0.113.9:51822", "100.101.1.2:51822"}
	if !slices.Equal(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}

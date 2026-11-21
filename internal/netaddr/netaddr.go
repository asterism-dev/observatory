// Package netaddr is the hub's pure address plan for VM networking.
package netaddr

import (
	"encoding/binary"
	"errors"
	"fmt"
	"net/netip"
	"slices"
)

var (
	MgmtPool        = netip.MustParsePrefix("10.64.0.0/10")
	DevicePool      = netip.MustParsePrefix("10.192.0.0/16")
	GatewayDNS      = netip.MustParseAddr("10.192.0.1")
	UnderlayPool    = netip.MustParsePrefix("10.198.0.0/16")
	GatewayUnderlay = netip.MustParseAddr("10.198.0.1")
	Transit         = netip.MustParsePrefix("10.199.0.0/30")
	AutoNetworks    = netip.MustParsePrefix("10.210.0.0/16")
	NATNet          = netip.MustParsePrefix("192.168.64.0/24")
)

const (
	SliceBits       = 20
	HostOffset      = 10
	DefaultGuestMTU = 1300
	UnderlayPort    = 51820
	ClientsPort     = 51821
	// NodeUnderlayPort is where nodes listen on wg-underlay for direct peers.
	NodeUnderlayPort = 51822
	// ControlPort is the gateway proxy that carries node control traffic over the underlay.
	ControlPort = 8091
	// Storage NBD exports listen on GatewayUnderlay, one port per attachment.
	StoragePortFirst = 10810
	StoragePortLast  = 11809
)

var (
	ErrExhausted     = errors.New("address space exhausted")
	ErrInvalidSubnet = errors.New("invalid subnet")
)

var reserved = []netip.Prefix{MgmtPool, DevicePool, UnderlayPool, Transit, AutoNetworks, NATNet}

var userRanges = []netip.Prefix{
	netip.MustParsePrefix("10.0.0.0/8"),
	netip.MustParsePrefix("172.16.0.0/12"),
	netip.MustParsePrefix("192.168.0.0/16"),
}

func toU32(a netip.Addr) uint32 { b := a.As4(); return binary.BigEndian.Uint32(b[:]) }

func fromU32(v uint32) netip.Addr {
	var b [4]byte
	binary.BigEndian.PutUint32(b[:], v)
	return netip.AddrFrom4(b)
}

func overlapsAny(p netip.Prefix, used []netip.Prefix) bool {
	return slices.ContainsFunc(used, p.Overlaps)
}

// freeAligned returns the lowest aligned prefix of the given size inside pool that overlaps nothing in used.
func freeAligned(pool netip.Prefix, bits int, used []netip.Prefix) (netip.Prefix, error) {
	step := uint32(1) << (32 - bits)
	start := toU32(pool.Addr())
	count := uint32(1) << (bits - pool.Bits())
	for i := uint32(0); i < count; i++ {
		p := netip.PrefixFrom(fromU32(start+i*step), bits)
		if !overlapsAny(p, used) {
			return p, nil
		}
	}
	return netip.Prefix{}, ErrExhausted
}

func FreeSlice(used []netip.Prefix) (netip.Prefix, error) {
	return freeAligned(MgmtPool, SliceBits, used)
}

func FreeSubnet(used []netip.Prefix) (netip.Prefix, error) {
	return freeAligned(AutoNetworks, 24, used)
}

func FreeAddr(first, last netip.Addr, used []netip.Addr) (netip.Addr, error) {
	taken := make(map[netip.Addr]struct{}, len(used))
	for _, a := range used {
		taken[a] = struct{}{}
	}
	for a := first; a.Compare(last) <= 0; a = a.Next() {
		if _, ok := taken[a]; !ok {
			return a, nil
		}
	}
	return netip.Addr{}, ErrExhausted
}

func FreeHost(p netip.Prefix, used []netip.Addr) (netip.Addr, error) {
	if !p.IsValid() || !p.Addr().Is4() {
		return netip.Addr{}, fmt.Errorf("%w: %s is not an IPv4 prefix", ErrInvalidSubnet, p)
	}
	base := toU32(p.Masked().Addr())
	broadcast := base | (^uint32(0) >> p.Bits())
	return FreeAddr(fromU32(base+HostOffset), fromU32(broadcast-1), used)
}

func ValidateSubnet(s string) (netip.Prefix, error) {
	p, err := netip.ParsePrefix(s)
	if err != nil || !p.Addr().Is4() {
		return netip.Prefix{}, fmt.Errorf("%w: %q is not an IPv4 CIDR", ErrInvalidSubnet, s)
	}
	p = p.Masked()
	if p.Bits() < 16 || p.Bits() > 28 {
		return netip.Prefix{}, fmt.Errorf("%w: prefix length must be /16 to /28", ErrInvalidSubnet)
	}
	if !slices.ContainsFunc(userRanges, func(r netip.Prefix) bool { return r.Contains(p.Addr()) }) {
		return netip.Prefix{}, fmt.Errorf("%w: must be inside 10.0.0.0/8, 172.16.0.0/12 or 192.168.0.0/16", ErrInvalidSubnet)
	}
	if overlapsAny(p, reserved) {
		return netip.Prefix{}, fmt.Errorf("%w: overlaps a reserved range", ErrInvalidSubnet)
	}
	return p, nil
}

func Router(slice netip.Prefix) netip.Addr { return slice.Masked().Addr().Next() }

func MAC(ip netip.Addr) string {
	b := ip.As4()
	return fmt.Sprintf("02:%02x:%02x:00:%02x:%02x", b[0], b[1], b[2], b[3])
}

func StorageURL(port int, diskID string) string {
	return fmt.Sprintf("nbd://%s:%d/%s", GatewayUnderlay, port, diskID)
}

// DirectEndpointAddr reports whether a is a plausible peer-reachable address: no zone, loopback, link-local, multicast, unspecified or platform-reserved range.
func DirectEndpointAddr(a netip.Addr) bool {
	if a.Zone() != "" {
		return false
	}
	a = a.Unmap()
	return a.IsValid() && !a.IsLoopback() && !a.IsLinkLocalUnicast() && !a.IsLinkLocalMulticast() && !a.IsMulticast() && !a.IsUnspecified() &&
		!slices.ContainsFunc(reserved, func(p netip.Prefix) bool { return p.Contains(a) })
}

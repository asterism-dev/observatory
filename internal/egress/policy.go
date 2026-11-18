package egress

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"syscall"
	"time"
)

var ErrForbidden = errors.New("destination not allowed")

// Forbidden adds the unspecified addresses to the spec's list because dialing them reaches loopback.
var Forbidden = []netip.Prefix{
	netip.MustParsePrefix("0.0.0.0/8"),
	netip.MustParsePrefix("127.0.0.0/8"),
	netip.MustParsePrefix("169.254.0.0/16"),
	netip.MustParsePrefix("10.64.0.0/10"),
	netip.MustParsePrefix("10.192.0.0/16"),
	netip.MustParsePrefix("10.198.0.0/16"),
	netip.MustParsePrefix("10.199.0.0/30"),
	netip.MustParsePrefix("10.210.0.0/16"),
	netip.MustParsePrefix("192.168.64.0/24"),
	netip.MustParsePrefix("::/128"),
	netip.MustParsePrefix("::1/128"),
	netip.MustParsePrefix("fe80::/10"),
	netip.MustParsePrefix("fc00::/7"),
}

type Policy struct {
	Deny  []netip.Prefix
	Local func() ([]netip.Addr, error)
}

func DefaultPolicy() Policy {
	return Policy{Deny: Forbidden, Local: interfaceAddrs}
}

func interfaceAddrs() ([]netip.Addr, error) {
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		return nil, err
	}
	var out []netip.Addr
	for _, a := range addrs {
		if n, ok := a.(*net.IPNet); ok {
			if ip, ok := netip.AddrFromSlice(n.IP); ok {
				out = append(out, ip.Unmap())
			}
		}
	}
	return out, nil
}

func (p Policy) Allowed(ip netip.Addr) bool {
	ip = ip.Unmap().WithZone("")
	if !ip.IsValid() {
		return false
	}
	for _, d := range p.Deny {
		if d.Contains(ip) {
			return false
		}
	}
	if p.Local == nil {
		return true
	}
	local, err := p.Local()
	if err != nil {
		return false
	}
	for _, l := range local {
		if l.Unmap() == ip {
			return false
		}
	}
	return true
}

// Dialer checks the address actually connected to, so names resolved by the dialer are filtered too.
func (p Policy) Dialer() *net.Dialer {
	return &net.Dialer{
		KeepAlive: 30 * time.Second,
		ControlContext: func(_ context.Context, _, address string, _ syscall.RawConn) error {
			ap, err := netip.ParseAddrPort(address)
			if err != nil {
				return err
			}
			if !p.Allowed(ap.Addr()) {
				return fmt.Errorf("%w: %s", ErrForbidden, ap.Addr())
			}
			return nil
		},
	}
}

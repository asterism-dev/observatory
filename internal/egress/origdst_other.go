//go:build !linux

package egress

import (
	"errors"
	"net"
	"net/netip"
)

func OriginalDst(net.Conn) (netip.AddrPort, error) {
	return netip.AddrPort{}, errors.New("original destination is only available on linux")
}

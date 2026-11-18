//go:build linux

package egress

import (
	"encoding/binary"
	"errors"
	"net"
	"net/netip"
	"syscall"

	"golang.org/x/sys/unix"
)

func OriginalDst(c net.Conn) (netip.AddrPort, error) {
	sc, ok := c.(syscall.Conn)
	if !ok {
		return netip.AddrPort{}, errors.New("not a socket")
	}
	rc, err := sc.SyscallConn()
	if err != nil {
		return netip.AddrPort{}, err
	}
	var dst netip.AddrPort
	var serr error
	err = rc.Control(func(fd uintptr) {
		// The 16-byte multiaddr of IPv6Mreq holds the returned sockaddr_in.
		var m *unix.IPv6Mreq
		m, serr = unix.GetsockoptIPv6Mreq(int(fd), unix.SOL_IP, unix.SO_ORIGINAL_DST)
		if serr != nil {
			return
		}
		sa := m.Multiaddr
		dst = netip.AddrPortFrom(netip.AddrFrom4([4]byte(sa[4:8])), binary.BigEndian.Uint16(sa[2:4]))
	})
	if err != nil {
		return netip.AddrPort{}, err
	}
	return dst, serr
}

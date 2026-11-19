package node

import "encoding/binary"

// idlePorts carry background chatter (DNS, DHCP, NTP, DHCPv6, mDNS, LLMNR) that must not keep a VM awake.
var idlePorts = map[uint16]bool{53: true, 67: true, 68: true, 123: true, 546: true, 547: true, 5353: true, 5355: true}

// IsActivity reports whether an Ethernet frame is TCP or UDP traffic that keeps a VM from auto-suspending.
func IsActivity(frame []byte) bool {
	// Broadcast and multicast reach every VM on the segment, so one chatty guest would keep all of them awake.
	if len(frame) < 14 || frame[0]&1 != 0 {
		return false
	}
	ip := frame[14:]
	var proto byte
	var l4 []byte
	switch binary.BigEndian.Uint16(frame[12:]) {
	case 0x0800:
		if len(ip) < 20 || ip[0]>>4 != 4 {
			return false
		}
		hl := int(ip[0]&0x0f) * 4
		proto = ip[9]
		if proto != 6 && proto != 17 {
			return false
		}
		// Later fragments carry no ports; the first one was judged already.
		if binary.BigEndian.Uint16(ip[6:])&0x1fff != 0 {
			return true
		}
		if hl < 20 || len(ip) < hl {
			return false
		}
		l4 = ip[hl:]
	case 0x86dd:
		if len(ip) < 40 {
			return false
		}
		proto, l4 = ip[6], ip[40:]
		// Hop-by-hop, routing, destination options and fragment headers.
		for proto == 0 || proto == 43 || proto == 60 || proto == 44 {
			if len(l4) < 8 {
				return false
			}
			n := (int(l4[1]) + 1) * 8
			if proto == 44 {
				n = 8
				if binary.BigEndian.Uint16(l4[2:])&0xfff8 != 0 {
					return l4[0] == 6 || l4[0] == 17
				}
			}
			if len(l4) < n {
				return false
			}
			proto, l4 = l4[0], l4[n:]
		}
		if proto != 6 && proto != 17 {
			return false
		}
	default:
		return false
	}
	if len(l4) < 4 {
		return false
	}
	return !idlePorts[binary.BigEndian.Uint16(l4)] && !idlePorts[binary.BigEndian.Uint16(l4[2:])]
}

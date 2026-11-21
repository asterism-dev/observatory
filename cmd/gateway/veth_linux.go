package main

import (
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/vishvananda/netlink"
	"github.com/vishvananda/netlink/nl"
	"golang.org/x/sys/unix"
)

// removeHostVeths deletes earlier gateways' host ends (gw0b*) by netnsid, as setns needs CAP_SYS_ADMIN, which the gateway lacks.
func removeHostVeths(hostNS string) error {
	f, err := os.Open(hostNS)
	if err != nil {
		return err
	}
	defer f.Close()
	fd := int(f.Fd())
	if err := netlink.SetNetNsIdByFd(fd, -1); err != nil && !errors.Is(err, unix.EEXIST) {
		return fmt.Errorf("assign netnsid: %w", err)
	}
	nsid, err := netlink.GetNetNsIdByFd(fd)
	if err != nil {
		return fmt.Errorf("get netnsid: %w", err)
	}
	target := nl.NewRtAttr(unix.IFLA_TARGET_NETNSID, nl.Uint32Attr(uint32(nsid)))

	req := nl.NewNetlinkRequest(unix.RTM_GETLINK, unix.NLM_F_DUMP)
	req.AddData(nl.NewIfInfomsg(unix.AF_UNSPEC))
	req.AddData(target)
	msgs, err := req.Execute(unix.NETLINK_ROUTE, unix.RTM_NEWLINK)
	if err != nil {
		return fmt.Errorf("list host links: %w", err)
	}
	for _, m := range msgs {
		ifi := nl.DeserializeIfInfomsg(m)
		attrs, err := nl.ParseRouteAttr(m[ifi.Len():])
		if err != nil {
			return err
		}
		for _, a := range attrs {
			if a.Attr.Type != unix.IFLA_IFNAME {
				continue
			}
			name := strings.TrimRight(string(a.Value), "\x00")
			if !strings.HasPrefix(name, "gw0b") {
				break
			}
			del := nl.NewNetlinkRequest(unix.RTM_DELLINK, unix.NLM_F_ACK)
			msg := nl.NewIfInfomsg(unix.AF_UNSPEC)
			msg.Index = ifi.Index
			del.AddData(msg)
			del.AddData(target)
			// a pair whose old namespace was freed meanwhile is gone already
			if _, err := del.Execute(unix.NETLINK_ROUTE, 0); err != nil && !errors.Is(err, unix.ENODEV) {
				return fmt.Errorf("delete host link %s: %w", name, err)
			}
			break
		}
	}
	return nil
}

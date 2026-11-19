package guestinit

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/insomniacslk/dhcp/dhcpv4"
	"github.com/insomniacslk/dhcp/dhcpv4/nclient4"
	"github.com/vishvananda/netlink"

	"github.com/asterism-dev/observatory/internal/proto"
)

// dhcpRouteMetric matches networkd's, so a static default route of the primary NIC wins over DHCP ones.
const dhcpRouteMetric = 1024

var (
	dhcpMu      sync.Mutex
	dhcpCancels = map[string]context.CancelFunc{}
)

// ConfigureNICs names each NIC after the seed (matched by MAC), gives the primary its static address and default route and starts DHCP clients; it does not wait for leases.
func ConfigureNICs(nics []proto.GuestNIC) error {
	var errs []error
	if lo, err := netlink.LinkByName("lo"); err != nil {
		errs = append(errs, err)
	} else if err := netlink.LinkSetUp(lo); err != nil {
		errs = append(errs, fmt.Errorf("lo: %w", err))
	}
	links, err := netlink.LinkList()
	if err != nil {
		return err
	}
	for i, n := range nics {
		if err := configureNIC(links, n, i == 0); err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", n.Name, err))
		}
	}
	return errors.Join(errs...)
}

func configureNIC(links []netlink.Link, n proto.GuestNIC, primary bool) error {
	var link netlink.Link
	for _, l := range links {
		if strings.EqualFold(l.Attrs().HardwareAddr.String(), n.MAC) {
			link = l
		}
	}
	if link == nil {
		return fmt.Errorf("no link with MAC %s", n.MAC)
	}
	// ponytail: a seed name already held by another link fails the rename; kernel order matches attach order, so only eth0 could clash.
	if link.Attrs().Name != n.Name {
		if err := netlink.LinkSetDown(link); err != nil {
			return err
		}
		if err := netlink.LinkSetName(link, n.Name); err != nil {
			return err
		}
	}
	if err := clearIPv4(link); err != nil {
		return err
	}
	// OVN NICs are IPv4 only, as with today's netplan link-local: [] and accept-ra: false.
	if !primary {
		err := os.WriteFile("/proc/sys/net/ipv6/conf/"+n.Name+"/disable_ipv6", []byte("1"), 0)
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	if err := netlink.LinkSetUp(link); err != nil {
		return err
	}
	switch {
	case n.Address != "":
		addr, err := netlink.ParseAddr(n.Address)
		if err != nil {
			return err
		}
		if err := netlink.AddrReplace(link, addr); err != nil {
			return err
		}
		if n.Gateway != "" {
			gw := net.ParseIP(n.Gateway)
			if gw == nil {
				return fmt.Errorf("invalid gateway %q", n.Gateway)
			}
			return netlink.RouteReplace(&netlink.Route{LinkIndex: link.Attrs().Index, Gw: gw})
		}
	case n.DHCP:
		startDHCP(n.Name, link)
	}
	return nil
}

// clearIPv4 drops the addresses and routes a restored template left on the link, so only the seed's addressing remains.
func clearIPv4(link netlink.Link) error {
	addrs, err := netlink.AddrList(link, netlink.FAMILY_V4)
	if err != nil {
		return err
	}
	for _, a := range addrs {
		if err := netlink.AddrDel(link, &a); err != nil && !errors.Is(err, syscall.EADDRNOTAVAIL) {
			return err
		}
	}
	routes, err := netlink.RouteList(link, netlink.FAMILY_V4)
	if err != nil {
		return err
	}
	for _, r := range routes {
		if r.Dst != nil && r.Dst.IP.IsLinkLocalUnicast() {
			continue
		}
		if err := netlink.RouteDel(&r); err != nil && !errors.Is(err, syscall.ESRCH) {
			return err
		}
	}
	return nil
}

// startDHCP replaces any client already running for the NIC, so a takeoff can reconfigure the network.
func startDHCP(name string, link netlink.Link) {
	ctx, cancel := context.WithCancel(context.Background())
	dhcpMu.Lock()
	if old := dhcpCancels[name]; old != nil {
		old()
	}
	dhcpCancels[name] = cancel
	dhcpMu.Unlock()
	go runDHCP(ctx, name, link)
}

func runDHCP(ctx context.Context, name string, link netlink.Link) {
	mods := []dhcpv4.Modifier{
		// The lease must be findable by MAC, as with netplan's dhcp-identifier: mac.
		dhcpv4.WithOption(dhcpv4.OptClientIdentifier(append([]byte{1}, link.Attrs().HardwareAddr...))),
		dhcpv4.WithRequestedOptions(dhcpv4.OptionSubnetMask, dhcpv4.OptionRouter, dhcpv4.OptionInterfaceMTU, dhcpv4.OptionClasslessStaticRoute),
	}
	var lease *nclient4.Lease
	for {
		next, err := dhcpExchange(ctx, name, lease, mods)
		wait := 5 * time.Second
		if err == nil {
			err = applyLease(link, next.ACK)
		}
		if err == nil {
			lease = next
			wait = next.ACK.IPAddressRenewalTime(next.ACK.IPAddressLeaseTime(time.Hour) / 2)
		} else if ctx.Err() == nil {
			// The next round starts over with a fresh discover; OVN hands a port the same address again.
			lease = nil
			fmt.Fprintf(os.Stderr, "init: dhcp %s: %v\n", name, err)
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}
	}
}

func dhcpExchange(ctx context.Context, name string, lease *nclient4.Lease, mods []dhcpv4.Modifier) (*nclient4.Lease, error) {
	c, err := nclient4.New(name)
	if err != nil {
		return nil, err
	}
	defer c.Close()
	if lease != nil {
		return c.Renew(ctx, lease, mods...)
	}
	return c.Request(ctx, mods...)
}

// applyLease takes address, MTU and routes from the lease, never DNS, hostname or domains.
func applyLease(link netlink.Link, ack *dhcpv4.DHCPv4) error {
	mask := ack.SubnetMask()
	if mask == nil {
		mask = ack.YourIPAddr.DefaultMask()
	}
	if err := netlink.AddrReplace(link, &netlink.Addr{IPNet: &net.IPNet{IP: ack.YourIPAddr, Mask: mask}}); err != nil {
		return err
	}
	if mtu, err := dhcpv4.GetUint16(dhcpv4.OptionInterfaceMTU, ack.Options); err == nil && mtu >= 576 {
		if err := netlink.LinkSetMTU(link, int(mtu)); err != nil {
			return err
		}
	}
	routes := ack.ClasslessStaticRoute()
	// RFC 3442: the router option only counts without classless routes.
	if gws := ack.Router(); len(routes) == 0 && len(gws) > 0 {
		routes = []*dhcpv4.Route{{Dest: &net.IPNet{IP: net.IPv4zero, Mask: net.CIDRMask(0, 32)}, Router: gws[0]}}
	}
	for _, rt := range routes {
		r := &netlink.Route{LinkIndex: link.Attrs().Index, Dst: rt.Dest, Priority: dhcpRouteMetric}
		if rt.Router.IsUnspecified() {
			r.Scope = netlink.SCOPE_LINK
		} else {
			r.Gw = rt.Router
		}
		if err := netlink.RouteReplace(r); err != nil {
			return fmt.Errorf("route %s: %w", rt.Dest, err)
		}
	}
	return nil
}

// waitIPv4 polls until the named link has an IPv4 address, for at most timeout.
func waitIPv4(name string, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for {
		if link, err := netlink.LinkByName(name); err == nil {
			if addrs, err := netlink.AddrList(link, netlink.FAMILY_V4); err == nil && len(addrs) > 0 {
				return true
			}
		}
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(time.Second)
	}
}

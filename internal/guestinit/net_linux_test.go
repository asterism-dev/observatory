package guestinit

import (
	"net"
	"os"
	"runtime"
	"testing"
	"time"

	"github.com/vishvananda/netlink"
	"github.com/vishvananda/netns"

	"github.com/asterism-dev/observatory/internal/proto"
)

func TestConfigureNICsDummy(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("needs root")
	}
	runtime.LockOSThread()
	orig, err := netns.Get()
	if err != nil {
		t.Fatal(err)
	}
	ns, err := netns.New()
	if err != nil {
		t.Fatal(err)
	}
	// The thread stays locked: it is unusable for other goroutines once it has been in the throwaway namespace.
	t.Cleanup(func() { netns.Set(orig); ns.Close(); orig.Close() })

	mac := func(s string) net.HardwareAddr { m, _ := net.ParseMAC(s); return m }
	for name, m := range map[string]string{"dummyA": "02:00:00:00:00:01", "dummyB": "02:00:00:00:00:02"} {
		if err := netlink.LinkAdd(&netlink.Dummy{LinkAttrs: netlink.LinkAttrs{Name: name, HardwareAddr: mac(m)}}); err != nil {
			t.Fatal(err)
		}
	}
	nics := []proto.GuestNIC{
		{MAC: "02:00:00:00:00:01", Name: "eth0", Address: "10.9.0.5/24", Gateway: "10.9.0.1"},
		{MAC: "02:00:00:00:00:02", Name: "net0"},
	}
	for range 2 {
		if err := ConfigureNICs(nics); err != nil {
			t.Fatal(err)
		}
	}
	eth0, err := netlink.LinkByName("eth0")
	if err != nil {
		t.Fatal(err)
	}
	if eth0.Attrs().Flags&net.FlagUp == 0 {
		t.Fatal("eth0 down")
	}
	addrs, _ := netlink.AddrList(eth0, netlink.FAMILY_V4)
	if len(addrs) != 1 || addrs[0].IPNet.String() != "10.9.0.5/24" {
		t.Fatalf("eth0 addrs %v", addrs)
	}
	routes, _ := netlink.RouteList(eth0, netlink.FAMILY_V4)
	var gw net.IP
	for _, r := range routes {
		if r.Dst == nil || r.Dst.IP.IsUnspecified() {
			gw = r.Gw
		}
	}
	if !gw.Equal(net.ParseIP("10.9.0.1")) {
		t.Fatalf("default route %v in %v", gw, routes)
	}
	if !waitIPv4("eth0", time.Second) || waitIPv4("net0", time.Second) {
		t.Fatal("waitIPv4: want eth0 addressed, net0 not")
	}
	if v6, err := os.ReadFile("/proc/sys/net/ipv6/conf/net0/disable_ipv6"); err == nil && string(v6) != "1\n" {
		t.Fatalf("net0 disable_ipv6 = %q", v6)
	}
	if lo, _ := netlink.LinkByName("lo"); lo.Attrs().Flags&net.FlagUp == 0 {
		t.Fatal("lo down")
	}
}

func TestConfigureNICsReplacesOldAddress(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("needs root")
	}
	runtime.LockOSThread()
	orig, err := netns.Get()
	if err != nil {
		t.Fatal(err)
	}
	ns, err := netns.New()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { netns.Set(orig); ns.Close(); orig.Close() })

	mac, _ := net.ParseMAC("02:00:00:00:00:01")
	if err := netlink.LinkAdd(&netlink.Dummy{LinkAttrs: netlink.LinkAttrs{Name: "dummyA", HardwareAddr: mac}}); err != nil {
		t.Fatal(err)
	}
	link, _ := netlink.LinkByName("dummyA")
	old, _ := netlink.ParseAddr("10.9.9.9/24")
	if err := netlink.LinkSetUp(link); err != nil || netlink.AddrAdd(link, old) != nil {
		t.Fatal("setup failed")
	}
	if err := ConfigureNICs([]proto.GuestNIC{{MAC: "02:00:00:00:00:01", Name: "eth0", Address: "10.1.1.2/24"}}); err != nil {
		t.Fatal(err)
	}
	eth0, _ := netlink.LinkByName("eth0")
	addrs, _ := netlink.AddrList(eth0, netlink.FAMILY_V4)
	if len(addrs) != 1 || addrs[0].IPNet.String() != "10.1.1.2/24" {
		t.Fatalf("addrs = %v", addrs)
	}
}

package main

import (
	"fmt"
	"os"
	"runtime"
	"slices"
	"testing"

	"github.com/vishvananda/netlink"
	"github.com/vishvananda/netns"
)

func TestRemoveHostVeths(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("needs root")
	}
	runtime.LockOSThread()
	orig, err := netns.Get()
	if err != nil {
		t.Fatal(err)
	}
	host, err := netns.New()
	if err != nil {
		t.Fatal(err)
	}
	if err := netlink.LinkAdd(&netlink.Dummy{LinkAttrs: netlink.LinkAttrs{Name: "eth9"}}); err != nil {
		t.Fatal(err)
	}
	gateway, err := netns.New()
	if err != nil {
		t.Fatal(err)
	}
	// The thread stays locked: it is unusable for other goroutines once it has been in the throwaway namespaces.
	t.Cleanup(func() { netns.Set(orig); gateway.Close(); host.Close(); orig.Close() })
	for _, br := range []string{"gw0b0000aaaa", "gw0b0000bbbb"} {
		veth := &netlink.Veth{LinkAttrs: netlink.LinkAttrs{Name: "gw0"}, PeerName: br, PeerNamespace: netlink.NsFd(host)}
		if err := netlink.LinkAdd(veth); err != nil {
			t.Fatal(err)
		}
		// a stale pair whose container end lives on in an old namespace, as after a gateway crash
		if err := netlink.LinkSetName(veth, "old"+br[4:]); err != nil {
			t.Fatal(err)
		}
	}
	path := fmt.Sprintf("/proc/self/fd/%d", int(host))
	for range 2 {
		if err := removeHostVeths(path); err != nil {
			t.Fatal(err)
		}
	}
	if err := netns.Set(host); err != nil {
		t.Fatal(err)
	}
	links, err := netlink.LinkList()
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, l := range links {
		names = append(names, l.Attrs().Name)
	}
	slices.Sort(names)
	if !slices.Equal(names, []string{"eth9", "lo"}) {
		t.Fatalf("host links = %v", names)
	}
}

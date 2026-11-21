package node

import (
	"context"
	"net"
	"net/netip"
	"slices"
	"strings"
	"time"

	"github.com/asterism-dev/observatory/internal/netaddr"
	"github.com/asterism-dev/observatory/internal/proto"
)

const maxCandidates = 8

var (
	skipIfacePrefixes = []string{"docker", "br-", "veth", "ovs-", "genev_sys_", "vmm0", "wg-underlay"}
	cgnat             = netip.MustParsePrefix("100.64.0.0/10")
)

// candidates lists the node's reachable underlay endpoints: private LAN first, then public, then CGNAT/VPN.
func candidates(ifaces []net.Interface, addrs func(net.Interface) ([]net.Addr, error), port int) []string {
	type ranked struct {
		rank int
		ep   string
	}
	var found []ranked
	for _, ifc := range ifaces {
		if ifc.Flags&net.FlagUp == 0 || ifc.Flags&net.FlagLoopback != 0 || slices.ContainsFunc(skipIfacePrefixes, func(p string) bool { return strings.HasPrefix(ifc.Name, p) }) {
			continue
		}
		as, err := addrs(ifc)
		if err != nil {
			continue
		}
		for _, a := range as {
			ipn, ok := a.(*net.IPNet)
			if !ok {
				continue
			}
			ip, ok := netip.AddrFromSlice(ipn.IP)
			if !ok || !netaddr.DirectEndpointAddr(ip) {
				continue
			}
			ip = ip.Unmap()
			rank := 1
			switch {
			case cgnat.Contains(ip) || strings.HasPrefix(ifc.Name, "tailscale"):
				rank = 2
			case ip.IsPrivate():
				rank = 0
			}
			found = append(found, ranked{rank, netip.AddrPortFrom(ip, uint16(port)).String()})
		}
	}
	slices.SortStableFunc(found, func(a, b ranked) int { return a.rank - b.rank })
	out := make([]string, 0, min(len(found), maxCandidates))
	for _, r := range found {
		if len(out) < maxCandidates && !slices.Contains(out, r.ep) {
			out = append(out, r.ep)
		}
	}
	return out
}

func localCandidates() []string {
	ifaces, err := net.Interfaces()
	if err != nil {
		return nil
	}
	return candidates(ifaces, func(i net.Interface) ([]net.Addr, error) { return i.Addrs() }, netaddr.NodeUnderlayPort)
}

// candidatesLoop sends the endpoint set on connect and whenever it changes.
func (a *Agent) candidatesLoop(ctx context.Context) {
	var last []string
	t := time.NewTicker(candidateCheck)
	defer t.Stop()
	for {
		if cur := localCandidates(); !slices.Equal(cur, last) {
			a.mu.Lock()
			send := a.send
			a.mu.Unlock()
			if send != nil && send(proto.Message{Type: "candidates", Candidates: cur}) == nil {
				last = cur
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

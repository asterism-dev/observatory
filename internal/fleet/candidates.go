package fleet

import (
	"log/slog"
	"net/netip"
	"slices"

	"github.com/google/uuid"

	"github.com/asterism-dev/observatory/internal/netaddr"
)

func validEndpoint(s string) (netip.AddrPort, bool) {
	ap, err := netip.ParseAddrPort(s)
	if err != nil || ap.Port() == 0 || !netaddr.DirectEndpointAddr(ap.Addr()) {
		return netip.AddrPort{}, false
	}
	return netip.AddrPortFrom(ap.Addr().Unmap(), ap.Port()), true
}

// validCandidates keeps the usable, distinct entries of a node-reported list on the node port, at most netaddr.MaxCandidates.
func validCandidates(in []string) []string {
	out := make([]string, 0, min(len(in), netaddr.MaxCandidates))
	for _, s := range in {
		ap, ok := validEndpoint(s)
		if !ok || ap.Port() != netaddr.NodeUnderlayPort {
			continue
		}
		s = ap.String()
		if slices.Contains(out, s) {
			continue
		}
		out = append(out, s)
		if len(out) == netaddr.MaxCandidates {
			break
		}
	}
	if dropped := len(in) - len(out); dropped > 0 {
		slog.Debug("dropped node endpoint candidates", "dropped", dropped)
	}
	return out
}

// Candidates is the node's reported endpoints with the gateway-observed one placed before its CGNAT/VPN addresses.
func (f *Fleet) Candidates(node uuid.UUID) []string {
	f.mu.Lock()
	list := slices.Clone(f.candidates[node])
	f.mu.Unlock()
	cs, ok := f.net.ChassisStatus(node)
	if !ok {
		return list
	}
	// NAT may map the node port to any port, so the observed endpoint keeps its own.
	ap, ok := validEndpoint(cs.Endpoint)
	observed := ap.String()
	if !ok || slices.Contains(list, observed) {
		return list
	}
	list = list[:min(len(list), netaddr.MaxCandidates-1)] // room for the observed endpoint, which is never the one dropped
	at := slices.IndexFunc(list, func(s string) bool {
		ap, err := netip.ParseAddrPort(s)
		return err == nil && netaddr.CGNAT.Contains(ap.Addr())
	})
	if at < 0 {
		at = len(list)
	}
	return slices.Insert(list, at, observed)
}

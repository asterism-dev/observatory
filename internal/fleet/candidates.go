package fleet

import (
	"log/slog"
	"net/netip"
	"slices"

	"github.com/google/uuid"

	"github.com/asterism-dev/observatory/internal/netaddr"
)

const maxCandidates = 8

var cgnat = netip.MustParsePrefix("100.64.0.0/10")

// validCandidates keeps the usable, distinct ip:port entries of a node-reported list, at most maxCandidates.
func validCandidates(in []string) []string {
	out := make([]string, 0, min(len(in), maxCandidates))
	for _, s := range in {
		ap, err := netip.ParseAddrPort(s)
		if err != nil || ap.Port() == 0 || !netaddr.DirectEndpointAddr(ap.Addr()) {
			continue
		}
		s = netip.AddrPortFrom(ap.Addr().Unmap(), ap.Port()).String()
		if slices.Contains(out, s) {
			continue
		}
		out = append(out, s)
		if len(out) == maxCandidates {
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
	observed := validCandidates([]string{cs.Endpoint})
	if len(observed) == 0 || slices.Contains(list, observed[0]) {
		return list
	}
	list = list[:min(len(list), maxCandidates-1)] // room for the observed endpoint, which is never the one dropped
	at := slices.IndexFunc(list, func(s string) bool {
		ap, err := netip.ParseAddrPort(s)
		return err == nil && cgnat.Contains(ap.Addr())
	})
	if at < 0 {
		at = len(list)
	}
	return slices.Insert(list, at, observed[0])
}

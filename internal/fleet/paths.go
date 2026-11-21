package fleet

import (
	"context"
	"log/slog"
	"net/netip"
	"slices"

	"github.com/google/uuid"

	"github.com/asterism-dev/observatory/internal/proto"
	"github.com/asterism-dev/observatory/internal/store"
)

const maxPaths = 256

// validPaths keeps one entry per peer node: known mode, plausible MTU and a parseable endpoint.
func validPaths(in []proto.PathStatus, peers map[string]bool) []proto.PathStatus {
	out := make([]proto.PathStatus, 0, min(len(in), len(peers)))
	seen := map[string]bool{}
	for _, p := range in[:min(len(in), maxPaths)] {
		if !peers[p.NodeID] || seen[p.NodeID] || (p.Mode != "direct" && p.Mode != "relay") {
			continue
		}
		if p.MTU != 0 && (p.MTU < 576 || p.MTU > 9000) {
			continue
		}
		if p.Endpoint != "" {
			ap, err := netip.ParseAddrPort(p.Endpoint)
			if err != nil {
				continue
			}
			p.Endpoint = ap.String()
		}
		seen[p.NodeID] = true
		out = append(out, p)
	}
	if dropped := len(in) - len(out); dropped > 0 {
		slog.Debug("dropped node path reports", "dropped", dropped)
	}
	return out
}

// Paths is how the node last reported reaching its peer nodes.
func (f *Fleet) Paths(node uuid.UUID) []proto.PathStatus {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.paths[node])
}

func (f *Fleet) storePaths(ctx context.Context, node store.Node, in []proto.PathStatus) {
	pm, err := f.peers(ctx, node.OrgID, node.ID)
	if err != nil {
		slog.Warn("node paths", "node", node.ID, "err", err)
		return
	}
	peers := make(map[string]bool, len(pm.Peers))
	for _, p := range pm.Peers {
		peers[p.NodeID] = true
	}
	valid := validPaths(in, peers)
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.conns[node.ID] != nil {
		f.paths[node.ID] = valid
	}
}

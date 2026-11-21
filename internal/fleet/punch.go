package fleet

import (
	"context"
	"log/slog"
	"slices"
	"time"

	"github.com/google/uuid"

	"github.com/asterism-dev/observatory/internal/proto"
	"github.com/asterism-dev/observatory/internal/store"
)

const (
	punchLead     = 2 * time.Second
	punchInterval = 5 * time.Minute
	punchGiveUp   = 3
	punchBackoff  = time.Hour
)

type punchPair [2]uuid.UUID

func pairOf(a, b uuid.UUID) punchPair {
	if slices.Compare(a[:], b[:]) > 0 {
		a, b = b, a
	}
	return punchPair{a, b}
}

type punchState struct {
	last     time.Time // last punch sent; zero after a backoff
	failures int
	until    time.Time // end of the backoff
}

func pathMode(paths []proto.PathStatus, peer uuid.UUID) string {
	i := slices.IndexFunc(paths, func(p proto.PathStatus) bool { return p.NodeID == peer.String() })
	if i < 0 {
		return ""
	}
	return paths[i].Mode
}

func (f *Fleet) observedEndpoint(node uuid.UUID) (string, bool) {
	cs, ok := f.net.ChassisStatus(node)
	if !ok {
		return "", false
	}
	ap, ok := validEndpoint(cs.Endpoint)
	return ap.String(), ok
}

// maybePunch asks node and each same-org peer it is relayed with, both ways, to punch towards each other's observed endpoint.
func (f *Fleet) maybePunch(ctx context.Context, org, node uuid.UUID) {
	chassis, err := f.store.OrgChassis(ctx, org)
	if err != nil {
		slog.Warn("punch peers", "node", node, "err", err)
		return
	}
	// node must be in its peers' sets too, which a quarantined node is not.
	if i := slices.IndexFunc(chassis, func(c store.Chassis) bool { return c.NodeID == node }); i < 0 || chassis[i].Quarantined {
		return
	}
	self, ok := f.observedEndpoint(node)
	if !ok {
		return
	}
	now := clock()
	at := now.Add(punchLead)
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, c := range chassis {
		peer := c.NodeID
		if c.Quarantined || peer == node {
			continue
		}
		key := pairOf(node, peer)
		mine, theirs := pathMode(f.paths[node], peer), pathMode(f.paths[peer], node)
		if mine == "direct" || theirs == "direct" {
			delete(f.punches, key)
			continue
		}
		cn, cp := f.conns[node], f.conns[peer]
		if mine != "relay" || theirs != "relay" || cn == nil || cp == nil {
			continue
		}
		st := f.punches[key]
		if now.Before(st.until) || (!st.last.IsZero() && now.Sub(st.last) < punchInterval) {
			continue
		}
		// Still relayed at the next eligible check: the previous punch failed.
		if !st.last.IsZero() {
			st.failures++
			if st.failures >= punchGiveUp {
				f.punches[key] = punchState{until: now.Add(punchBackoff)}
				continue
			}
		}
		theirEP, ok := f.observedEndpoint(peer)
		if !ok {
			continue
		}
		st.last = now
		f.punches[key] = st
		cn.punch(proto.Punch{NodeID: peer.String(), Endpoint: theirEP, At: at})
		cp.punch(proto.Punch{NodeID: node.String(), Endpoint: self, At: at})
	}
}

// forgetPunches drops the pair state of node; f.mu must be held.
func (f *Fleet) forgetPunches(node uuid.UUID) {
	for k := range f.punches {
		if k[0] == node || k[1] == node {
			delete(f.punches, k)
		}
	}
}

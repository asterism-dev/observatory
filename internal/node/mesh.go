package node

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/netip"
	"os/exec"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/asterism-dev/observatory/internal/netaddr"
	"github.com/asterism-dev/observatory/internal/proto"
	"github.com/asterism-dev/observatory/internal/wgkey"
)

const (
	meshTick      = 5 * time.Second
	directTimeout = 15 * time.Second
	directRetry   = 2 * time.Minute
	directStale   = 3 * time.Minute
	underlayIface = "wg-underlay"
)

type pathMode int

const (
	pathRelay pathMode = iota
	pathTrying
	pathDirect
)

type meshPeer struct {
	proto.Peer // validated, candidates in canonical form
	mode       pathMode
	cand       int       // index of the candidate tried or in use
	since      time.Time // start of the current try or relay; only a newer handshake proves the path
	retryAt    time.Time
	handshake  time.Time
	cleared    bool // a previous run or an older version of this peer may have left allowed-ips until set
}

// mesh moves each peer node's /32 onto its own wg-underlay entry once a handshake proves the direct path; until then the gateway's /16 relays it.
type mesh struct {
	cmd func(name string, args ...string) (string, error)
	now func() time.Time

	mu      sync.Mutex
	gateway string // the hub's wg-underlay key; never configured or removed here
	peers   map[string]*meshPeer
}

func newMesh() *mesh { return &mesh{cmd: runOutput, now: time.Now} }

func runOutput(name string, args ...string) (string, error) {
	out, err := exec.Command(name, args...).Output()
	if err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			return "", fmt.Errorf("%s %v: %w: %s", name, args, err, ee.Stderr)
		}
		return "", fmt.Errorf("%s %v: %w", name, args, err)
	}
	return string(out), nil
}

func (m *mesh) setGateway(key string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.gateway = key
}

func (m *mesh) update(p proto.Peers) {
	m.mu.Lock()
	defer m.mu.Unlock()
	next := map[string]*meshPeer{}
	keys, addrs := map[string]bool{}, map[string]bool{}
	for _, raw := range p.Peers {
		v, err := m.validPeer(raw)
		if err == nil && (next[v.NodeID] != nil || keys[v.PublicKey] || addrs[v.Address]) {
			err = errors.New("duplicate node, key or address")
		}
		if err != nil {
			slog.Warn("ignoring underlay peer", "node", raw.NodeID, "err", err)
			continue
		}
		keys[v.PublicKey], addrs[v.Address] = true, true
		if old := m.peers[v.NodeID]; old != nil && old.PublicKey == v.PublicKey && old.Address == v.Address && slices.Equal(old.Candidates, v.Candidates) {
			next[v.NodeID] = old
			continue
		}
		next[v.NodeID] = &meshPeer{Peer: v, since: m.now()} // relay with a zero retryAt: tried right away
	}
	m.peers = next
	m.reconcileLocked()
}

func (m *mesh) validPeer(p proto.Peer) (proto.Peer, error) {
	if !wgkey.Valid(p.PublicKey) || p.PublicKey == m.gateway {
		return proto.Peer{}, errors.New("invalid public key")
	}
	a, err := netip.ParseAddr(p.Address)
	if err != nil || !a.Is4() || !netaddr.UnderlayPool.Contains(a) || a == netaddr.GatewayUnderlay {
		return proto.Peer{}, fmt.Errorf("invalid underlay address %q", p.Address)
	}
	v := proto.Peer{NodeID: p.NodeID, PublicKey: p.PublicKey, Address: a.String()}
	for _, c := range p.Candidates {
		ap, err := netip.ParseAddrPort(c)
		if err != nil || ap.Port() == 0 || !netaddr.DirectEndpointAddr(ap.Addr()) {
			continue
		}
		v.Candidates = append(v.Candidates, ap.String())
	}
	return v, nil
}

func (m *mesh) run(ctx context.Context) {
	t := time.NewTicker(meshTick)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		m.tick()
	}
}

func (m *mesh) tick() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.reconcileLocked()
}

// reconcileLocked's sweep removes dropped peers and a previous run's leftovers, and retries failed removes.
func (m *mesh) reconcileLocked() {
	if m.gateway == "" {
		return
	}
	out, err := m.cmd("wg", "show", underlayIface, "latest-handshakes")
	if err != nil {
		slog.Warn("reading underlay peers", "err", err)
		return
	}
	kernel := parseHandshakes(out)
	now := m.now()
	want := map[string]bool{m.gateway: true}
	for _, p := range m.peers {
		hs, present := kernel[p.PublicKey]
		m.step(p, hs, present, now)
		if len(p.Candidates) > 0 {
			want[p.PublicKey] = true
		}
	}
	for key := range kernel {
		if want[key] {
			continue
		}
		if _, err := m.cmd("wg", "set", underlayIface, "peer", key, "remove"); err != nil {
			slog.Warn("removing underlay peer", "err", err)
		}
	}
}

func (m *mesh) step(p *meshPeer, hs time.Time, present bool, now time.Time) {
	if len(p.Candidates) == 0 {
		return
	}
	// A wg-quick restart (chassis cert renewal) drops runtime peers; a relay peer is re-added at its retry.
	if !present && p.mode != pathRelay {
		m.try(p, p.cand, now)
		return
	}
	if present {
		p.handshake = hs
	}
	proven := present && !hs.Before(p.since.Truncate(time.Second))
	switch p.mode {
	case pathRelay:
		// The other node's try handshakes with us too; routing at once keeps both sides symmetric.
		if proven {
			m.direct(p)
		} else if !now.Before(p.retryAt) {
			m.try(p, 0, now)
		}
	case pathTrying:
		switch {
		case proven:
			m.direct(p)
		case now.Sub(p.since) < directTimeout:
		case p.cand+1 < len(p.Candidates):
			m.try(p, p.cand+1, now)
		default:
			m.relay(p, now)
		}
	case pathDirect:
		if now.Sub(hs) > directStale {
			m.relay(p, now)
		}
	}
}

// try leaves allowed-ips empty so traffic keeps going through the relay; keepalives start the handshake.
func (m *mesh) try(p *meshPeer, cand int, now time.Time) {
	p.mode, p.cand, p.since = pathTrying, cand, now
	args := []string{"set", underlayIface, "peer", p.PublicKey, "endpoint", p.Candidates[cand]}
	if !p.cleared {
		args = append(args, "allowed-ips", "")
	}
	if _, err := m.cmd("wg", append(args, "persistent-keepalive", "25")...); err != nil {
		slog.Warn("configuring direct underlay path", "node", p.NodeID, "err", err)
		m.relay(p, now)
		return
	}
	p.cleared = true
}

func (m *mesh) direct(p *meshPeer) {
	if _, err := m.cmd("wg", "set", underlayIface, "peer", p.PublicKey, "allowed-ips", p.Address+"/32"); err != nil {
		slog.Warn("routing direct underlay path", "node", p.NodeID, "err", err)
		return
	}
	p.mode = pathDirect
}

// relay keeps the peer so its keepalives can still prove a path; only its /32 goes back to the gateway.
func (m *mesh) relay(p *meshPeer, now time.Time) {
	if p.mode == pathDirect {
		if _, err := m.cmd("wg", "set", underlayIface, "peer", p.PublicKey, "allowed-ips", ""); err != nil {
			slog.Warn("returning underlay path to relay", "node", p.NodeID, "err", err)
			return
		}
	}
	p.mode, p.since, p.retryAt, p.handshake = pathRelay, now, now.Add(directRetry), time.Time{}
}

func (m *mesh) status() []proto.PathStatus {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]proto.PathStatus, 0, len(m.peers))
	for _, p := range m.peers {
		s := proto.PathStatus{NodeID: p.NodeID, Mode: "relay"}
		if p.mode == pathDirect {
			s.Mode, s.Endpoint = "direct", p.Candidates[p.cand]
		}
		if !p.handshake.IsZero() {
			hs := p.handshake
			s.Handshake = &hs
		}
		out = append(out, s)
	}
	slices.SortFunc(out, func(a, b proto.PathStatus) int { return strings.Compare(a.NodeID, b.NodeID) })
	return out
}

// parseHandshakes reads `wg show <iface> latest-handshakes`; a zero time means no handshake yet.
func parseHandshakes(out string) map[string]time.Time {
	hs := map[string]time.Time{}
	for _, line := range strings.Split(out, "\n") {
		f := strings.Fields(line)
		if len(f) != 2 || !wgkey.Valid(f[0]) {
			continue
		}
		sec, err := strconv.ParseInt(f[1], 10, 64)
		if err != nil {
			continue
		}
		var t time.Time
		if sec > 0 {
			t = time.Unix(sec, 0)
		}
		hs[f[0]] = t
	}
	return hs
}

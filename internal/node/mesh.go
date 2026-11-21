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
	wgTimeout     = 5 * time.Second
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
	since      time.Time // start of the current try
	retryAt    time.Time
	handshake  time.Time
	endpoint   string // what wg reports in use, which roaming may change
	cleared    bool   // a previous run or an older version of this peer may have left allowed-ips until set
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
	ctx, cancel := context.WithTimeout(context.Background(), wgTimeout)
	defer cancel()
	out, err := exec.CommandContext(ctx, name, args...).Output()
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
		if old := m.peers[v.NodeID]; old != nil && old.PublicKey == v.PublicKey && old.Address == v.Address {
			// Rebuilding a working path would drop traffic until the next rekey.
			old.Candidates = v.Candidates
			if old.cand >= len(v.Candidates) {
				old.cand = 0
			}
			next[v.NodeID] = old
			continue
		}
		// Relay with a zero retryAt: a live kernel session from an earlier run is adopted, otherwise tried right away.
		next[v.NodeID] = &meshPeer{Peer: v}
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
	var tables [3]map[string]string
	for i, what := range []string{"latest-handshakes", "endpoints", "allowed-ips"} {
		out, err := m.cmd("wg", "show", underlayIface, what)
		if err != nil {
			slog.Warn("reading underlay peers", "err", err)
			return
		}
		tables[i] = parsePeerTable(out)
	}
	handshakes, endpoints, allowed := tables[0], tables[1], tables[2]
	now := m.now()
	want := map[string]bool{m.gateway: true}
	for _, p := range m.peers {
		hsText, present := handshakes[p.PublicKey]
		hs := parseHandshake(hsText)
		p.endpoint = ""
		if ap, err := netip.ParseAddrPort(endpoints[p.PublicKey]); err == nil {
			p.endpoint = ap.String()
		}
		m.step(p, hs, present, now)
		if len(p.Candidates) > 0 {
			want[p.PublicKey] = true
		}
	}
	for key := range handshakes {
		// The gateway is also recognised by its route, should its key ever change before wg-quick reloads it.
		if want[key] || slices.Contains(strings.Fields(allowed[key]), netaddr.UnderlayPool.String()) {
			continue
		}
		if _, err := m.cmd("wg", "set", underlayIface, "peer", key, "remove"); err != nil {
			slog.Warn("removing underlay peer", "err", err)
		}
	}
}

func (m *mesh) step(p *meshPeer, hs time.Time, present bool, now time.Time) {
	if len(p.Candidates) == 0 {
		// The sweep removes the peer, which hands its /32 back to the gateway.
		p.mode, p.handshake = pathRelay, time.Time{}
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
	// A stale handshake never proves a path, so relay fallback holds; a fresh one may predate this try (rebuild, restart).
	proven := present && !hs.IsZero() && now.Sub(hs) <= directStale
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
	p.mode, p.retryAt, p.handshake = pathRelay, now.Add(directRetry), time.Time{}
}

func (m *mesh) status() []proto.PathStatus {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]proto.PathStatus, 0, len(m.peers))
	for _, p := range m.peers {
		s := proto.PathStatus{NodeID: p.NodeID, Mode: "relay"}
		if p.mode == pathDirect {
			s.Mode, s.Endpoint = "direct", p.endpoint
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

// parsePeerTable reads `wg show <iface> <field>` lines of a peer key, a tab and the field's value.
func parsePeerTable(out string) map[string]string {
	t := map[string]string{}
	for _, line := range strings.Split(out, "\n") {
		key, val, ok := strings.Cut(line, "\t")
		if ok && wgkey.Valid(key) {
			t[key] = strings.TrimSpace(val)
		}
	}
	return t
}

// parseHandshake returns the zero time for no handshake yet or an unreadable value.
func parseHandshake(s string) time.Time {
	sec, err := strconv.ParseInt(s, 10, 64)
	if err != nil || sec <= 0 {
		return time.Time{}
	}
	return time.Unix(sec, 0)
}

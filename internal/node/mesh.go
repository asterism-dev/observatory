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
	since      time.Time // when cand was configured
	retryAt    time.Time
	handshake  time.Time
}

// mesh moves each peer node's /32 between its own wg-underlay entry (direct) and the gateway's /16 (relay).
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
		next[v.NodeID] = &meshPeer{Peer: v} // relay with a zero retryAt: tried right away
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

// reconcileLocked's sweep of unwanted kernel peers also clears a previous run's leftovers and retries failed removes.
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
		if p.mode != pathRelay {
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
	if p.mode == pathRelay {
		if len(p.Candidates) > 0 && !now.Before(p.retryAt) {
			m.try(p, 0, now)
		}
		return
	}
	// A wg-quick restart (chassis cert renewal) drops runtime peers.
	if !present {
		m.try(p, p.cand, now)
		return
	}
	p.handshake = hs
	switch {
	case p.mode == pathTrying && !hs.Before(p.since.Truncate(time.Second)):
		p.mode = pathDirect
	case p.mode == pathTrying && now.Sub(p.since) >= directTimeout:
		if p.cand+1 < len(p.Candidates) {
			m.try(p, p.cand+1, now)
		} else {
			m.relay(p, now)
		}
	case p.mode == pathDirect && now.Sub(hs) > directStale:
		m.relay(p, now)
	}
}

func (m *mesh) try(p *meshPeer, cand int, now time.Time) {
	p.mode, p.cand, p.since = pathTrying, cand, now
	_, err := m.cmd("wg", "set", underlayIface, "peer", p.PublicKey, "endpoint", p.Candidates[cand],
		"allowed-ips", p.Address+"/32", "persistent-keepalive", "25")
	if err != nil {
		slog.Warn("configuring direct underlay path", "node", p.NodeID, "err", err)
		m.relay(p, now)
	}
}

// relay leaves the removal to reconcileLocked's sweep, which returns the /32 to the gateway's /16.
func (m *mesh) relay(p *meshPeer, now time.Time) {
	p.mode, p.retryAt, p.handshake = pathRelay, now.Add(directRetry), time.Time{}
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

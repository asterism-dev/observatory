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
	mtuRetry      = 10 * time.Minute
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
	mtus    map[string]mtuResult // direct endpoint → probed path MTU
	queued  map[string]bool
	queue   []string
	wake    chan struct{}
}

type mtuResult struct {
	mtu int // 0 when the probe failed
	at  time.Time
}

type runner = func(name string, args ...string) (string, error)

func newMesh() *mesh { return &mesh{cmd: runOutput, now: time.Now, wake: make(chan struct{}, 1)} }

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
	go m.prober(ctx)
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
	m.reconcileLocked()
	now := m.now()
	used := map[string]bool{}
	for _, p := range m.peers {
		if p.mode == pathDirect && p.endpoint != "" {
			used[p.endpoint] = true
		}
	}
	for ep := range m.mtus {
		if !used[ep] {
			delete(m.mtus, ep)
		}
	}
	for ep := range used {
		r, done := m.mtus[ep]
		if (!done || (r.mtu == 0 && now.Sub(r.at) >= mtuRetry)) && !m.queued[ep] {
			if m.queued == nil {
				m.queued = map[string]bool{}
			}
			m.queued[ep] = true
			m.queue = append(m.queue, ep)
		}
	}
	m.mu.Unlock()
	select {
	case m.wake <- struct{}{}:
	default:
	}
}

// prober probes one endpoint at a time so slow pings never delay path judgment.
func (m *mesh) prober(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-m.wake:
			m.probePending()
		}
	}
}

func (m *mesh) probePending() {
	for {
		m.mu.Lock()
		if len(m.queue) == 0 {
			m.mu.Unlock()
			return
		}
		ep := m.queue[0]
		m.queue = m.queue[1:]
		m.mu.Unlock()
		mtu := 0
		if ap, err := netip.ParseAddrPort(ep); err == nil {
			mtu = probeMTU(m.cmd, ap.Addr().String())
		}
		m.mu.Lock()
		if m.mtus == nil {
			m.mtus = map[string]mtuResult{}
		}
		m.mtus[ep] = mtuResult{mtu, m.now()}
		delete(m.queued, ep)
		m.mu.Unlock()
	}
}

// probeMTU returns the largest total IP size in 1200-1500 that passes unfragmented, or 0 when even 1200 fails.
func probeMTU(run runner, ip string) int {
	// Only IPv4 literals reach ping, which has no shell.
	if a, err := netip.ParseAddr(ip); err != nil || !a.Is4() {
		return 0
	}
	ok := func(size int) bool {
		_, err := run("ping", "-M", "do", "-c", "1", "-W", "1", "-s", strconv.Itoa(size-28), ip)
		return err == nil
	}
	lo, hi := 1200, 1500
	if !ok(lo) {
		return 0
	}
	for lo < hi {
		mid := (lo + hi + 1) / 2
		if ok(mid) {
			lo = mid
		} else {
			hi = mid - 1
		}
	}
	return lo
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
		want[p.PublicKey] = true
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
		m.stepUnreachable(p, hs, present, now)
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

// stepUnreachable keeps a candidate-less peer's proven path until stale, since dropping it on one side black-holes both.
func (m *mesh) stepUnreachable(p *meshPeer, hs time.Time, present bool, now time.Time) {
	if !present {
		// The kernel peer is gone; whatever re-adds it later starts without allowed-ips.
		p.mode, p.handshake, p.cleared = pathRelay, time.Time{}, false
		return
	}
	p.handshake = hs
	proven := !hs.IsZero() && now.Sub(hs) <= directStale
	switch {
	case proven && p.mode != pathDirect:
		m.direct(p)
	case proven:
	case p.mode == pathDirect:
		m.relay(p, now)
	case !p.cleared:
		// No try will clear a previous run's leftover /32.
		if _, err := m.cmd("wg", "set", underlayIface, "peer", p.PublicKey, "allowed-ips", ""); err != nil {
			slog.Warn("returning underlay path to relay", "node", p.NodeID, "err", err)
			return
		}
		p.mode, p.cleared = pathRelay, true
	default:
		p.mode = pathRelay
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
			s.Mode, s.Endpoint, s.MTU = "direct", p.endpoint, m.mtus[p.endpoint].mtu
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

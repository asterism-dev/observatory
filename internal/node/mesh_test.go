package node

import (
	"bytes"
	"cmp"
	"encoding/base64"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/asterism-dev/observatory/internal/proto"
)

func testKey(b byte) string { return base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{b}, 32)) }

var gwKey = testKey(1)

type wgPeer struct {
	hs       int64
	endpoint string
	ips      string
}

// fakeWG models wg-underlay's runtime peers.
type fakeWG struct {
	t       *testing.T
	peers   map[string]*wgPeer
	calls   []string
	pathMTU int // largest IP packet the fake path passes; 0 means 1500
}

func (f *fakeWG) run(name string, args ...string) (string, error) {
	shown := make([]string, len(args))
	for i, a := range args {
		shown[i] = a
		if a == "" {
			shown[i] = `""`
		}
	}
	call := name + " " + strings.Join(shown, " ")
	f.calls = append(f.calls, call)
	if name == "ping" {
		return f.ping(args)
	}
	if name == "wg" && len(args) == 3 && args[0] == "show" && args[1] == "wg-underlay" {
		var out strings.Builder
		for k, p := range f.peers {
			val := map[string]string{"latest-handshakes": fmt.Sprint(p.hs), "endpoints": p.endpoint, "allowed-ips": p.ips}[args[2]]
			if val == "" {
				val = "(none)"
			}
			fmt.Fprintf(&out, "%s\t%s\n", k, val)
		}
		return out.String(), nil
	}
	if name != "wg" || len(args) < 4 || args[0] != "set" || args[1] != "wg-underlay" || args[2] != "peer" {
		f.t.Fatalf("unexpected command %q", call)
	}
	key, rest := args[3], args[4:]
	if len(rest) == 1 && rest[0] == "remove" {
		delete(f.peers, key)
		return "", nil
	}
	if len(rest)%2 != 0 {
		f.t.Fatalf("odd wg set arguments %q", call)
	}
	p := f.peers[key]
	if p == nil {
		p = &wgPeer{}
		f.peers[key] = p
	}
	for i := 0; i < len(rest); i += 2 {
		switch rest[i] {
		case "endpoint":
			p.endpoint = rest[i+1]
		case "allowed-ips":
			p.ips = rest[i+1]
		case "persistent-keepalive":
		default:
			f.t.Fatalf("unexpected wg set option in %q", call)
		}
	}
	return "", nil
}

func (f *fakeWG) took(call string) bool { return slices.Contains(f.calls, call) }

func (f *fakeWG) routed() bool {
	for _, c := range f.calls {
		if strings.Contains(c, "/32") {
			return true
		}
	}
	return false
}

type meshEnv struct {
	m   *mesh
	wg  *fakeWG
	now time.Time
}

func newMeshEnv(t *testing.T) *meshEnv {
	e := &meshEnv{wg: &fakeWG{t: t, peers: map[string]*wgPeer{gwKey: {hs: 1, ips: "10.198.0.0/16"}}}, now: time.Unix(1_800_000_000, 0)}
	e.m = &mesh{cmd: e.wg.run, now: func() time.Time { return e.now }, gateway: gwKey}
	return e
}

func (e *meshEnv) advance(d time.Duration) {
	e.now = e.now.Add(d)
	e.m.tick()
}

func (e *meshEnv) handshake(key string) { e.wg.peers[key].hs = e.now.Unix() }

func (e *meshEnv) path(t *testing.T, nodeID string) proto.PathStatus {
	t.Helper()
	for _, s := range e.m.status() {
		if s.NodeID == nodeID {
			return s
		}
	}
	t.Fatalf("no path for %s", nodeID)
	return proto.PathStatus{}
}

var peerA = proto.Peer{NodeID: "a", PublicKey: testKey(2), Address: "10.198.0.5", Candidates: []string{"192.168.1.5:51822", "203.0.113.5:51822"}}

// firstTry also clears routes a previous run or an older version of the peer may have left.
func firstTry(p proto.Peer, cand string) string {
	return "wg set wg-underlay peer " + p.PublicKey + " endpoint " + cand + ` allowed-ips "" persistent-keepalive 25`
}

func trySet(p proto.Peer, cand string) string {
	return "wg set wg-underlay peer " + p.PublicKey + " endpoint " + cand + " persistent-keepalive 25"
}

func route(p proto.Peer) string {
	return "wg set wg-underlay peer " + p.PublicKey + " allowed-ips " + p.Address + "/32"
}

func unroute(p proto.Peer) string {
	return "wg set wg-underlay peer " + p.PublicKey + ` allowed-ips ""`
}

func removeCall(key string) string { return "wg set wg-underlay peer " + key + " remove" }

func TestMeshGoesDirectOnHandshake(t *testing.T) {
	e := newMeshEnv(t)
	e.m.update(proto.Peers{Peers: []proto.Peer{peerA}})
	if !e.wg.took(firstTry(peerA, "192.168.1.5:51822")) {
		t.Fatalf("first candidate not tried: %v", e.wg.calls)
	}
	e.advance(5 * time.Second)
	if e.wg.routed() || e.wg.peers[peerA.PublicKey].ips != "" {
		t.Fatalf("/32 moved off the relay before a handshake: %v", e.wg.calls)
	}
	if s := e.path(t, "a"); s.Mode != "relay" {
		t.Fatalf("mode while trying = %q, want relay", s.Mode)
	}
	e.handshake(peerA.PublicKey)
	e.wg.peers[peerA.PublicKey].endpoint = "203.0.113.77:6000" // roamed
	e.advance(5 * time.Second)
	s := e.path(t, "a")
	if s.Mode != "direct" || s.Endpoint != "203.0.113.77:6000" || s.Handshake == nil {
		t.Fatalf("path = %+v, want direct via first candidate", s)
	}
	if !e.wg.took(route(peerA)) || e.wg.peers[peerA.PublicKey].ips != "10.198.0.5/32" {
		t.Fatalf("/32 not routed to the peer after its handshake: %v", e.wg.calls)
	}
}

func TestMeshTriesNextCandidate(t *testing.T) {
	e := newMeshEnv(t)
	e.m.update(proto.Peers{Peers: []proto.Peer{peerA}})
	e.advance(10 * time.Second)
	if e.wg.took(trySet(peerA, "203.0.113.5:51822")) {
		t.Fatal("second candidate tried before the timeout")
	}
	e.advance(5 * time.Second)
	if !e.wg.took(trySet(peerA, "203.0.113.5:51822")) {
		t.Fatalf("second candidate not tried after 15s: %v", e.wg.calls)
	}
	if e.wg.routed() {
		t.Fatalf("/32 moved off the relay while trying: %v", e.wg.calls)
	}
	e.handshake(peerA.PublicKey)
	e.advance(5 * time.Second)
	if s := e.path(t, "a"); s.Mode != "direct" || s.Endpoint != "203.0.113.5:51822" {
		t.Fatalf("path = %+v, want direct via second candidate", s)
	}
}

func TestMeshFallsBackToRelay(t *testing.T) {
	e := newMeshEnv(t)
	e.m.update(proto.Peers{Peers: []proto.Peer{peerA}})
	e.advance(15 * time.Second)
	e.advance(15 * time.Second)
	if s := e.path(t, "a"); s.Mode != "relay" {
		t.Fatalf("mode = %q, want relay", s.Mode)
	}
	if e.wg.routed() || e.wg.took(removeCall(peerA.PublicKey)) {
		t.Fatalf("relay fallback routed or removed the peer: %v", e.wg.calls)
	}
	if p := e.wg.peers[peerA.PublicKey]; p == nil || p.ips != "" {
		t.Fatalf("peer = %+v, want kept without allowed-ips", p)
	}
}

func TestMeshRetriesAfterTwoMinutes(t *testing.T) {
	e := newMeshEnv(t)
	e.m.update(proto.Peers{Peers: []proto.Peer{peerA}})
	e.advance(15 * time.Second)
	e.advance(15 * time.Second)
	e.wg.calls = nil
	e.advance(2*time.Minute - 5*time.Second)
	if e.wg.took(trySet(peerA, "192.168.1.5:51822")) {
		t.Fatal("retried before directRetry")
	}
	e.advance(5 * time.Second)
	if !e.wg.took(trySet(peerA, "192.168.1.5:51822")) {
		t.Fatalf("not retried after directRetry: %v", e.wg.calls)
	}
}

func TestMeshRelayPeerGoesDirectOnHandshake(t *testing.T) {
	e := newMeshEnv(t)
	e.m.update(proto.Peers{Peers: []proto.Peer{peerA}})
	e.advance(15 * time.Second)
	e.advance(15 * time.Second)
	e.advance(10 * time.Second)
	// The other node's try reached us: both sides share the handshake, so this side routes too.
	e.handshake(peerA.PublicKey)
	e.advance(5 * time.Second)
	if s := e.path(t, "a"); s.Mode != "direct" || !e.wg.took(route(peerA)) {
		t.Fatalf("path = %+v after a handshake while on relay, want direct", s)
	}
}

func TestMeshStaleHandshakeFallsBack(t *testing.T) {
	e := newMeshEnv(t)
	e.m.update(proto.Peers{Peers: []proto.Peer{peerA}})
	e.handshake(peerA.PublicKey)
	e.advance(5 * time.Second)
	e.advance(2 * time.Minute)
	e.handshake(peerA.PublicKey)
	e.advance(3 * time.Minute)
	if s := e.path(t, "a"); s.Mode != "direct" {
		t.Fatalf("mode = %q with a 3m old handshake, want direct", s.Mode)
	}
	e.advance(5 * time.Second)
	if s := e.path(t, "a"); s.Mode != "relay" {
		t.Fatalf("mode = %q with a stale handshake, want relay", s.Mode)
	}
	if !e.wg.took(unroute(peerA)) || e.wg.took(removeCall(peerA.PublicKey)) {
		t.Fatalf("stale peer: want allowed-ips cleared and peer kept: %v", e.wg.calls)
	}
	if p := e.wg.peers[peerA.PublicKey]; p == nil || p.ips != "" {
		t.Fatalf("peer = %+v, want kept without allowed-ips", p)
	}
	e.advance(5 * time.Second)
	if s := e.path(t, "a"); s.Mode != "relay" {
		t.Fatalf("old handshake promoted the relay peer again: %+v", s)
	}
}

func TestMeshRemovesDroppedPeer(t *testing.T) {
	e := newMeshEnv(t)
	e.m.update(proto.Peers{Peers: []proto.Peer{peerA}})
	e.m.update(proto.Peers{})
	if !e.wg.took(removeCall(peerA.PublicKey)) {
		t.Fatalf("dropped peer not removed: %v", e.wg.calls)
	}
	if len(e.m.status()) != 0 {
		t.Fatalf("status = %+v, want empty", e.m.status())
	}
}

func TestMeshReappliesAfterWGRestart(t *testing.T) {
	e := newMeshEnv(t)
	e.m.update(proto.Peers{Peers: []proto.Peer{peerA}})
	e.advance(15 * time.Second)
	e.handshake(peerA.PublicKey)
	e.advance(5 * time.Second)
	// wg-quick restart: only the gateway peer from the config file is left.
	e.wg.peers = map[string]*wgPeer{gwKey: {hs: e.now.Unix(), ips: "10.198.0.0/16"}}
	e.wg.calls = nil
	e.advance(5 * time.Second)
	if !e.wg.took(trySet(peerA, "203.0.113.5:51822")) {
		t.Fatalf("direct peer not re-applied after restart: %v", e.wg.calls)
	}
	e.handshake(peerA.PublicKey)
	e.advance(5 * time.Second)
	if !e.wg.took(route(peerA)) {
		t.Fatalf("re-applied peer not routed after its handshake: %v", e.wg.calls)
	}
}

func TestMeshClearsLeftoverRoute(t *testing.T) {
	for name, age := range map[string]time.Duration{"stale": 4 * time.Minute, "none": 0} {
		t.Run(name, func(t *testing.T) {
			e := newMeshEnv(t)
			leftover := &wgPeer{ips: "10.198.0.5/32", endpoint: "192.168.1.5:51822"}
			if age > 0 {
				leftover.hs = e.now.Add(-age).Unix()
			}
			e.wg.peers[peerA.PublicKey] = leftover
			e.m.update(proto.Peers{Peers: []proto.Peer{peerA}})
			if p := e.wg.peers[peerA.PublicKey]; p.ips != "" {
				t.Fatalf("leftover /32 without a fresh handshake kept: %+v", p)
			}
		})
	}
}

func TestMeshAdoptsLiveSessionAfterRestart(t *testing.T) {
	e := newMeshEnv(t)
	e.wg.peers[peerA.PublicKey] = &wgPeer{hs: e.now.Unix() - 30, ips: "10.198.0.5/32", endpoint: "198.51.100.9:40000"}
	e.m.update(proto.Peers{Peers: []proto.Peer{peerA}})
	e.advance(5 * time.Second)
	if e.wg.took(unroute(peerA)) || slices.ContainsFunc(e.wg.calls, func(c string) bool { return strings.Contains(c, `allowed-ips ""`) }) {
		t.Fatalf("live direct session cleared: %v", e.wg.calls)
	}
	if p := e.wg.peers[peerA.PublicKey]; p.ips != "10.198.0.5/32" {
		t.Fatalf("peer = %+v, want its /32 kept", p)
	}
	if s := e.path(t, "a"); s.Mode != "direct" || s.Endpoint != "198.51.100.9:40000" {
		t.Fatalf("path = %+v, want direct via the endpoint wg reports", s)
	}
}

func TestMeshCandidateChangeKeepsDirect(t *testing.T) {
	e := newMeshEnv(t)
	e.m.update(proto.Peers{Peers: []proto.Peer{peerA}})
	e.handshake(peerA.PublicKey)
	e.advance(5 * time.Second)
	e.wg.calls = nil
	changed := peerA
	changed.Candidates = []string{"192.0.2.7:51822"}
	e.m.update(proto.Peers{Peers: []proto.Peer{changed}})
	e.advance(5 * time.Second)
	if slices.ContainsFunc(e.wg.calls, func(c string) bool { return strings.HasPrefix(c, "wg set") }) {
		t.Fatalf("direct peer rebuilt on a candidate change: %v", e.wg.calls)
	}
	if s := e.path(t, "a"); s.Mode != "direct" || e.wg.peers[peerA.PublicKey].ips != "10.198.0.5/32" {
		t.Fatalf("path = %+v, want direct with its /32", s)
	}
	e.advance(3 * time.Minute)
	e.advance(2 * time.Minute)
	if !e.wg.took(trySet(changed, "192.0.2.7:51822")) {
		t.Fatalf("new candidates not used on the next retry: %v", e.wg.calls)
	}
}

func TestMeshKeepsListedPeerWithoutCandidates(t *testing.T) {
	e := newMeshEnv(t)
	e.m.update(proto.Peers{Peers: []proto.Peer{peerA}})
	e.handshake(peerA.PublicKey)
	e.advance(5 * time.Second)
	e.wg.calls = nil
	unknown := peerA
	unknown.Candidates = nil
	e.m.update(proto.Peers{Peers: []proto.Peer{unknown}})
	for range 24 {
		e.handshake(peerA.PublicKey)
		e.advance(5 * time.Second)
	}
	if s := e.path(t, "a"); s.Mode != "direct" || e.wg.peers[peerA.PublicKey] == nil || e.wg.peers[peerA.PublicKey].ips != "10.198.0.5/32" {
		t.Fatalf("path = %+v, peer = %+v, want direct with its /32 while the handshake is fresh", s, e.wg.peers[peerA.PublicKey])
	}
	e.advance(3*time.Minute + 5*time.Second)
	if s := e.path(t, "a"); s.Mode != "relay" {
		t.Fatalf("mode = %q with a stale handshake, want relay", s.Mode)
	}
	if p := e.wg.peers[peerA.PublicKey]; p == nil || p.ips != "" {
		t.Fatalf("peer = %+v, want kept without allowed-ips", p)
	}
	e.advance(5 * time.Minute)
	if e.wg.took(removeCall(peerA.PublicKey)) || slices.ContainsFunc(e.wg.calls, func(c string) bool { return strings.Contains(c, " endpoint ") }) {
		t.Fatalf("listed peer without candidates removed or tried: %v", e.wg.calls)
	}
	e.m.update(proto.Peers{})
	if !e.wg.took(removeCall(peerA.PublicKey)) {
		t.Fatalf("dropped peer not removed: %v", e.wg.calls)
	}
}

func TestMeshAddsPeerWithoutCandidatesKeyOnly(t *testing.T) {
	e := newMeshEnv(t)
	unknown := peerA
	unknown.Candidates = nil
	e.m.update(proto.Peers{Peers: []proto.Peer{unknown}})
	keyOnly := "wg set wg-underlay peer " + peerA.PublicKey
	if p := e.wg.peers[peerA.PublicKey]; p == nil || p.ips != "" || p.endpoint != "" || !e.wg.took(keyOnly) {
		t.Fatalf("peer = %+v, want added key-only: %v", p, e.wg.calls)
	}
	e.wg.peers = map[string]*wgPeer{gwKey: {hs: e.now.Unix(), ips: "10.198.0.0/16"}}
	e.wg.calls = nil
	e.advance(5 * time.Second)
	if p := e.wg.peers[peerA.PublicKey]; p == nil || p.ips != "" || !e.wg.took(keyOnly) {
		t.Fatalf("peer = %+v after wg-quick restart, want re-added key-only: %v", p, e.wg.calls)
	}
}

func TestMeshClearsLeftoverRouteWithoutCandidates(t *testing.T) {
	e := newMeshEnv(t)
	e.wg.peers[peerA.PublicKey] = &wgPeer{hs: e.now.Add(-4 * time.Minute).Unix(), ips: "10.198.0.5/32", endpoint: "192.168.1.5:51822"}
	unknown := peerA
	unknown.Candidates = nil
	e.m.update(proto.Peers{Peers: []proto.Peer{unknown}})
	if p := e.wg.peers[peerA.PublicKey]; p == nil || p.ips != "" {
		t.Fatalf("peer = %+v, want kept without its stale /32", p)
	}
}

func TestMeshSweepSparesGatewayRoute(t *testing.T) {
	e := newMeshEnv(t)
	e.m.gateway = testKey(8) // the hub's key as the agent last saw it, not yet what wg-quick loaded
	e.advance(5 * time.Second)
	if _, ok := e.wg.peers[gwKey]; !ok || e.wg.took(removeCall(gwKey)) {
		t.Fatalf("peer holding the underlay /16 removed: %v", e.wg.calls)
	}
}

func TestMeshRemovesUnknownPeers(t *testing.T) {
	e := newMeshEnv(t)
	stale := testKey(9)
	e.wg.peers[stale] = &wgPeer{hs: 1}
	e.advance(5 * time.Second)
	if !e.wg.took(removeCall(stale)) {
		t.Fatalf("leftover peer not removed: %v", e.wg.calls)
	}
}

func TestMeshNeverTouchesGatewayPeer(t *testing.T) {
	e := newMeshEnv(t)
	e.m.update(proto.Peers{Peers: []proto.Peer{
		{NodeID: "gwkey", PublicKey: gwKey, Address: "10.198.0.9", Candidates: []string{"192.168.1.9:51822"}},
		{NodeID: "gwaddr", PublicKey: testKey(3), Address: "10.198.0.1", Candidates: []string{"192.168.1.9:51822"}},
		{NodeID: "outside", PublicKey: testKey(4), Address: "10.1.0.1", Candidates: []string{"192.168.1.9:51822"}},
		{NodeID: "badkey", PublicKey: "x --help", Address: "10.198.0.10", Candidates: []string{"192.168.1.9:51822"}},
		{NodeID: "prefix", PublicKey: testKey(5), Address: "10.198.0.11/16", Candidates: []string{"192.168.1.9:51822"}},
		{NodeID: "badcands", PublicKey: testKey(6), Address: "10.198.0.12", Candidates: []string{"10.198.0.1:51820", "1.2.3.4", "x;rm:1", "127.0.0.1:51822", "1.2.3.4:0", "198.51.100.7:51822"}},
		{NodeID: "nocands", PublicKey: testKey(7), Address: "10.198.0.13"},
	}})
	for range 60 {
		e.advance(5 * time.Second)
	}
	for _, c := range e.wg.calls {
		if strings.Contains(c, gwKey) || strings.Contains(c, "10.198.0.1/") || strings.Contains(c, "10.198.0.1:") {
			t.Fatalf("gateway touched: %q", c)
		}
		if strings.Contains(c, testKey(7)) && c != "wg set wg-underlay peer "+testKey(7) && !strings.HasPrefix(c, "wg show") {
			t.Fatalf("peer without candidates configured beyond its key: %q", c)
		}
		for _, bad := range []string{"10.1.0.1", "x --help", "10.198.0.11", "1.2.3.4", "x;rm", "127.0.0.1"} {
			if strings.Contains(c, bad) {
				t.Fatalf("unvalidated value %q passed to wg: %q", bad, c)
			}
		}
	}
	if !e.wg.took(firstTry(proto.Peer{PublicKey: testKey(6)}, "198.51.100.7:51822")) {
		t.Fatalf("valid candidate of a peer with bad ones not tried: %v", e.wg.calls)
	}
	if gw := e.wg.peers[gwKey]; gw == nil || gw.ips != "10.198.0.0/16" {
		t.Fatalf("gateway peer changed: %+v", gw)
	}
	if s := e.path(t, "nocands"); s.Mode != "relay" {
		t.Fatalf("peer without candidates: mode %q, want relay", s.Mode)
	}
}

func TestAgentAppliesPeers(t *testing.T) {
	e := newMeshEnv(t)
	a := &Agent{mesh: e.m}
	a.handle(t.Context(), proto.Message{Type: proto.TypePeers, Peers: &proto.Peers{Peers: []proto.Peer{peerA}}}, nil)
	if !e.wg.took(firstTry(peerA, "192.168.1.5:51822")) {
		t.Fatalf("peers message not applied: %v", e.wg.calls)
	}
}

func (f *fakeWG) ping(args []string) (string, error) {
	want := []string{"-M", "do", "-c", "1", "-W", "1", "-s"}
	if len(args) != 9 || !slices.Equal(args[:7], want) {
		f.t.Fatalf("unexpected ping %q", args)
	}
	size, err := strconv.Atoi(args[7])
	if err != nil {
		f.t.Fatalf("ping size %q", args[7])
	}
	if limit := cmp.Or(f.pathMTU, 1500); size+28 > limit {
		return "", errors.New("message too long")
	}
	return "", nil
}

func (f *fakeWG) pings() (n int) {
	for _, c := range f.calls {
		if strings.HasPrefix(c, "ping ") {
			n++
		}
	}
	return
}

func TestProbeMTUBinarySearch(t *testing.T) {
	for limit, want := range map[int]int{1420: 1420, 1500: 1500, 1200: 1200, 1199: 0, 1350: 1350} {
		f := &fakeWG{t: t, pathMTU: limit}
		if got := probeMTU(f.run, "203.0.113.5"); got != want {
			t.Errorf("limit %d: probeMTU = %d, want %d", limit, got, want)
		}
	}
	f := &fakeWG{t: t}
	if got := probeMTU(f.run, "not-an-ip; reboot"); got != 0 || len(f.calls) != 0 {
		t.Fatalf("invalid ip probed: %d %v", got, f.calls)
	}
}

func TestMeshProbesDirectPathMTUOnce(t *testing.T) {
	e := newMeshEnv(t)
	e.wg.pathMTU = 1420
	e.m.update(proto.Peers{Peers: []proto.Peer{peerA}})
	if e.wg.pings() != 0 || e.path(t, "a").MTU != 0 {
		t.Fatalf("probed a relay path: %v", e.wg.calls)
	}
	e.advance(time.Second)
	e.handshake(peerA.PublicKey)
	e.wg.peers[peerA.PublicKey].endpoint = "192.168.1.5:51822"
	e.advance(meshTick)
	if e.wg.pings() != 0 {
		t.Fatal("tick probed inline")
	}
	e.m.probePending()
	if s := e.path(t, "a"); s.Mode != "direct" || s.MTU != 1420 {
		t.Fatalf("path = %+v", s)
	}
	n := e.wg.pings()
	e.handshake(peerA.PublicKey)
	e.advance(meshTick)
	e.m.probePending()
	if e.wg.pings() != n {
		t.Fatal("probed the same endpoint again")
	}
	e.wg.peers[peerA.PublicKey].endpoint = "192.168.1.6:51822"
	e.handshake(peerA.PublicKey)
	e.wg.pathMTU = 1400
	e.advance(meshTick)
	e.m.probePending()
	if s := e.path(t, "a"); s.MTU != 1400 {
		t.Fatalf("new endpoint not probed: %+v", s)
	}
}

func TestMeshRetriesFailedProbeAndPrunes(t *testing.T) {
	e := newMeshEnv(t)
	e.wg.pathMTU = 1100
	e.m.update(proto.Peers{Peers: []proto.Peer{peerA}})
	e.advance(time.Second)
	e.handshake(peerA.PublicKey)
	e.wg.peers[peerA.PublicKey].endpoint = "192.168.1.5:51822"
	e.advance(meshTick)
	e.m.probePending()
	if s := e.path(t, "a"); s.Mode != "direct" || s.MTU != 0 {
		t.Fatalf("path = %+v", s)
	}
	n := e.wg.pings()
	e.wg.pathMTU = 1420
	for range 9 {
		e.handshake(peerA.PublicKey)
		e.advance(time.Minute)
	}
	e.m.probePending()
	if e.wg.pings() != n {
		t.Fatal("retried before mtuRetry")
	}
	e.handshake(peerA.PublicKey)
	e.advance(time.Minute)
	e.m.probePending()
	if s := e.path(t, "a"); s.MTU != 1420 {
		t.Fatalf("failed probe not retried: %+v", s)
	}
	e.wg.peers[peerA.PublicKey].endpoint = "192.168.1.6:51822"
	e.advance(meshTick)
	if _, ok := e.m.mtus["192.168.1.5:51822"]; ok {
		t.Fatal("stale endpoint kept")
	}
}

func TestPathsChangedIgnoresHandshake(t *testing.T) {
	t1, t2 := time.Unix(1, 0), time.Unix(2, 0)
	base := []proto.PathStatus{{NodeID: "a", Mode: "direct", Endpoint: "192.168.1.5:51822", MTU: 1420, Handshake: &t1}}
	rekeyed := []proto.PathStatus{{NodeID: "a", Mode: "direct", Endpoint: "192.168.1.5:51822", MTU: 1420, Handshake: &t2}}
	if pathsChanged(base, rekeyed) {
		t.Fatal("a new handshake alone counted as a change")
	}
	relayed := []proto.PathStatus{{NodeID: "a", Mode: "relay", Handshake: &t2}}
	if !pathsChanged(base, relayed) || !pathsChanged(base, nil) || !pathsChanged(nil, base) {
		t.Fatal("a mode or peer change not detected")
	}
}

const punchEP = "198.51.100.20:40000"

func punchSet(p proto.Peer, ep string) string {
	return "wg set wg-underlay peer " + p.PublicKey + " endpoint " + ep + " persistent-keepalive 1"
}

func keepalive0(p proto.Peer) string {
	return "wg set wg-underlay peer " + p.PublicKey + " persistent-keepalive 0"
}

// startedPunch reports whether the punch went 0 → 1 in two calls, since only that change makes the kernel send at once.
func (f *fakeWG) startedPunch(p proto.Peer, ep string) bool {
	i := slices.Index(f.calls, keepalive0(p))
	return i >= 0 && i+1 < len(f.calls) && f.calls[i+1] == punchSet(p, ep)
}

func keepalive25(p proto.Peer) string {
	return "wg set wg-underlay peer " + p.PublicKey + " persistent-keepalive 25"
}

// relayedEnv has peerA on relay after both candidates failed, with its next retry two minutes away.
func relayedEnv(t *testing.T) *meshEnv {
	e := newMeshEnv(t)
	e.m.update(proto.Peers{Peers: []proto.Peer{peerA}})
	e.advance(15 * time.Second)
	e.advance(15 * time.Second)
	e.wg.calls = nil
	return e
}

func (e *meshEnv) punch(ep string, at time.Time) {
	e.m.punch(proto.Punch{NodeID: "a", Endpoint: ep, At: at}, e.now)
}

func TestPunchSetsEndpointAndFastKeepalive(t *testing.T) {
	e := newMeshEnv(t)
	e.m.update(proto.Peers{Peers: []proto.Peer{peerA}})
	e.advance(5 * time.Second)
	e.wg.calls = nil
	// Mid-try: without the window holding it, the try timeout at 15s would move on to the next candidate.
	e.punch(punchEP, e.now.Add(2*time.Second))
	e.advance(time.Second)
	if e.wg.took(punchSet(peerA, punchEP)) {
		t.Fatal("punch started before its time")
	}
	e.advance(time.Second)
	if !e.wg.startedPunch(peerA, punchEP) || e.wg.peers[peerA.PublicKey].endpoint != punchEP {
		t.Fatalf("punch not started at its time: %v", e.wg.calls)
	}
	e.wg.calls = nil
	for range 8 {
		e.advance(time.Second)
	}
	if slices.ContainsFunc(e.wg.calls, func(c string) bool { return strings.HasPrefix(c, "wg set") }) || e.wg.peers[peerA.PublicKey].endpoint != punchEP {
		t.Fatalf("regular step touched the peer during the punch window: %v", e.wg.calls)
	}
}

func TestPunchRestoresKeepalive(t *testing.T) {
	e := relayedEnv(t)
	e.punch(punchEP, e.now)
	if !e.wg.took(punchSet(peerA, punchEP)) {
		t.Fatalf("due punch not started at once: %v", e.wg.calls)
	}
	e.advance(9 * time.Second)
	if e.wg.took(keepalive25(peerA)) {
		t.Fatal("keepalive restored before the window ended")
	}
	e.advance(time.Second)
	if !e.wg.took(keepalive25(peerA)) {
		t.Fatalf("keepalive not restored after the window: %v", e.wg.calls)
	}
	if s := e.path(t, "a"); s.Mode != "relay" {
		t.Fatalf("mode = %q after a failed punch, want relay", s.Mode)
	}

	e = relayedEnv(t)
	e.punch(punchEP, e.now)
	e.m.update(proto.Peers{})
	e.advance(10 * time.Second)
	if e.wg.took(keepalive25(peerA)) || e.wg.peers[peerA.PublicKey] != nil {
		t.Fatalf("removed peer re-added by the keepalive restore: %v", e.wg.calls)
	}
}

func TestPunchNeverSetsAllowedIPsBeforeHandshake(t *testing.T) {
	e := relayedEnv(t)
	e.punch(punchEP, e.now.Add(2*time.Second))
	for range 15 {
		e.advance(time.Second)
	}
	if !e.wg.took(punchSet(peerA, punchEP)) {
		t.Fatalf("punch not started: %v", e.wg.calls)
	}
	if e.wg.routed() || e.wg.peers[peerA.PublicKey].ips != "" {
		t.Fatalf("/32 moved off the relay without a handshake: %v", e.wg.calls)
	}
}

func TestPunchHandshakeMakesDirect(t *testing.T) {
	e := relayedEnv(t)
	e.punch(punchEP, e.now)
	e.advance(3 * time.Second)
	e.handshake(peerA.PublicKey)
	e.wg.peers[peerA.PublicKey].endpoint = "198.51.100.20:40001" // the NAT mapped another port
	e.advance(time.Second)
	if s := e.path(t, "a"); s.Mode != "direct" || s.Endpoint != "198.51.100.20:40001" || !e.wg.took(route(peerA)) {
		t.Fatalf("path = %+v after a handshake in the punch window, want direct: %v", s, e.wg.calls)
	}
	e.advance(6 * time.Second)
	if s := e.path(t, "a"); s.Mode != "direct" || !e.wg.took(keepalive25(peerA)) {
		t.Fatalf("path = %+v after the window, want direct with keepalive 25: %v", s, e.wg.calls)
	}
}

func TestPunchIgnoredForDirectPeer(t *testing.T) {
	e := newMeshEnv(t)
	e.m.update(proto.Peers{Peers: []proto.Peer{peerA}})
	e.handshake(peerA.PublicKey)
	e.advance(5 * time.Second)
	e.wg.calls = nil
	e.punch(punchEP, e.now)
	e.punch(punchEP, e.now.Add(2*time.Second))
	for range 12 {
		e.advance(time.Second)
	}
	if slices.ContainsFunc(e.wg.calls, func(c string) bool { return strings.HasPrefix(c, "wg set") }) {
		t.Fatalf("punch reconfigured a direct peer: %v", e.wg.calls)
	}
}

func TestPunchRejectsInvalidEndpoint(t *testing.T) {
	for _, ep := range []string{"10.198.0.5:51822", "[2606:4700::1%eth0]:51822", "x;reboot", "198.51.100.20", "198.51.100.20:0"} {
		e := relayedEnv(t)
		e.punch(ep, e.now)
		e.advance(2 * time.Second)
		for _, c := range e.wg.calls {
			if strings.Contains(c, "persistent-keepalive 1") || strings.Contains(c, ep) {
				t.Fatalf("invalid endpoint %q punched: %q", ep, c)
			}
		}
	}
}

func TestPunchIgnoredForUnknownPeer(t *testing.T) {
	e := relayedEnv(t)
	e.m.punch(proto.Punch{NodeID: "other-org", Endpoint: punchEP, At: e.now}, e.now)
	e.advance(2 * time.Second)
	if slices.ContainsFunc(e.wg.calls, func(c string) bool { return strings.HasPrefix(c, "wg set") }) {
		t.Fatalf("punch for a node outside the peer set applied: %v", e.wg.calls)
	}
}

func TestPunchClockSkewGuard(t *testing.T) {
	for name, tc := range map[string]struct {
		offset time.Duration
		want   bool
	}{
		"far future":  {31 * time.Second, false},
		"far past":    {-31 * time.Second, false},
		"past window": {-10 * time.Second, false},
		"near future": {29 * time.Second, true},
		"in window":   {-5 * time.Second, true},
	} {
		t.Run(name, func(t *testing.T) {
			e := relayedEnv(t)
			at := e.now.Add(tc.offset)
			e.punch(punchEP, at)
			for range 30 {
				e.advance(time.Second)
			}
			if got := e.wg.took(punchSet(peerA, punchEP)); got != tc.want {
				t.Fatalf("punched = %v, want %v: %v", got, tc.want, e.wg.calls)
			}
		})
	}
	e := relayedEnv(t)
	e.punch(punchEP, e.now.Add(-5*time.Second))
	if !e.wg.took(punchSet(peerA, punchEP)) {
		t.Fatalf("punch inside its window not started at once: %v", e.wg.calls)
	}
	e.advance(4 * time.Second)
	if e.wg.took(keepalive25(peerA)) {
		t.Fatal("late punch ended early")
	}
	e.advance(time.Second)
	if !e.wg.took(keepalive25(peerA)) {
		t.Fatalf("late punch window not kept to its start time: %v", e.wg.calls)
	}
}

func TestAgentAppliesPunch(t *testing.T) {
	e := relayedEnv(t)
	a := &Agent{mesh: e.m}
	a.handle(t.Context(), proto.Message{Type: proto.TypePunch, Punch: &proto.Punch{NodeID: "a", Endpoint: punchEP, At: e.now}}, nil)
	if !e.wg.took(punchSet(peerA, punchEP)) {
		t.Fatalf("punch message not applied: %v", e.wg.calls)
	}
}

func TestPunchPeerWithoutCandidates(t *testing.T) {
	unknown := peerA
	unknown.Candidates = nil
	e := newMeshEnv(t)
	e.m.update(proto.Peers{Peers: []proto.Peer{unknown}})
	e.advance(5 * time.Second)
	e.wg.calls = nil
	e.punch(punchEP, e.now)
	if !e.wg.startedPunch(peerA, punchEP) {
		t.Fatalf("punch not started for a peer without candidates: %v", e.wg.calls)
	}
	e.wg.calls = nil
	for range 9 {
		e.advance(time.Second)
	}
	if slices.ContainsFunc(e.wg.calls, func(c string) bool { return strings.HasPrefix(c, "wg set") }) ||
		e.m.peers["a"].mode != pathPunching || e.wg.peers[peerA.PublicKey].endpoint != punchEP {
		t.Fatalf("punch window not held: mode %v, %v", e.m.peers["a"].mode, e.wg.calls)
	}
	e.advance(time.Second)
	if !e.wg.took(keepalive25(peerA)) || e.wg.routed() {
		t.Fatalf("want keepalive 25 restored and no route after the window: %v", e.wg.calls)
	}
	if s := e.path(t, "a"); s.Mode != "relay" {
		t.Fatalf("mode = %q after a failed punch, want relay", s.Mode)
	}

	e = newMeshEnv(t)
	e.m.update(proto.Peers{Peers: []proto.Peer{unknown}})
	e.punch(punchEP, e.now)
	e.advance(3 * time.Second)
	e.handshake(peerA.PublicKey)
	e.advance(time.Second)
	if s := e.path(t, "a"); s.Mode != "direct" || !e.wg.took(route(peerA)) {
		t.Fatalf("path = %+v after a handshake in the punch window, want direct: %v", s, e.wg.calls)
	}
}

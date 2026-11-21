package node

import (
	"bytes"
	"encoding/base64"
	"fmt"
	"slices"
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
	t     *testing.T
	peers map[string]*wgPeer
	calls []string
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
	if name != "wg" || len(args) < 5 || args[0] != "set" || args[1] != "wg-underlay" || args[2] != "peer" {
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
		for _, bad := range []string{"10.1.0.1", "x --help", "10.198.0.11", "1.2.3.4", "x;rm", "127.0.0.1", testKey(7)} {
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

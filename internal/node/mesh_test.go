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

// fakeWG models wg-underlay's runtime peers and their latest handshakes.
type fakeWG struct {
	t     *testing.T
	peers map[string]int64
	calls []string
}

func (f *fakeWG) run(name string, args ...string) (string, error) {
	call := strings.Join(append([]string{name}, args...), " ")
	f.calls = append(f.calls, call)
	switch {
	case call == "wg show wg-underlay latest-handshakes":
		var out strings.Builder
		for k, ts := range f.peers {
			fmt.Fprintf(&out, "%s\t%d\n", k, ts)
		}
		return out.String(), nil
	case len(args) == 5 && args[0] == "set" && args[4] == "remove":
		delete(f.peers, args[3])
		return "", nil
	case len(args) == 10 && args[0] == "set" && args[4] == "endpoint":
		if _, ok := f.peers[args[3]]; !ok {
			f.peers[args[3]] = 0
		}
		return "", nil
	}
	f.t.Fatalf("unexpected command %q", call)
	return "", nil
}

func (f *fakeWG) took(call string) bool { return slices.Contains(f.calls, call) }

type meshEnv struct {
	m   *mesh
	wg  *fakeWG
	now time.Time
}

func newMeshEnv(t *testing.T) *meshEnv {
	e := &meshEnv{wg: &fakeWG{t: t, peers: map[string]int64{gwKey: 1}}, now: time.Unix(1_800_000_000, 0)}
	e.m = &mesh{cmd: e.wg.run, now: func() time.Time { return e.now }, gateway: gwKey}
	return e
}

func (e *meshEnv) advance(d time.Duration) {
	e.now = e.now.Add(d)
	e.m.tick()
}

func (e *meshEnv) handshake(key string) { e.wg.peers[key] = e.now.Unix() }

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

func trySet(p proto.Peer, cand string) string {
	return "wg set wg-underlay peer " + p.PublicKey + " endpoint " + cand + " allowed-ips " + p.Address + "/32 persistent-keepalive 25"
}

func removeCall(key string) string { return "wg set wg-underlay peer " + key + " remove" }

func TestMeshGoesDirectOnHandshake(t *testing.T) {
	e := newMeshEnv(t)
	e.m.update(proto.Peers{Peers: []proto.Peer{peerA}})
	if !e.wg.took(trySet(peerA, "192.168.1.5:51822")) {
		t.Fatalf("first candidate not tried: %v", e.wg.calls)
	}
	if s := e.path(t, "a"); s.Mode != "relay" {
		t.Fatalf("mode while trying = %q, want relay", s.Mode)
	}
	e.now = e.now.Add(3 * time.Second)
	e.handshake(peerA.PublicKey)
	e.advance(2 * time.Second)
	s := e.path(t, "a")
	if s.Mode != "direct" || s.Endpoint != "192.168.1.5:51822" || s.Handshake == nil {
		t.Fatalf("path = %+v, want direct via first candidate", s)
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
	if !e.wg.took(removeCall(peerA.PublicKey)) {
		t.Fatalf("peer not removed after all candidates failed: %v", e.wg.calls)
	}
	if _, ok := e.wg.peers[peerA.PublicKey]; ok {
		t.Fatal("peer still configured, its /32 would black-hole relayed traffic")
	}
	if s := e.path(t, "a"); s.Mode != "relay" {
		t.Fatalf("mode = %q, want relay", s.Mode)
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
	if _, ok := e.wg.peers[peerA.PublicKey]; ok {
		t.Fatal("stale peer still configured")
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
	e.wg.peers = map[string]int64{gwKey: e.now.Unix()}
	e.wg.calls = nil
	e.advance(5 * time.Second)
	if !e.wg.took(trySet(peerA, "203.0.113.5:51822")) {
		t.Fatalf("direct peer not re-applied after restart: %v", e.wg.calls)
	}
}

func TestMeshRemovesUnknownPeers(t *testing.T) {
	e := newMeshEnv(t)
	stale := testKey(9)
	e.wg.peers[stale] = 1
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
	if !e.wg.took(trySet(proto.Peer{PublicKey: testKey(6), Address: "10.198.0.12"}, "198.51.100.7:51822")) {
		t.Fatalf("valid candidate of a peer with bad ones not tried: %v", e.wg.calls)
	}
	if _, ok := e.wg.peers[gwKey]; !ok {
		t.Fatal("gateway peer removed")
	}
	if s := e.path(t, "nocands"); s.Mode != "relay" {
		t.Fatalf("peer without candidates: mode %q, want relay", s.Mode)
	}
}

func TestAgentAppliesPeers(t *testing.T) {
	e := newMeshEnv(t)
	a := &Agent{mesh: e.m}
	a.handle(t.Context(), proto.Message{Type: proto.TypePeers, Peers: &proto.Peers{Peers: []proto.Peer{peerA}}}, nil)
	if !e.wg.took(trySet(peerA, "192.168.1.5:51822")) {
		t.Fatalf("peers message not applied: %v", e.wg.calls)
	}
}

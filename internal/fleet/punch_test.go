package fleet_test

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/asterism-dev/observatory/internal/fleet"
	"github.com/asterism-dev/observatory/internal/proto"
	"github.com/asterism-dev/observatory/internal/store"
)

func fakeClock(t *testing.T) (time.Time, *atomic.Int64) {
	base := time.Now().Truncate(time.Second)
	offset := &atomic.Int64{}
	prev := *fleet.Clock
	*fleet.Clock = func() time.Time { return base.Add(time.Duration(offset.Load())) }
	t.Cleanup(func() { *fleet.Clock = prev })
	return base, offset
}

// observe sets the gateway-observed endpoints, replacing the previous status.
func (e *env) observe(endpoints map[uuid.UUID]string, quarantined ...string) {
	e.t.Helper()
	chassis, err := e.s.Chassis(context.Background())
	if err != nil {
		e.t.Fatal(err)
	}
	st := proto.GatewayStatus{Endpoints: map[string]string{}, Quarantined: quarantined}
	for _, c := range chassis {
		if ep, ok := endpoints[c.NodeID]; ok {
			st.Endpoints[c.PublicKey] = ep
		}
	}
	if err := e.h.SetStatus(context.Background(), st); err != nil {
		e.t.Fatal(err)
	}
}

func (n *fakeNode) reportPath(peer store.Node, mode string) {
	n.t.Helper()
	n.send(proto.Message{Type: proto.TypePaths, Paths: []proto.PathStatus{{NodeID: peer.ID.String(), Mode: mode}}})
}

func (n *fakeNode) nextPunch() proto.Punch {
	n.t.Helper()
	select {
	case p := <-n.punch:
		return p
	case <-time.After(5 * time.Second):
		n.t.Fatal("no punch message")
	}
	return proto.Punch{}
}

func (n *fakeNode) noPunch() {
	n.t.Helper()
	select {
	case p := <-n.punch:
		n.t.Fatalf("unexpected punch: %+v", p)
	case <-time.After(500 * time.Millisecond):
	}
}

const epA, epB = "203.0.113.5:40001", "198.51.100.7:40002"

// relayPair connects two nodes of e's org with observed endpoints, both past their first peers push.
func relayPair(e *env) (na, nb store.Node, a, b *fakeNode) {
	na, credA := e.node(e.org.ID, "a")
	nb, credB := e.node(e.org.ID, "b")
	a, b = e.connect(credA), e.connect(credB)
	a.nextPeers()
	b.nextPeers()
	e.observe(map[uuid.UUID]string{na.ID: epA, nb.ID: epB})
	return na, nb, a, b
}

func TestPunchSentToBothSides(t *testing.T) {
	base, _ := fakeClock(t)
	e := newEnv(t)
	na, nb, a, b := relayPair(e)
	a.reportPath(nb, "relay")
	a.noPunch()
	b.reportPath(na, "relay")
	pa, pb := a.nextPunch(), b.nextPunch()
	if pa.NodeID != nb.ID.String() || pa.Endpoint != epB || pb.NodeID != na.ID.String() || pb.Endpoint != epA {
		t.Fatalf("punches a=%+v b=%+v", pa, pb)
	}
	if !pa.At.Equal(pb.At) || !pa.At.Equal(base.Add(2*time.Second)) {
		t.Fatalf("start times a=%v b=%v, want %v", pa.At, pb.At, base.Add(2*time.Second))
	}
}

func TestPunchOnlyWithinPeerSet(t *testing.T) {
	ctx := context.Background()
	_, offset := fakeClock(t)
	e := newEnv(t)
	orgB, err := e.s.CreateOrg(ctx, "B", e.owner)
	if err != nil {
		t.Fatal(err)
	}
	na, nb, a, b := relayPair(e)
	nx, credX := e.node(orgB.ID, "x")
	x := e.connect(credX)
	e.observe(map[uuid.UUID]string{na.ID: epA, nb.ID: epB, nx.ID: "192.0.2.9:40003"})
	a.reportPath(nx, "relay")
	x.reportPath(na, "relay")
	a.noPunch()
	x.noPunch()

	a.reportPath(nb, "relay")
	b.reportPath(na, "relay")
	a.nextPunch()
	b.nextPunch()
	e.observe(map[uuid.UUID]string{na.ID: epA, nb.ID: epB}, nb.ID.String())
	offset.Add(int64(6 * time.Minute))
	// a's stored report still lists b as relay, so only b's quarantine keeps the pair from being punched.
	b.reportPath(na, "relay")
	a.noPunch()
	b.noPunch()
}

func TestPunchRateLimited(t *testing.T) {
	_, offset := fakeClock(t)
	e := newEnv(t)
	na, nb, a, b := relayPair(e)
	a.reportPath(nb, "relay")
	b.reportPath(na, "relay")
	a.nextPunch()
	b.nextPunch()
	for range 5 {
		offset.Add(int64(30 * time.Second))
		a.reportPath(nb, "relay")
		b.reportPath(na, "relay")
	}
	a.noPunch()
	b.noPunch()
	offset.Add(int64(3 * time.Minute))
	a.reportPath(nb, "relay")
	a.nextPunch()
	b.nextPunch()
}

func TestPunchBackoffAfterFailures(t *testing.T) {
	_, offset := fakeClock(t)
	e := newEnv(t)
	na, nb, a, b := relayPair(e)
	b.reportPath(na, "relay")
	a.reportPath(nb, "relay")
	a.nextPunch()
	b.nextPunch()
	for range 2 {
		offset.Add(int64(5*time.Minute + time.Second))
		a.reportPath(nb, "relay")
		a.nextPunch()
		b.nextPunch()
	}
	offset.Add(int64(5*time.Minute + time.Second))
	a.reportPath(nb, "relay")
	a.noPunch()
	offset.Add(int64(50 * time.Minute))
	a.reportPath(nb, "relay")
	a.noPunch()
	offset.Add(int64(10*time.Minute + time.Second))
	a.reportPath(nb, "relay")
	a.nextPunch()
	b.nextPunch()
}

func TestPunchSkipsDirectPairs(t *testing.T) {
	e := newEnv(t)
	na, nb, a, b := relayPair(e)
	a.reportPath(nb, "direct")
	b.reportPath(na, "relay")
	a.noPunch()
	b.noPunch()
}

func TestPunchNeedsObservedEndpoints(t *testing.T) {
	e := newEnv(t)
	na, nb, a, b := relayPair(e)
	e.observe(map[uuid.UUID]string{na.ID: epA, nb.ID: "10.198.0.9:1"})
	a.reportPath(nb, "relay")
	b.reportPath(na, "relay")
	a.noPunch()
	b.noPunch()
	e.observe(map[uuid.UUID]string{na.ID: epA})
	b.reportPath(na, "relay")
	a.noPunch()
	b.noPunch()
}

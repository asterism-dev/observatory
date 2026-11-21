package network_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/asterism-dev/observatory/internal/authz"
	"github.com/asterism-dev/observatory/internal/netaddr"
	"github.com/asterism-dev/observatory/internal/network"
	"github.com/asterism-dev/observatory/internal/proto"
	"github.com/asterism-dev/observatory/internal/store"
	"github.com/asterism-dev/observatory/internal/wgkey"
)

type fixture struct {
	t                      testing.TB
	s                      *store.Store
	z                      *authz.Service
	h                      *network.Hub
	orgA, orgB             uuid.UUID
	owner, a1, a2, adm, b1 uuid.UUID
	nodeA, nodeB           uuid.UUID
}

func newFixture(t testing.TB) *fixture {
	t.Helper()
	ctx := context.Background()
	f := &fixture{t: t, s: newStore(t)}
	f.owner, f.a1, f.a2, f.adm, f.b1 = f.user("owner"), f.user("a1"), f.user("a2"), f.user("adm"), f.user("b1")
	f.orgA, f.orgB = f.org(f.owner), f.org(f.b1)
	f.member(f.orgA, f.a1, store.RoleMember)
	f.member(f.orgA, f.a2, store.RoleMember)
	f.member(f.orgA, f.adm, store.RoleAdmin)
	f.nodeA, f.nodeB = f.node(f.orgA, f.owner, "n"), f.node(f.orgB, f.b1, "n")
	z, err := authz.NewService(ctx, f.s.PolicySnapshot)
	if err != nil {
		t.Fatal(err)
	}
	f.z = z
	if f.h, err = network.Load(ctx, f.s, z, cfg); err != nil {
		t.Fatal(err)
	}
	return f
}

func (f *fixture) user(name string) uuid.UUID {
	f.t.Helper()
	u, err := f.s.Login(context.Background(), store.Identity{Subject: name, Email: name + "@x.io", Name: name})
	if err != nil {
		f.t.Fatal(err)
	}
	return u.User.ID
}

func (f *fixture) org(owner uuid.UUID) uuid.UUID {
	f.t.Helper()
	o, err := f.s.CreateOrg(context.Background(), uuid.NewString(), owner)
	if err != nil {
		f.t.Fatal(err)
	}
	return o.ID
}

func (f *fixture) member(org, user uuid.UUID, role store.Role) {
	f.t.Helper()
	ctx := context.Background()
	tok, _, err := f.s.CreateInvite(ctx, org, role, f.owner, time.Hour)
	if err != nil {
		f.t.Fatal(err)
	}
	if _, _, err := f.s.AcceptInvite(ctx, tok, user); err != nil {
		f.t.Fatal(err)
	}
}

func (f *fixture) node(org, by uuid.UUID, name string) uuid.UUID {
	f.t.Helper()
	ctx := context.Background()
	n, tok, err := f.s.CreateNode(ctx, store.Node{OrgID: org, Name: name, CreatedBy: by})
	if err != nil {
		f.t.Fatal(err)
	}
	if _, _, err := f.s.EnrollNode(ctx, tok); err != nil {
		f.t.Fatal(err)
	}
	pub := f.key()
	if _, err := f.s.EnrollChassis(ctx, n.ID, pub); err != nil {
		f.t.Fatal(err)
	}
	return n.ID
}

func (f *fixture) device(org, user uuid.UUID, name string) store.Device {
	f.t.Helper()
	pub := f.key()
	d, err := f.s.CreateDevice(context.Background(), store.Device{OrgID: org, UserID: user, Name: name, PublicKey: pub})
	if err != nil {
		f.t.Fatal(err)
	}
	return d
}

func (f *fixture) key() string {
	f.t.Helper()
	_, pub, err := wgkey.Generate()
	if err != nil {
		f.t.Fatal(err)
	}
	return pub
}

func (f *fixture) team(members ...uuid.UUID) uuid.UUID {
	f.t.Helper()
	ctx := context.Background()
	team, err := f.s.CreateTeam(ctx, f.orgA, "core")
	if err != nil {
		f.t.Fatal(err)
	}
	for _, u := range members {
		if err := f.s.SetTeamMember(ctx, f.orgA, team.ID, u, false); err != nil {
			f.t.Fatal(err)
		}
	}
	return team.ID
}

func (f *fixture) network(name string) store.Network {
	f.t.Helper()
	n, err := f.s.CreateNetwork(context.Background(), store.Network{OrgID: f.orgA, Name: name, CreatedBy: f.owner})
	if err != nil {
		f.t.Fatal(err)
	}
	return n
}

func (f *fixture) attach(vm store.VM, n store.Network) {
	f.t.Helper()
	if _, err := f.s.SetVMNetworks(context.Background(), f.orgA, vm.ID, []uuid.UUID{n.ID}, true); err != nil {
		f.t.Fatal(err)
	}
}

func (f *fixture) bindRole(rule authz.Rule, subjects ...string) {
	f.t.Helper()
	ctx := context.Background()
	r, err := f.s.CreateRole(ctx, f.orgA, authz.CustomRole{Name: uuid.NewString(), Rules: []authz.Rule{rule}})
	if err != nil {
		f.t.Fatal(err)
	}
	for _, sub := range subjects {
		if _, err := f.s.Bind(ctx, f.orgA, sub, r.ID); err != nil {
			f.t.Fatal(err)
		}
	}
}

func (f *fixture) vm(org, node uuid.UUID, name string, user, team *uuid.UUID) store.VM {
	f.t.Helper()
	v, err := f.s.CreateVM(context.Background(), store.VM{OrgID: org, NodeID: node, UserID: user, TeamID: team, Name: name, CPUs: 1, MemoryMB: 1, DiskGB: 1, CreatedBy: f.owner})
	if err != nil {
		f.t.Fatal(err)
	}
	return v
}

func (f *fixture) state() proto.GatewayState {
	f.t.Helper()
	ctx := context.Background()
	if err := f.z.Reload(ctx); err != nil {
		f.t.Fatal(err)
	}
	st, err := f.h.State(ctx)
	if err != nil {
		f.t.Fatal(err)
	}
	return st
}

func orgOf(t *testing.T, st proto.GatewayState, id uuid.UUID) proto.GatewayOrg {
	t.Helper()
	for _, o := range st.Orgs {
		if o.ID == id.String() {
			return o
		}
	}
	t.Fatalf("org %s not in state", id)
	return proto.GatewayOrg{}
}

func mgmtPort(t *testing.T, st proto.GatewayState, vm store.VM) proto.GatewayPort {
	t.Helper()
	for _, p := range orgOf(t, st, vm.OrgID).Ports {
		if p.Name == vm.ID.String()+"-mgmt" {
			return p
		}
	}
	t.Fatalf("no mgmt port for %s", vm.Name)
	return proto.GatewayPort{}
}

func allowed(t *testing.T, st proto.GatewayState, vm store.VM, d store.Device) bool {
	t.Helper()
	return slices.Contains(mgmtPort(t, st, vm).AllowFrom, d.IP.String())
}

func TestAllowListOtherPerson(t *testing.T) {
	f := newFixture(t)
	d1 := f.device(f.orgA, f.a1, "laptop")
	v1 := f.vm(f.orgA, f.nodeA, "v1", &f.a1, nil)
	v2 := f.vm(f.orgA, f.nodeA, "v2", &f.a2, nil)
	st := f.state()
	if !allowed(t, st, v1, d1) {
		t.Error("owner's device missing from own VM")
	}
	if allowed(t, st, v2, d1) {
		t.Error("a1's device admitted to a2's VM")
	}
	p := mgmtPort(t, st, v1)
	if p.IP != v1.MgmtIP.String() || p.MAC != netaddr.MAC(v1.MgmtIP) {
		t.Errorf("port %+v", p)
	}
}

func TestUserVMAdmitsOnlyOwner(t *testing.T) {
	f := newFixture(t)
	d2 := f.device(f.orgA, f.a2, "laptop")
	v1 := f.vm(f.orgA, f.nodeA, "v1", &f.a1, nil)
	f.bindRole(authz.Rule{Effect: authz.Allow, Object: authz.VMObject(v1.ID), Action: "vm:connect"}, authz.UserSubject(f.a2))
	st := f.state()
	if !f.z.Allowed(authz.UserSubject(f.a2), f.orgA, authz.VMObject(v1.ID), "vm:connect") {
		t.Fatal("custom grant not effective")
	}
	if allowed(t, st, v1, d2) {
		t.Error("custom grant admitted a2's device to a1's VM")
	}
}

func TestTeamVMAllowList(t *testing.T) {
	f := newFixture(t)
	team := f.team(f.a1)
	d1, d2 := f.device(f.orgA, f.a1, "laptop"), f.device(f.orgA, f.a2, "laptop")
	v := f.vm(f.orgA, f.nodeA, "tv", nil, &team)
	st := f.state()
	if !allowed(t, st, v, d1) || allowed(t, st, v, d2) {
		t.Fatalf("team VM allow list %v", mgmtPort(t, st, v).AllowFrom)
	}
	if err := f.s.RemoveTeamMember(context.Background(), f.orgA, team, f.a1); err != nil {
		t.Fatal(err)
	}
	if st := f.state(); allowed(t, st, v, d1) {
		t.Error("removed member still admitted")
	}
}

func TestAdminDeviceNotAllowed(t *testing.T) {
	f := newFixture(t)
	da, do := f.device(f.orgA, f.adm, "laptop"), f.device(f.orgA, f.owner, "laptop")
	vms := []store.VM{f.vm(f.orgA, f.nodeA, "v1", &f.a1, nil), f.vm(f.orgA, f.nodeA, "v2", &f.a2, nil)}
	st := f.state()
	for _, v := range vms {
		if allowed(t, st, v, da) || allowed(t, st, v, do) {
			t.Errorf("%s admits admin/owner: %v", v.Name, mgmtPort(t, st, v).AllowFrom)
		}
	}
}

func TestDenyConnectEmptiesList(t *testing.T) {
	f := newFixture(t)
	team := f.team(f.a1)
	d1 := f.device(f.orgA, f.a1, "laptop")
	f.device(f.orgA, f.a2, "laptop")
	v1 := f.vm(f.orgA, f.nodeA, "v1", &f.a1, nil)
	f.vm(f.orgA, f.nodeA, "v2", &f.a2, nil)
	f.vm(f.orgA, f.nodeA, "tv", nil, &team)
	if !allowed(t, f.state(), v1, d1) {
		t.Fatal("a1's device not admitted to v1 before the deny")
	}
	f.bindRole(authz.Rule{Effect: authz.Deny, Object: authz.ObjVMs + "/*", Action: "vm:connect"},
		authz.UserSubject(f.a1), authz.UserSubject(f.a2), authz.TeamSubject(team))
	for _, p := range orgOf(t, f.state(), f.orgA).Ports {
		if len(p.AllowFrom) != 0 {
			t.Errorf("%s allows %v", p.Name, p.AllowFrom)
		}
	}
}

func TestOrgIsolation(t *testing.T) {
	f := newFixture(t)
	f.member(f.orgB, f.a1, store.RoleMember)
	va := f.vm(f.orgA, f.nodeA, "dev", &f.a1, nil)
	vb := f.vm(f.orgB, f.nodeB, "dev", &f.b1, nil)
	f.vm(f.orgB, f.nodeB, "bonly", &f.b1, nil)
	f.device(f.orgA, f.a1, "laptop")
	bDevices := []store.Device{f.device(f.orgB, f.b1, "laptop"), f.device(f.orgB, f.a1, "laptop")}
	st := f.state()
	a, b := orgOf(t, st, f.orgA), orgOf(t, st, f.orgB)
	if a.Records["dev.internal"] != va.MgmtIP.String() || b.Records["dev.internal"] != vb.MgmtIP.String() {
		t.Fatalf("records A %v B %v", a.Records, b.Records)
	}
	if _, ok := a.Records["bonly.internal"]; ok || len(a.Records) != 1 {
		t.Errorf("A records %v", a.Records)
	}
	for _, p := range a.Ports {
		for _, d := range bDevices {
			if slices.Contains(p.AllowFrom, d.IP.String()) {
				t.Errorf("B device %s in A port %s", d.IP, p.Name)
			}
		}
	}
	for _, d := range st.Devices {
		if d.OrgID == "" {
			t.Errorf("device %s without org", d.IP)
		}
	}
}

func TestPortsRequestChassis(t *testing.T) {
	f := newFixture(t)
	n := f.network("build")
	nodeA2 := f.node(f.orgA, f.owner, "n2")
	vms := map[string]string{}
	for _, v := range []store.VM{f.vm(f.orgA, f.nodeA, "v1", &f.a1, nil), f.vm(f.orgA, nodeA2, "v2", &f.a1, nil)} {
		f.attach(v, n)
		vms[v.ID.String()] = v.NodeID.String()
	}
	a := orgOf(t, f.state(), f.orgA)
	ports := append([]proto.GatewayPort{}, a.Ports...)
	for _, nw := range a.Networks {
		ports = append(ports, nw.Ports...)
	}
	if len(ports) != 4 {
		t.Fatalf("ports %+v", ports)
	}
	onA2 := 0
	for _, p := range ports {
		if p.Chassis == "" || p.Chassis != vms[p.Name[:36]] {
			t.Errorf("port %s chassis %q", p.Name, p.Chassis)
		}
		if p.Chassis == nodeA2.String() {
			onA2++
		}
	}
	if onA2 != 2 {
		t.Errorf("%d ports request the second node, want 2", onA2)
	}
}

func TestNetworkPortsAndRecords(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	n := f.network("build")
	f.device(f.orgA, f.a1, "laptop")
	v := f.vm(f.orgA, f.nodeA, "dev", &f.a1, nil)
	f.attach(v, n)
	nics, err := f.s.VMNetworks(ctx, v.ID)
	if err != nil || len(nics) != 1 {
		t.Fatalf("nics %v %v", nics, err)
	}
	a := orgOf(t, f.state(), f.orgA)
	if a.Records["dev.build.internal"] != nics[0].IP.String() {
		t.Fatalf("records %v", a.Records)
	}
	if len(a.Networks) != 1 || a.Networks[0].ID != n.ID.String() || a.Networks[0].Subnet != n.Subnet.String() || len(a.Networks[0].Ports) != 1 {
		t.Fatalf("networks %+v", a.Networks)
	}
	p := a.Networks[0].Ports[0]
	want := proto.GatewayPort{Name: v.ID.String() + "-" + n.ID.String(), MAC: netaddr.MAC(nics[0].IP), IP: nics[0].IP.String(), Chassis: f.nodeA.String()}
	if !slices.Equal(p.AllowFrom, nil) || p.Name != want.Name || p.MAC != want.MAC || p.IP != want.IP || p.Chassis != want.Chassis {
		t.Errorf("network port %+v, want %+v", p, want)
	}
}

func TestDeletedDeviceLeavesState(t *testing.T) {
	f := newFixture(t)
	d := f.device(f.orgA, f.a1, "laptop")
	v := f.vm(f.orgA, f.nodeA, "v1", &f.a1, nil)
	if !allowed(t, f.state(), v, d) {
		t.Fatal("device not admitted before delete")
	}
	if err := f.s.DeleteDevice(context.Background(), f.orgA, d.ID); err != nil {
		t.Fatal(err)
	}
	st := f.state()
	if allowed(t, st, v, d) || slices.ContainsFunc(st.Devices, func(g proto.GatewayDevice) bool { return g.PublicKey == d.PublicKey }) {
		t.Error("deleted device still in state")
	}
}

func TestStateDeterministic(t *testing.T) {
	f := newFixture(t)
	team := f.team(f.a1, f.a2)
	for _, u := range []uuid.UUID{f.a1, f.a2} {
		f.device(f.orgA, u, "laptop")
		f.device(f.orgA, u, "phone")
	}
	n := f.network("build")
	for _, name := range []string{"c", "a", "b"} {
		f.attach(f.vm(f.orgA, f.nodeA, name, nil, &team), n)
	}
	first, err := json.Marshal(f.state())
	if err != nil {
		t.Fatal(err)
	}
	second, err := json.Marshal(f.state())
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(first, second) {
		t.Fatalf("state differs:\n%s\n%s", first, second)
	}
}

// The build must agree with CanConnect for every device × VM pair.
func TestAllowListMatchesCanConnect(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	team := f.team(f.a2)
	if err := f.s.SetTeamMember(ctx, f.orgA, team, f.adm, true); err != nil {
		t.Fatal(err)
	}
	var devices []store.Device
	for _, u := range []uuid.UUID{f.owner, f.a1, f.a2, f.adm} {
		devices = append(devices, f.device(f.orgA, u, "laptop"))
	}
	vms := []store.VM{f.vm(f.orgA, f.nodeA, "v1", &f.a1, nil), f.vm(f.orgA, f.nodeA, "v2", &f.a2, nil), f.vm(f.orgA, f.nodeA, "tv", nil, &team)}
	f.bindRole(authz.Rule{Effect: authz.Deny, Object: authz.VMObject(vms[1].ID), Action: "vm:connect"}, authz.UserSubject(f.a2))
	st := f.state()
	e := f.z.Enforcer()
	hits := 0
	for _, v := range vms {
		ref := authz.VMRef{OrgID: v.OrgID, ID: v.ID, UserID: v.UserID, TeamID: v.TeamID}
		for _, d := range devices {
			want := e.CanConnect(d.UserID, ref)
			if want {
				hits++
			}
			if got := allowed(t, st, v, d); got != want {
				t.Errorf("%s / device of %s: got %v, CanConnect %v", v.Name, d.UserID, got, want)
			}
		}
	}
	if hits != 3 {
		t.Errorf("want 3 connecting pairs (a1/v1, a2/tv, adm/tv), got %d", hits)
	}
}

func TestSetStatus(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	d := f.device(f.orgA, f.a1, "laptop")
	st := f.state()
	var chA proto.GatewayChassis
	for _, c := range st.Chassis {
		if c.NodeID == f.nodeA.String() {
			chA = c
		}
	}
	if chA.IP == "" || chA.OrgID != f.orgA.String() {
		t.Fatalf("chassis %+v", st.Chassis)
	}
	at := time.Now().UTC().Truncate(time.Second)
	if err := f.h.SetStatus(ctx, proto.GatewayStatus{Handshakes: map[string]time.Time{d.PublicKey: at, chA.PublicKey: at}, Chassis: []string{f.nodeA.String()}}); err != nil {
		t.Fatal(err)
	}
	got, err := f.s.Device(ctx, f.orgA, d.ID)
	if err != nil || got.LastHandshake == nil || !got.LastHandshake.Equal(at) {
		t.Fatalf("device handshake %v %v", got.LastHandshake, err)
	}
	cs, ok := f.h.ChassisStatus(f.nodeA)
	if !ok || !cs.Registered || cs.IP != chA.IP || cs.LastHandshake == nil || !cs.LastHandshake.Equal(at) {
		t.Fatalf("chassis A %+v %v", cs, ok)
	}
	cs, ok = f.h.ChassisStatus(f.nodeB)
	if !ok || cs.Registered || cs.LastHandshake != nil {
		t.Fatalf("chassis B %+v %v", cs, ok)
	}
	if _, ok := f.h.ChassisStatus(uuid.New()); ok {
		t.Error("unknown node has chassis status")
	}
}

func TestQuarantineUntilReenroll(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	quarantined := func() map[string]bool {
		q := map[string]bool{}
		for _, c := range f.state().Chassis {
			q[c.NodeID] = c.Quarantined
		}
		return q
	}
	if err := f.h.SetStatus(ctx, proto.GatewayStatus{Quarantined: []string{f.nodeA.String(), "garbage", uuid.NewString()}}); err != nil {
		t.Fatal(err)
	}
	if q := quarantined(); !q[f.nodeA.String()] || q[f.nodeB.String()] {
		t.Fatalf("after report %v", q)
	}
	if err := f.h.SetStatus(ctx, proto.GatewayStatus{}); err != nil {
		t.Fatal(err)
	}
	if q := quarantined(); !q[f.nodeA.String()] {
		t.Fatal("quarantine lifted without re-enrollment")
	}
	token, err := f.s.RegenerateNodeCredential(ctx, f.orgA, f.nodeA)
	if err != nil {
		t.Fatal(err)
	}
	if q := quarantined(); !q[f.nodeA.String()] {
		t.Fatal("quarantine lifted by regenerating the credential alone")
	}
	if _, _, err := f.s.EnrollNode(ctx, token); err != nil {
		t.Fatal(err)
	}
	if q := quarantined(); q[f.nodeA.String()] {
		t.Fatal("quarantine kept after re-enrollment")
	}
}

// BenchmarkState builds the state for 200 devices and 500 VMs, half of them owned by a 50-member team.
func BenchmarkState(b *testing.B) {
	f := newFixture(b)
	users := make([]uuid.UUID, 100)
	for i := range users {
		users[i] = f.user(fmt.Sprintf("u%d", i))
		f.member(f.orgA, users[i], store.RoleMember)
		f.device(f.orgA, users[i], "laptop")
		f.device(f.orgA, users[i], "phone")
	}
	team := f.team(users[:50]...)
	for i := range 500 {
		if i%2 == 0 {
			f.vm(f.orgA, f.nodeA, fmt.Sprintf("u%d", i), &users[i%len(users)], nil)
		} else {
			f.vm(f.orgA, f.nodeA, fmt.Sprintf("t%d", i), nil, &team)
		}
	}
	ctx := context.Background()
	if err := f.z.Reload(ctx); err != nil {
		b.Fatal(err)
	}
	b.ResetTimer()
	for range b.N {
		if _, err := f.h.State(ctx); err != nil {
			b.Fatal(err)
		}
	}
}

func TestSetStatusRecordsEndpoint(t *testing.T) {
	f := newFixture(t)
	var chA proto.GatewayChassis
	for _, c := range f.state().Chassis {
		if c.NodeID == f.nodeA.String() {
			chA = c
		}
	}
	st := proto.GatewayStatus{Endpoints: map[string]string{chA.PublicKey: "203.0.113.5:40001"}}
	if err := f.h.SetStatus(context.Background(), st); err != nil {
		t.Fatal(err)
	}
	if cs, _ := f.h.ChassisStatus(f.nodeA); cs.Endpoint != "203.0.113.5:40001" {
		t.Fatalf("endpoint %q", cs.Endpoint)
	}
	if cs, _ := f.h.ChassisStatus(f.nodeB); cs.Endpoint != "" {
		t.Fatalf("node B endpoint %q", cs.Endpoint)
	}
}

func TestGatewayStateUsesGuestMTU(t *testing.T) {
	f := newFixture(t)
	if got := f.state().GuestMTU; got != netaddr.DefaultGuestMTU {
		t.Fatalf("default %d", got)
	}
	if err := f.s.SetGuestMTU(context.Background(), 1200, f.owner); err != nil {
		t.Fatal(err)
	}
	if got := f.state().GuestMTU; got != 1200 {
		t.Fatalf("got %d", got)
	}
}

func TestStateGatewayChassisDefault(t *testing.T) {
	f := newFixture(t)
	if got := f.state().GatewayChassis; got != (proto.GatewayChassisRef{Name: "hub-gw", IP: "10.198.0.1"}) {
		t.Fatalf("gateway chassis %+v", got)
	}
}

func TestStateGatewayChassisNode(t *testing.T) {
	f := newFixture(t)
	c := cfg
	c.GatewayNode = f.nodeA
	h, err := network.Load(context.Background(), f.s, f.z, c)
	if err != nil {
		t.Fatal(err)
	}
	st, err := h.State(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	i := slices.IndexFunc(st.Chassis, func(c proto.GatewayChassis) bool { return c.NodeID == f.nodeA.String() })
	if i < 0 {
		t.Fatal("node chassis missing")
	}
	if want := (proto.GatewayChassisRef{Name: f.nodeA.String(), IP: st.Chassis[i].IP}); st.GatewayChassis != want {
		t.Fatalf("gateway chassis %+v, want %+v", st.GatewayChassis, want)
	}
}

func TestStateQuarantinedGatewayNode(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	if err := f.h.SetStatus(ctx, proto.GatewayStatus{Quarantined: []string{f.nodeA.String()}}); err != nil {
		t.Fatal(err)
	}
	c := cfg
	c.GatewayNode = f.nodeA
	h, err := network.Load(ctx, f.s, f.z, c)
	if err != nil {
		t.Fatal(err)
	}
	st, err := h.State(ctx)
	if err != nil {
		t.Fatal(err)
	}
	i := slices.IndexFunc(st.Chassis, func(c proto.GatewayChassis) bool { return c.NodeID == f.nodeA.String() })
	if i < 0 || !st.Chassis[i].Quarantined {
		t.Fatalf("quarantined gateway node not in chassis: %+v", st.Chassis)
	}
	if want := (proto.GatewayChassisRef{Name: f.nodeA.String(), IP: st.Chassis[i].IP}); st.GatewayChassis != want {
		t.Fatalf("gateway chassis %+v, want %+v", st.GatewayChassis, want)
	}
}

func TestStateWarnsOnceWhileGatewayNodeHasNoChassis(t *testing.T) {
	f := newFixture(t)
	var logs bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })
	c := cfg
	c.GatewayNode = uuid.New()
	h, err := network.Load(context.Background(), f.s, f.z, c)
	if err != nil {
		t.Fatal(err)
	}
	for range 3 {
		st, err := h.State(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if st.GatewayChassis.Name != "hub-gw" {
			t.Fatalf("gateway chassis %+v", st.GatewayChassis)
		}
	}
	if n := strings.Count(logs.String(), "level=WARN"); n != 1 || !strings.Contains(logs.String(), c.GatewayNode.String()) {
		t.Fatalf("%d warnings:\n%s", n, logs.String())
	}
}

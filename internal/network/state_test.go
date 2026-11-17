package network_test

import (
	"bytes"
	"context"
	"encoding/json"
	"slices"
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
	t                      *testing.T
	s                      *store.Store
	z                      *authz.Service
	h                      *network.Hub
	orgA, orgB             uuid.UUID
	owner, a1, a2, adm, b1 uuid.UUID
	nodeA, nodeB           uuid.UUID
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	ctx := context.Background()
	f := &fixture{t: t, s: newStore(t)}
	f.owner, f.a1, f.a2, f.adm, f.b1 = f.user("owner"), f.user("a1"), f.user("a2"), f.user("adm"), f.user("b1")
	f.orgA, f.orgB = f.org(f.owner), f.org(f.b1)
	f.member(f.orgA, f.a1, store.RoleMember)
	f.member(f.orgA, f.a2, store.RoleMember)
	f.member(f.orgA, f.adm, store.RoleAdmin)
	f.nodeA, f.nodeB = f.node(f.orgA, f.owner), f.node(f.orgB, f.b1)
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

func (f *fixture) node(org, by uuid.UUID) uuid.UUID {
	f.t.Helper()
	ctx := context.Background()
	n, tok, err := f.s.CreateNode(ctx, store.Node{OrgID: org, Name: "n", CreatedBy: by})
	if err != nil {
		f.t.Fatal(err)
	}
	if _, _, err := f.s.EnrollNode(ctx, tok); err != nil {
		f.t.Fatal(err)
	}
	_, pub, _ := wgkey.Generate()
	if _, err := f.s.EnrollChassis(ctx, n.ID, pub); err != nil {
		f.t.Fatal(err)
	}
	return n.ID
}

func (f *fixture) device(org, user uuid.UUID, name string) store.Device {
	f.t.Helper()
	_, pub, _ := wgkey.Generate()
	d, err := f.s.CreateDevice(context.Background(), store.Device{OrgID: org, UserID: user, Name: name, PublicKey: pub})
	if err != nil {
		f.t.Fatal(err)
	}
	return d
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
	ctx := context.Background()
	d2 := f.device(f.orgA, f.a2, "laptop")
	v1 := f.vm(f.orgA, f.nodeA, "v1", &f.a1, nil)
	r, err := f.s.CreateRole(ctx, f.orgA, authz.CustomRole{Name: "connect", Rules: []authz.Rule{{Effect: authz.Allow, Object: authz.VMObject(v1.ID), Action: "vm:connect"}}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.s.Bind(ctx, f.orgA, authz.UserSubject(f.a2), r.ID); err != nil {
		t.Fatal(err)
	}
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
	ctx := context.Background()
	team, err := f.s.CreateTeam(ctx, f.orgA, "core")
	if err != nil {
		t.Fatal(err)
	}
	if err := f.s.SetTeamMember(ctx, f.orgA, team.ID, f.a1, false); err != nil {
		t.Fatal(err)
	}
	d1, d2 := f.device(f.orgA, f.a1, "laptop"), f.device(f.orgA, f.a2, "laptop")
	v := f.vm(f.orgA, f.nodeA, "tv", nil, &team.ID)
	st := f.state()
	if !allowed(t, st, v, d1) || allowed(t, st, v, d2) {
		t.Fatalf("team VM allow list %v", mgmtPort(t, st, v).AllowFrom)
	}
	if err := f.s.RemoveTeamMember(ctx, f.orgA, team.ID, f.a1); err != nil {
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
	ctx := context.Background()
	team, _ := f.s.CreateTeam(ctx, f.orgA, "core")
	if err := f.s.SetTeamMember(ctx, f.orgA, team.ID, f.a1, false); err != nil {
		t.Fatal(err)
	}
	f.device(f.orgA, f.a1, "laptop")
	f.device(f.orgA, f.a2, "laptop")
	f.vm(f.orgA, f.nodeA, "v1", &f.a1, nil)
	f.vm(f.orgA, f.nodeA, "v2", &f.a2, nil)
	f.vm(f.orgA, f.nodeA, "tv", nil, &team.ID)
	r, err := f.s.CreateRole(ctx, f.orgA, authz.CustomRole{Name: "deny", Rules: []authz.Rule{{Effect: authz.Deny, Object: authz.ObjVMs + "/*", Action: "vm:connect"}}})
	if err != nil {
		t.Fatal(err)
	}
	for _, sub := range []string{authz.UserSubject(f.a1), authz.UserSubject(f.a2), authz.TeamSubject(team.ID)} {
		if _, err := f.s.Bind(ctx, f.orgA, sub, r.ID); err != nil {
			t.Fatal(err)
		}
	}
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
	ctx := context.Background()
	n, err := f.s.CreateNetwork(ctx, store.Network{OrgID: f.orgA, Name: "build", CreatedBy: f.owner})
	if err != nil {
		t.Fatal(err)
	}
	vms := map[string]string{}
	for _, name := range []string{"v1", "v2"} {
		v := f.vm(f.orgA, f.nodeA, name, &f.a1, nil)
		if _, err := f.s.SetVMNetworks(ctx, f.orgA, v.ID, []uuid.UUID{n.ID}, true); err != nil {
			t.Fatal(err)
		}
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
	for _, p := range ports {
		if p.Chassis == "" || p.Chassis != vms[p.Name[:36]] {
			t.Errorf("port %s chassis %q", p.Name, p.Chassis)
		}
	}
}

func TestNetworkPortsAndRecords(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	n, err := f.s.CreateNetwork(ctx, store.Network{OrgID: f.orgA, Name: "build", CreatedBy: f.owner})
	if err != nil {
		t.Fatal(err)
	}
	f.device(f.orgA, f.a1, "laptop")
	v := f.vm(f.orgA, f.nodeA, "dev", &f.a1, nil)
	if _, err := f.s.SetVMNetworks(ctx, f.orgA, v.ID, []uuid.UUID{n.ID}, true); err != nil {
		t.Fatal(err)
	}
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
	ctx := context.Background()
	team, _ := f.s.CreateTeam(ctx, f.orgA, "core")
	_ = f.s.SetTeamMember(ctx, f.orgA, team.ID, f.a1, false)
	_ = f.s.SetTeamMember(ctx, f.orgA, team.ID, f.a2, false)
	for _, u := range []uuid.UUID{f.a1, f.a2} {
		f.device(f.orgA, u, "laptop")
		f.device(f.orgA, u, "phone")
	}
	n, _ := f.s.CreateNetwork(ctx, store.Network{OrgID: f.orgA, Name: "build", CreatedBy: f.owner})
	for _, name := range []string{"c", "a", "b"} {
		v := f.vm(f.orgA, f.nodeA, name, nil, &team.ID)
		_, _ = f.s.SetVMNetworks(ctx, f.orgA, v.ID, []uuid.UUID{n.ID}, true)
	}
	first, _ := json.Marshal(f.state())
	second, _ := json.Marshal(f.state())
	if !bytes.Equal(first, second) {
		t.Fatalf("state differs:\n%s\n%s", first, second)
	}
}

// The build must agree with CanConnect for every device × VM pair.
func TestAllowListMatchesCanConnect(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	team, _ := f.s.CreateTeam(ctx, f.orgA, "core")
	_ = f.s.SetTeamMember(ctx, f.orgA, team.ID, f.a2, false)
	_ = f.s.SetTeamMember(ctx, f.orgA, team.ID, f.adm, true)
	var devices []store.Device
	for _, u := range []uuid.UUID{f.owner, f.a1, f.a2, f.adm} {
		devices = append(devices, f.device(f.orgA, u, "laptop"))
	}
	vms := []store.VM{f.vm(f.orgA, f.nodeA, "v1", &f.a1, nil), f.vm(f.orgA, f.nodeA, "v2", &f.a2, nil), f.vm(f.orgA, f.nodeA, "tv", nil, &team.ID)}
	r, _ := f.s.CreateRole(ctx, f.orgA, authz.CustomRole{Name: "deny", Rules: []authz.Rule{{Effect: authz.Deny, Object: authz.VMObject(vms[1].ID), Action: "vm:connect"}}})
	if _, err := f.s.Bind(ctx, f.orgA, authz.UserSubject(f.a2), r.ID); err != nil {
		t.Fatal(err)
	}
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

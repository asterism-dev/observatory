package main

import (
	"slices"
	"strings"
	"testing"

	"github.com/asterism-dev/observatory/internal/proto"
)

type fakeRun struct {
	cmds []string
	out  map[string]string
}

func (f *fakeRun) run(args ...string) (string, error) {
	c := strings.Join(args, " ")
	f.cmds = append(f.cmds, c)
	return f.out[c], nil
}

// transactions are the ovn-nbctl invocations that mutate, other than creating DHCP rows.
func (f *fakeRun) transactions() []string {
	var m []string
	for _, c := range f.cmds {
		if !strings.HasPrefix(c, "ovn-nbctl --bare ") && !strings.HasPrefix(c, "ovn-nbctl --format=") && !strings.HasPrefix(c, "ovn-nbctl dhcp-options-create ") {
			m = append(m, c)
		}
	}
	return m
}

// tx returns the commands of all transactions.
func (f *fakeRun) tx(t *testing.T) []string {
	t.Helper()
	var cmds []string
	for _, tx := range f.transactions() {
		cmds = append(cmds, strings.Split(strings.TrimPrefix(tx, "ovn-nbctl "), " -- ")...)
	}
	if len(cmds) == 0 {
		t.Fatalf("no transaction:\n%s", strings.Join(f.cmds, "\n"))
	}
	return cmds
}

// txWith returns the one transaction containing cmd.
func (f *fakeRun) txWith(t *testing.T, cmd string) []string {
	t.Helper()
	var found [][]string
	for _, tx := range f.transactions() {
		if c := strings.Split(strings.TrimPrefix(tx, "ovn-nbctl "), " -- "); has(c, cmd) {
			found = append(found, c)
		}
	}
	if len(found) != 1 {
		t.Fatalf("%d transactions with %q:\n%s", len(found), cmd, strings.Join(f.cmds, "\n"))
	}
	return found[0]
}

const (
	orgA  = "aaaaaaaa-0000-0000-0000-00000000000a"
	orgB  = "bbbbbbbb-0000-0000-0000-00000000000b"
	orgC  = "cccccccc-0000-0000-0000-00000000000c"
	nodeA = "11111111-0000-0000-0000-000000000001"
	nodeB = "22222222-0000-0000-0000-000000000002"
	netA  = "eeeeeeee-0000-0000-0000-00000000000e"
)

var hubGW = proto.GatewayChassisRef{Name: "hub-gw", IP: "10.198.0.1"}

func dhcpFind(tag string) string {
	return "ovn-nbctl --bare --columns=_uuid find dhcp_options external_ids:" + tag
}

func twoOrgs() (proto.GatewayState, map[string]string) {
	st := proto.GatewayState{GuestMTU: 1300, GatewayChassis: hubGW, Orgs: []proto.GatewayOrg{
		{ID: orgA, Slice: "10.64.0.0/20",
			Ports: []proto.GatewayPort{
				{Name: "vma-mgmt", MAC: "02:0a:40:00:00:0a", IP: "10.64.0.10", Chassis: nodeA, AllowFrom: []string{"10.192.0.10", "10.192.0.11"}},
				{Name: "vma2-mgmt", MAC: "02:0a:40:00:00:0b", IP: "10.64.0.11", Chassis: nodeA}},
			Networks: []proto.GatewayNetwork{{ID: netA, Subnet: "10.210.0.0/24", Ports: []proto.GatewayPort{
				{Name: "vma-net", MAC: "02:0a:d2:00:00:0a", IP: "10.210.0.10", Chassis: nodeA}}}}},
		{ID: orgB, Slice: "10.64.16.0/20",
			Ports: []proto.GatewayPort{
				{Name: "vmb-mgmt", MAC: "02:0a:40:10:00:0a", IP: "10.64.16.10", Chassis: nodeB, AllowFrom: []string{"10.192.0.20"}}}},
	}}
	out := map[string]string{
		dhcpFind("obs-org=" + orgA):     "u-a",
		dhcpFind("obs-org=" + orgB):     "u-b",
		dhcpFind("obs-network=" + netA): "u-n",
	}
	return st, out
}

func has(cmds []string, want string) bool {
	for _, c := range cmds {
		if c == want {
			return true
		}
	}
	return false
}

func TestReconcileNBPerOrg(t *testing.T) {
	st, out := twoOrgs()
	f := &fakeRun{out: out}
	if err := reconcileNB(f.run, st); err != nil {
		t.Fatal(err)
	}
	tx := f.tx(t)
	for _, want := range []string{
		"--may-exist ls-add ls-mgmt-" + orgA,
		"--may-exist ls-add ls-mgmt-" + orgB,
		"--may-exist lrp-add lr-gw lrp-mgmt-" + orgA + " 02:0a:40:00:00:01 10.64.0.1/20",
		"--may-exist lrp-add lr-gw lrp-mgmt-" + orgB + " 02:0a:40:00:10:01 10.64.16.1/20",
		"lsp-set-options mgmt-lr-" + orgA + " router-port=lrp-mgmt-" + orgA,
		"dhcp-options-set-options u-a server_id=10.64.0.1 server_mac=02:0a:40:00:00:01 lease_time=3600 router=10.64.0.1 mtu=1300 classless_static_route={10.192.0.0/16,10.64.0.1}",
		"dhcp-options-set-options u-n server_id=10.210.0.1 server_mac=02:0a:d2:00:00:01 lease_time=3600 mtu=1300",
		"--may-exist lsp-add ls-mgmt-" + orgA + " vma-mgmt",
		"lsp-set-port-security vma-mgmt 02:0a:40:00:00:0a 10.64.0.10",
		"lsp-set-dhcpv4-options vma-mgmt u-a",
		"lsp-set-dhcpv4-options vmb-mgmt u-b",
		"--may-exist lsp-add ls-net-" + netA + " vma-net",
		"lsp-set-dhcpv4-options vma-net u-n",
		"pg-add pg_aaaaaaaa_0000_0000_0000_00000000000a",
		"pg-set-ports pg_aaaaaaaa_0000_0000_0000_00000000000a vma-mgmt vma2-mgmt",
		"pg-set-ports pg_bbbbbbbb_0000_0000_0000_00000000000b vmb-mgmt",
		"acl-del pg_aaaaaaaa_0000_0000_0000_00000000000a",
		"acl-add pg_aaaaaaaa_0000_0000_0000_00000000000a from-lport 1002 inport == @pg_aaaaaaaa_0000_0000_0000_00000000000a && (ip4.dst == {10.192.0.1, 10.64.0.1} || (udp.src == 68 && udp.dst == 67)) allow-related",
		"acl-add pg_aaaaaaaa_0000_0000_0000_00000000000a from-lport 1001 inport == @pg_aaaaaaaa_0000_0000_0000_00000000000a && ip4 drop",
		"acl-add pg_aaaaaaaa_0000_0000_0000_00000000000a to-lport 1002 outport == @pg_aaaaaaaa_0000_0000_0000_00000000000a && ip4.src == {10.192.0.1, 10.64.0.1} allow-related",
		"acl-add pg_aaaaaaaa_0000_0000_0000_00000000000a to-lport 1002 outport == \"vma-mgmt\" && ip4.src == {10.192.0.10, 10.192.0.11} allow-related",
		"acl-add pg_aaaaaaaa_0000_0000_0000_00000000000a to-lport 1001 outport == @pg_aaaaaaaa_0000_0000_0000_00000000000a && ip4 drop",
		"acl-add pg_aaaaaaaa_0000_0000_0000_00000000000a from-lport 1001 inport == @pg_aaaaaaaa_0000_0000_0000_00000000000a && ip6 drop",
		"acl-add pg_aaaaaaaa_0000_0000_0000_00000000000a to-lport 1001 outport == @pg_aaaaaaaa_0000_0000_0000_00000000000a && ip6 drop",
		"acl-add pg_bbbbbbbb_0000_0000_0000_00000000000b from-lport 1001 inport == @pg_bbbbbbbb_0000_0000_0000_00000000000b && ip6 drop",
		"acl-add pg_bbbbbbbb_0000_0000_0000_00000000000b to-lport 1002 outport == \"vmb-mgmt\" && ip4.src == {10.192.0.20} allow-related",
		"--may-exist lr-route-add lr-gw 10.192.0.0/16 10.199.0.2",
		"lrp-set-gateway-chassis lrp-transit hub-gw 1",
	} {
		if !has(tx, want) {
			t.Errorf("transaction missing %q", want)
		}
	}
	chassis := map[string]string{"vma-mgmt": nodeA, "vma2-mgmt": nodeA, "vma-net": nodeA, "vmb-mgmt": nodeB}
	adds := 0
	for _, c := range tx {
		f := strings.Fields(c)
		if len(f) == 4 && f[1] == "lsp-add" && strings.HasPrefix(f[2], "ls-") && f[2] != "ls-transit" && !strings.HasPrefix(f[3], "mgmt-lr-") {
			adds++
			if !has(tx, "lsp-set-options "+f[3]+" requested-chassis="+chassis[f[3]]) {
				t.Errorf("%s has no requested-chassis", f[3])
			}
		}
	}
	if adds != len(chassis) {
		t.Errorf("lsp-adds %d", adds)
	}
	var vmPorts int
	for _, c := range tx {
		if strings.Contains(c, "requested-chassis=") && !strings.HasPrefix(c, "lsp-set-options gw0 ") {
			vmPorts++
		}
	}
	if vmPorts != len(chassis) {
		t.Errorf("requested-chassis options %d", vmPorts)
	}
	for _, c := range tx {
		if strings.HasPrefix(c, "acl-add pg_aaaaaaaa") && strings.Contains(c, "10.192.0.20") {
			t.Errorf("org B device in org A ACL: %s", c)
		}
	}
}

func TestReconcileNBRemovesStaleOrg(t *testing.T) {
	st, out := twoOrgs()
	out["ovn-nbctl --bare --columns=name list logical_switch"] = "ls-mgmt-" + orgA + "\nls-mgmt-" + orgC + "\nls-net-gone\nls-transit"
	out["ovn-nbctl --bare --columns=name list port_group"] = "pg_aaaaaaaa_0000_0000_0000_00000000000a\npg_cccccccc_0000_0000_0000_00000000000c"
	out["ovn-nbctl --bare --columns=name list logical_router_port"] = "lrp-transit\nlrp-mgmt-" + orgA + "\nlrp-mgmt-" + orgC
	out["ovn-nbctl --format=csv --data=bare --no-headings --columns=name,type list logical_switch_port"] = "vma-mgmt,\nold-mgmt,\nmgmt-lr-" + orgA + ",router\ngw0,\ntransit-lr,router"
	out["ovn-nbctl --format=csv --data=bare --no-headings --columns=_uuid,external_ids list dhcp_options"] = "u-a,obs-org=" + orgA + "\nu-c,obs-org=" + orgC + "\nu-g,obs-network=gone\nu-x,\n"
	f := &fakeRun{out: out}
	if err := reconcileNB(f.run, st); err != nil {
		t.Fatal(err)
	}
	tx := f.tx(t)
	for _, want := range []string{
		"ls-del ls-mgmt-" + orgC, "ls-del ls-net-gone",
		"pg-del pg_cccccccc_0000_0000_0000_00000000000c",
		"lrp-del lrp-mgmt-" + orgC,
		"lsp-del old-mgmt",
		"dhcp-options-del u-c", "dhcp-options-del u-g",
	} {
		if !has(tx, want) {
			t.Errorf("transaction missing %q", want)
		}
	}
	for _, c := range tx {
		for _, keep := range []string{"ls-del ls-mgmt-" + orgA, "ls-del ls-transit", "pg-del pg_aaaa", "lrp-del lrp-transit", "lrp-del lrp-mgmt-" + orgA,
			"lsp-del vma-mgmt", "lsp-del gw0", "lsp-del mgmt-lr", "lsp-del transit-lr", "dhcp-options-del u-a", "dhcp-options-del u-x"} {
			if strings.HasPrefix(c, keep) {
				t.Errorf("deleted wanted object: %s", c)
			}
		}
	}
	if has(tx, "pg-add pg_aaaaaaaa_0000_0000_0000_00000000000a") {
		t.Error("existing port group re-added")
	}
}

func TestReconcileNBCreatesMissingDHCPOptions(t *testing.T) {
	st := proto.GatewayState{GuestMTU: 1300, Orgs: []proto.GatewayOrg{{ID: orgA, Slice: "10.64.0.0/20"}}}
	calls := 0
	f := &fakeRun{out: map[string]string{}}
	run := func(args ...string) (string, error) {
		c := strings.Join(args, " ")
		if c == dhcpFind("obs-org="+orgA) {
			calls++
			if calls == 2 {
				f.cmds = append(f.cmds, c)
				return "u-9\n", nil
			}
		}
		return f.run(args...)
	}
	if err := reconcileNB(run, st); err != nil {
		t.Fatal(err)
	}
	if !has(f.cmds, "ovn-nbctl dhcp-options-create 10.64.0.0/20 obs-org="+orgA) {
		t.Errorf("dhcp row not created:\n%s", strings.Join(f.cmds, "\n"))
	}
	tx := f.tx(t)
	if !has(tx, "clear port_group pg_aaaaaaaa_0000_0000_0000_00000000000a ports") || !has(tx, "dhcp-options-set-options u-9 server_id=10.64.0.1 server_mac=02:0a:40:00:00:01 lease_time=3600 router=10.64.0.1 mtu=1300 classless_static_route={10.192.0.0/16,10.64.0.1}") {
		t.Errorf("empty org not reconciled: %v", tx)
	}
}

func TestReconcileNBSkipsBadOrg(t *testing.T) {
	st, out := twoOrgs()
	st.Orgs[0].Slice = "nope"
	f := &fakeRun{out: out}
	if err := reconcileNB(f.run, st); err == nil || !strings.Contains(err.Error(), orgA) {
		t.Fatalf("err %v", err)
	}
	tx := f.tx(t)
	if !has(tx, "--may-exist ls-add ls-mgmt-"+orgB) || !has(tx, "--may-exist lr-add lr-gw") {
		t.Fatalf("good org not applied: %v", tx)
	}
	for _, c := range tx {
		if strings.Contains(c, "ls-mgmt-"+orgA) || strings.Contains(c, "vma-mgmt") || strings.HasPrefix(c, "ls-del") || strings.HasPrefix(c, "lsp-del") {
			t.Errorf("bad org touched: %s", c)
		}
	}
}

func TestReconcileNBOrgTransactions(t *testing.T) {
	st, out := twoOrgs()
	out["ovn-nbctl --format=csv --data=bare --no-headings --columns=name,type list logical_switch_port"] = "old-mgmt,"
	var cmds []string
	run := func(args ...string) (string, error) {
		c := strings.Join(args, " ")
		cmds = append(cmds, c)
		if strings.Contains(c, " ls-add ls-mgmt-"+orgA) {
			return "", errTest
		}
		return out[c], nil
	}
	if err := reconcileNB(run, st); err == nil || !strings.Contains(err.Error(), orgA) {
		t.Fatalf("err %v", err)
	}
	f := &fakeRun{cmds: cmds}
	b := f.txWith(t, "--may-exist ls-add ls-mgmt-"+orgB)
	for _, want := range []string{"--may-exist lsp-add ls-mgmt-" + orgB + " vmb-mgmt", "pg-set-ports pg_bbbbbbbb_0000_0000_0000_00000000000b vmb-mgmt",
		"acl-add pg_bbbbbbbb_0000_0000_0000_00000000000b from-lport 1001 inport == @pg_bbbbbbbb_0000_0000_0000_00000000000b && ip4 drop"} {
		if !has(b, want) {
			t.Errorf("org B transaction missing %q", want)
		}
	}
	for _, c := range b {
		if strings.Contains(c, orgA) || strings.Contains(c, "vma") || strings.Contains(c, " lr-add ") || strings.HasPrefix(c, "lsp-del ") {
			t.Errorf("org B transaction carries %s", c)
		}
	}
	a := f.txWith(t, "--may-exist ls-add ls-mgmt-"+orgA)
	if !has(a, "lsp-set-port-security vma-mgmt 02:0a:40:00:00:0a 10.64.0.10") || !has(a, "acl-del pg_aaaaaaaa_0000_0000_0000_00000000000a") {
		t.Errorf("org A port without its ACLs: %v", a)
	}
	f.txWith(t, "--may-exist lr-add lr-gw")
	f.txWith(t, "lsp-del old-mgmt")
}

func TestReconcileNBRouterAddressOnlyForOwnOrg(t *testing.T) {
	st, out := twoOrgs()
	st.Devices = []proto.GatewayDevice{{IP: "10.192.0.10", OrgID: orgA}, {IP: "10.192.0.11", OrgID: orgA}}
	f := &fakeRun{out: out}
	if err := reconcileNB(f.run, st); err != nil {
		t.Fatal(err)
	}
	hub := f.txWith(t, "--may-exist lr-add lr-gw")
	var acls []string
	for _, c := range hub {
		if strings.Contains(c, "acl-") {
			acls = append(acls, c)
		}
	}
	want := []string{
		"acl-del ls-transit",
		`acl-add ls-transit from-lport 1002 inport == "gw0" && ip4.dst == 10.64.0.1 && ip4.src == {10.192.0.10, 10.192.0.11} allow`,
		`acl-add ls-transit from-lport 1001 inport == "gw0" && ip4.dst == {10.64.0.1, 10.64.16.1} drop`,
	}
	if strings.Join(acls, "\n") != strings.Join(want, "\n") {
		t.Fatalf("transit ACLs:\n%s", strings.Join(acls, "\n"))
	}
}

func TestReconcileNBDefaultHubGW(t *testing.T) {
	st, out := twoOrgs()
	out["ovn-nbctl --bare --columns=name list gateway_chassis"] = "lrp-transit-hub-gw\n"
	f := &fakeRun{out: out}
	if err := reconcileNB(f.run, st); err != nil {
		t.Fatal(err)
	}
	hub := f.txWith(t, "--may-exist lr-add lr-gw")
	if !has(hub, "lsp-set-options gw0 requested-chassis=hub-gw") || !has(hub, "lrp-set-gateway-chassis lrp-transit hub-gw 1") {
		t.Fatalf("hub-gw not requested: %v", hub)
	}
	if !has(f.cmds, "ovn-nbctl --bare --columns=name list gateway_chassis") {
		t.Error("gateway chassis rows not read")
	}
	for _, c := range f.cmds {
		if strings.Contains(c, "lrp-del-gateway-chassis") {
			t.Errorf("steady state deletes: %s", c)
		}
	}
}

func TestReconcileNBRemovesStaleGatewayChassis(t *testing.T) {
	for _, c := range []struct {
		name    string
		gateway proto.GatewayChassisRef
		rows    string
		del     []string
	}{
		{"hub-gw to node", proto.GatewayChassisRef{Name: nodeA, IP: "10.198.0.11"}, "lrp-transit-hub-gw\n", []string{"hub-gw"}},
		{"node to hub-gw", hubGW, "lrp-transit-hub-gw\nlrp-transit-" + nodeA + "\n", []string{nodeA}},
		{"node to node", proto.GatewayChassisRef{Name: nodeB, IP: "10.198.0.21"}, "lrp-transit-" + nodeA + "\n", []string{nodeA}},
		{"node steady", proto.GatewayChassisRef{Name: nodeA, IP: "10.198.0.11"}, "lrp-transit-" + nodeA + "\n", nil},
	} {
		st, out := twoOrgs()
		st.GatewayChassis = c.gateway
		out["ovn-nbctl --bare --columns=name list gateway_chassis"] = c.rows
		f := &fakeRun{out: out}
		if err := reconcileNB(f.run, st); err != nil {
			t.Fatal(err)
		}
		hub := f.txWith(t, "--may-exist lr-add lr-gw")
		if !has(hub, "lsp-set-options gw0 requested-chassis="+c.gateway.Name) || !has(hub, "lrp-set-gateway-chassis lrp-transit "+c.gateway.Name+" 1") {
			t.Errorf("%s: gateway chassis not requested: %v", c.name, hub)
		}
		var del []string
		for _, cmd := range f.tx(t) {
			if d, ok := strings.CutPrefix(cmd, "lrp-del-gateway-chassis lrp-transit "); ok {
				del = append(del, d)
			}
		}
		if !slices.Equal(del, c.del) {
			t.Errorf("%s: deleted %v, want %v", c.name, del, c.del)
		}
	}
}

func quarantinedGatewayNode() (proto.GatewayState, map[string]string) {
	st, out := twoOrgs()
	st.GatewayChassis = proto.GatewayChassisRef{Name: nodeA, IP: "10.198.0.11"}
	st.Chassis = []proto.GatewayChassis{
		{NodeID: nodeA, OrgID: orgA, IP: "10.198.0.11", Quarantined: true},
		{NodeID: nodeB, OrgID: orgB, IP: "10.198.0.21"},
	}
	return st, out
}

func dropACLs(tx []string) []string {
	var acls []string
	for _, c := range tx {
		if strings.HasPrefix(c, "acl-add ") && strings.Contains(c, " 1003 ") {
			acls = append(acls, c)
		}
	}
	return acls
}

func TestQuarantinedGatewayNodeVMPortsDropped(t *testing.T) {
	st, out := quarantinedGatewayNode()
	st.Orgs[1].Ports = append(st.Orgs[1].Ports, proto.GatewayPort{Name: "vmb2-mgmt", MAC: "02:0a:40:10:00:0b", IP: "10.64.16.11", Chassis: nodeA})
	f := &fakeRun{out: out}
	if err := reconcileNB(f.run, st); err != nil {
		t.Fatal(err)
	}
	mgmtA, mgmtB, net := "ls-mgmt-"+orgA, "ls-mgmt-"+orgB, "ls-net-"+netA
	want := []string{
		"acl-add " + mgmtA + ` from-lport 1003 inport == {"vma-mgmt", "vma2-mgmt"} drop`,
		"acl-add " + mgmtA + ` to-lport 1003 outport == {"vma-mgmt", "vma2-mgmt"} drop`,
		"acl-add " + net + ` from-lport 1003 inport == {"vma-net"} drop`,
		"acl-add " + net + ` to-lport 1003 outport == {"vma-net"} drop`,
		"acl-add " + mgmtB + ` from-lport 1003 inport == {"vmb2-mgmt"} drop`,
		"acl-add " + mgmtB + ` to-lport 1003 outport == {"vmb2-mgmt"} drop`,
	}
	if got := dropACLs(f.tx(t)); strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("drop ACLs:\n%s", strings.Join(got, "\n"))
	}
	a := f.txWith(t, "--may-exist ls-add "+mgmtA)
	for _, c := range []string{"acl-del " + mgmtA, "acl-del " + net, want[0], want[2]} {
		if !has(a, c) {
			t.Errorf("org A transaction missing %q", c)
		}
	}

	st.Chassis[0].Quarantined = false
	f = &fakeRun{out: out}
	if err := reconcileNB(f.run, st); err != nil {
		t.Fatal(err)
	}
	tx := f.tx(t)
	if got := dropACLs(tx); len(got) != 0 {
		t.Errorf("drop ACLs after quarantine lifted: %v", got)
	}
	for _, sw := range []string{mgmtA, mgmtB, net} {
		if !has(tx, "acl-del "+sw) {
			t.Errorf("drop ACLs of %s not cleared", sw)
		}
	}
}

func TestQuarantinedGatewayNodeGatewayPortsUntouched(t *testing.T) {
	st, out := quarantinedGatewayNode()
	out["ovn-nbctl --bare --columns=name list gateway_chassis"] = "lrp-transit-" + nodeA + "\n"
	f := &fakeRun{out: out}
	if err := reconcileNB(f.run, st); err != nil {
		t.Fatal(err)
	}
	quarantined := f.txWith(t, "--may-exist lr-add lr-gw")
	st.Chassis[0].Quarantined = false
	f2 := &fakeRun{out: out}
	if err := reconcileNB(f2.run, st); err != nil {
		t.Fatal(err)
	}
	if healthy := f2.txWith(t, "--may-exist lr-add lr-gw"); !slices.Equal(quarantined, healthy) {
		t.Errorf("hub transaction changed by quarantine:\n%s\nvs\n%s", strings.Join(quarantined, "\n"), strings.Join(healthy, "\n"))
	}
	for _, c := range f.tx(t) {
		if strings.Contains(c, " 1003 ") && (strings.Contains(c, "gw0") || strings.Contains(c, "transit")) || strings.Contains(c, "lrp-del-gateway-chassis") {
			t.Errorf("gateway port touched: %s", c)
		}
	}
	run := func(args ...string) (string, error) {
		if strings.Join(args, " ") == "ovn-sbctl --bare --columns=name list chassis" {
			return nodeA + "\n" + nodeB + "\n", nil
		}
		if args[1] == "chassis-del" {
			t.Errorf("deleted chassis %s", args[2])
		}
		return "", nil
	}
	st.Chassis[0].Quarantined = true
	if err := cleanChassis(run, st); err != nil {
		t.Fatal(err)
	}
}

func TestQuarantinedOtherNodeUnchanged(t *testing.T) {
	for _, gateway := range []proto.GatewayChassisRef{hubGW, {Name: nodeA, IP: "10.198.0.11"}} {
		st, out := quarantinedGatewayNode()
		st.GatewayChassis = gateway
		st.Chassis[0].Quarantined, st.Chassis[1].Quarantined = false, true
		f := &fakeRun{out: out}
		if err := reconcileNB(f.run, st); err != nil {
			t.Fatal(err)
		}
		if got := dropACLs(f.tx(t)); len(got) != 0 {
			t.Errorf("%s: drop ACLs for a quarantined non-gateway node: %v", gateway.Name, got)
		}
		if r := underlayRules(st); !strings.Contains(r, "set quarantine {\n\t\ttype ipv4_addr\n\t\telements = { 10.198.0.21 }\n\t}") {
			t.Errorf("%s: node B not in the quarantine set:\n%s", gateway.Name, r)
		}
		var deleted []string
		run := func(args ...string) (string, error) {
			if strings.Join(args, " ") == "ovn-sbctl --bare --columns=name list chassis" {
				return nodeA + "\n" + nodeB + "\n", nil
			}
			deleted = append(deleted, strings.Join(args, " "))
			return "", nil
		}
		if err := cleanChassis(run, st); err != nil {
			t.Fatal(err)
		}
		if !slices.Equal(deleted, []string{"ovn-sbctl chassis-del " + nodeB}) {
			t.Errorf("%s: chassis deletes %v", gateway.Name, deleted)
		}
	}
}

func TestReconcileNBKeepsForeignGatewayChassisRows(t *testing.T) {
	st, out := twoOrgs()
	out["ovn-nbctl --bare --columns=name list gateway_chassis"] = "lrp-transit-garbage\nlrp-transit-" + strings.ToUpper(nodeA) + "\nlrp-transit-{" + nodeB + "}\nlrp-transit-" + nodeA + "\n"
	f := &fakeRun{out: out}
	if err := reconcileNB(f.run, st); err != nil {
		t.Fatal(err)
	}
	var del []string
	for _, cmd := range f.tx(t) {
		if d, ok := strings.CutPrefix(cmd, "lrp-del-gateway-chassis lrp-transit "); ok {
			del = append(del, d)
		}
	}
	if !slices.Equal(del, []string{nodeA}) {
		t.Errorf("deleted %v, want only the canonical %s", del, nodeA)
	}
}

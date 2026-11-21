package main

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/asterism-dev/observatory/internal/proto"
	"github.com/asterism-dev/observatory/internal/wgkey"
)

var goodSB = proto.GatewaySB{CACert: "ca", Cert: "cert", Key: "key"}

func TestValidateState(t *testing.T) {
	good := proto.GatewayState{UnderlayKey: "a", ClientsKey: "b", GuestMTU: 1300, SB: goodSB}
	if err := validateState(good); err != nil {
		t.Fatal(err)
	}
	noCA, noCert, noKey := good, good, good
	noCA.SB.CACert, noCert.SB.Cert, noKey.SB.Key = "", "", ""
	noUnderlay, noClients, noMTU := good, good, good
	noUnderlay.UnderlayKey, noClients.ClientsKey, noMTU.GuestMTU = "", "", 0
	for _, bad := range []proto.GatewayState{{}, noCA, noCert, noKey, noUnderlay, noClients, noMTU} {
		if validateState(bad) == nil {
			t.Errorf("accepted %+v", bad)
		}
	}
}

func TestApplySyncsWGWhenNBFails(t *testing.T) {
	var wg int
	run := func(args ...string) (string, error) {
		if args[0] == "wg" {
			wg++
			return "", nil
		}
		return "", errTest
	}
	err := apply(run, proto.GatewayState{UnderlayKey: "a", ClientsKey: "b", GuestMTU: 1300, SB: goodSB}, t.TempDir())
	if err == nil || !strings.Contains(err.Error(), "boom") {
		t.Fatalf("err %v", err)
	}
	if wg != 2 {
		t.Fatalf("wg syncs %d", wg)
	}
}

func TestApplySkipsInvalidPeers(t *testing.T) {
	_, good := keyPair(t)
	files := map[string]string{}
	run := func(args ...string) (string, error) {
		if (args[0] == "wg" && args[1] == "syncconf") || args[0] == "nft" {
			b, err := os.ReadFile(args[len(args)-1])
			files[args[0]+" "+args[len(args)-2]] += string(b)
			return "", err
		}
		return "", nil
	}
	st := proto.GatewayState{UnderlayKey: "a", ClientsKey: "b", GuestMTU: 1300, SB: goodSB,
		Orgs: []proto.GatewayOrg{{ID: orgA, Slice: "10.64.0.0/20"}},
		Chassis: []proto.GatewayChassis{
			{NodeID: "ok", OrgID: orgA, PublicKey: good, IP: "10.198.0.11"},
			{NodeID: "newline", OrgID: orgA, PublicKey: good[:20] + "\n" + good[20:], IP: "10.198.0.12"},
			{NodeID: "badip", OrgID: orgA, PublicKey: good, IP: "10.198.0.13\n"},
			{NodeID: "v6", OrgID: orgA, PublicKey: good, IP: "fe80::1"},
		},
		Devices: []proto.GatewayDevice{
			{PublicKey: good, IP: "10.192.0.10", OrgID: orgA},
			{PublicKey: good + "\r\n", IP: "10.192.0.11", OrgID: orgA},
			{PublicKey: good, IP: "nope", OrgID: orgA},
		},
	}
	_ = apply(run, st, t.TempDir()) // the fake NB fails; only the WireGuard and nft inputs matter here
	underlay, clients := files["wg wg-underlay"], files["wg wg-clients"]
	if strings.Count(underlay, "[Peer]") != 1 || !strings.Contains(underlay, "AllowedIPs = 10.198.0.11/32") {
		t.Errorf("underlay peers:\n%s", underlay)
	}
	if strings.Count(clients, "[Peer]") != 1 || !strings.Contains(clients, "AllowedIPs = 10.192.0.10/32") {
		t.Errorf("client peers:\n%s", clients)
	}
	nft := files["nft -f"]
	if !strings.Contains(nft, "elements = { 10.198.0.11 }") {
		t.Errorf("nft:\n%s", nft)
	}
}

func keyPair(t *testing.T) (string, string) {
	t.Helper()
	priv, pub, err := wgkey.Generate()
	if err != nil {
		t.Fatal(err)
	}
	return priv, pub
}

type testErr struct{}

func (testErr) Error() string { return "boom" }

var errTest = testErr{}

func TestCallHubAcceptsAny2xx(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer tok" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		if r.URL.Path == "/status" {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		w.Write([]byte("ok"))
	}))
	defer srv.Close()
	if _, err := callHub(srv.Client(), srv.URL, "tok", "POST", "/status", nil); err != nil {
		t.Fatalf("204: %v", err)
	}
	if b, err := callHub(srv.Client(), srv.URL, "tok", "GET", "/state", nil); err != nil || string(b) != "ok" {
		t.Fatalf("200: %q, %v", b, err)
	}
	if _, err := callHub(srv.Client(), srv.URL, "bad", "GET", "/state", nil); err == nil {
		t.Fatal("401 accepted")
	}
}

func TestGuardBindings(t *testing.T) {
	var calls []string
	run := func(args ...string) (string, error) {
		c := strings.Join(args, " ")
		calls = append(calls, c)
		switch {
		case strings.Contains(c, "list chassis"):
			return "ch-a,node-a\nch-b,node-b\nch-gw,hub-gw\n", nil
		case strings.Contains(c, "ovn-nbctl") && strings.Contains(c, "list logical_switch_port"):
			return strings.Join([]string{
				"vm1-mgmt,requested-chassis=node-a", "vm2-mgmt,requested-chassis=node-a", "vm3-mgmt,requested-chassis=node-a",
				"vm6-mgmt,requested-chassis=node-a", `vm7-mgmt,"requested-chassis=node-a,node-b"`, "vm9-mgmt,requested-chassis=node-a",
				`vm10-mgmt,"requested-chassis=node-a,node-b"`, "mgmt-lr,router-port=lrp", "gw0,requested-chassis=hub-gw",
			}, "\n") + "\n", nil
		case strings.Contains(c, "list port_binding"):
			// SB options are chassis-writable, so they are not even read.
			return strings.Join([]string{
				"u1,vm1-mgmt,ch-a,",        // bound where requested
				"u2,vm2-mgmt,ch-b,",        // foreign claim, e.g. via a spoofed hostname
				"u3,vm3-mgmt,,",            // unbound
				"u4,cr-lrp-transit,ch-gw,", // gateway port on hub-gw
				"u5,cr-lrp-transit2,ch-b,", // gateway port claimed by a node
				"u6,vm6-mgmt,ch-a,ch-b",    // additional chassis not requested
				"u7,vm7-mgmt,ch-a,ch-b",    // requested additional chassis
				"u8,mgmt-lr,,",
				"u9,vm9-mgmt,ch-gone,",          // chassis row without a name
				"u10,vm10-mgmt,ch-a,ch-gw ch-b", // one additional chassis not requested
				"u11,gw0,ch-gw,",
				"u12,deleted-mgmt,ch-b,", // VM deleted: gone from NB, still bound in SB
			}, "\n") + "\n", nil
		}
		return "", nil
	}
	got, claims, err := guardBindings(run)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(got, ",") != "vm2-mgmt,cr-lrp-transit2,vm6-mgmt,vm9-mgmt,vm10-mgmt,deleted-mgmt" {
		t.Fatalf("rebound %v", got)
	}
	wantClaims := []claim{{"vm2-mgmt", "node-b"}, {"cr-lrp-transit2", "node-b"}, {"vm6-mgmt", "node-b"}}
	if !slices.Equal(claims, wantClaims) {
		t.Fatalf("claims %v", claims)
	}
	var clears []string
	for _, c := range calls {
		if strings.Contains(c, " clear ") {
			clears = append(clears, c)
		}
		if strings.Contains(c, "requested_chassis") {
			t.Errorf("trusts the SB requested_chassis column: %s", c)
		}
	}
	want := []string{
		"ovn-sbctl clear port_binding u2 chassis",
		"ovn-sbctl clear port_binding u5 chassis",
		"ovn-sbctl clear port_binding u6 additional_chassis",
		"ovn-sbctl clear port_binding u9 chassis",
		"ovn-sbctl clear port_binding u10 additional_chassis",
		"ovn-sbctl clear port_binding u12 chassis",
	}
	if strings.Join(clears, "\n") != strings.Join(want, "\n") {
		t.Fatalf("clears %v", clears)
	}
}

func TestGuardMACBindings(t *testing.T) {
	var calls []string
	run := func(args ...string) (string, error) {
		c := strings.Join(args, " ")
		calls = append(calls, c)
		if strings.Contains(c, "list mac_binding") {
			return strings.Join([]string{
				"m1,lrp-mgmt-a,10.64.0.10,02:0a:40:00:00:0a",
				"m2,lrp-mgmt-a,10.64.0.11,02:0a:40:00:00:0a", // another VM's MAC
				"m3,lrp-transit,10.199.0.2,02:00:00:c7:00:02",
				"m4,lrp-transit,10.199.0.2,02:0a:c7:00:00:02",
				"m5,lrp-mgmt-b,10.64.16.10,02:0A:40:00:10:0A",
				"m6,lrp-transit,10.199.0.3,02:00:00:c7:00:02",
				"m7,vm-net,10.210.0.10,02:00:00:00:00:01", // not on lr-gw
				"m8,lrp-mgmt-a,nope,02:0a:40:00:00:0a",
			}, "\n") + "\n", nil
		}
		return "", nil
	}
	if err := guardMACBindings(run); err != nil {
		t.Fatal(err)
	}
	var del []string
	for _, c := range calls {
		if strings.Contains(c, "destroy") {
			del = append(del, c)
		}
	}
	want := "ovn-sbctl destroy mac_binding m2\novn-sbctl destroy mac_binding m4\novn-sbctl destroy mac_binding m6\novn-sbctl destroy mac_binding m8"
	if strings.Join(del, "\n") != want {
		t.Fatalf("destroyed %v", del)
	}
}

func TestApplySBSkipsUnchangedConfig(t *testing.T) {
	dir := t.TempDir()
	key, cert, ca := filepath.Join(dir, "sb-key.pem"), filepath.Join(dir, "sb-cert.pem"), filepath.Join(dir, "sb-ca.pem")
	configured := map[string]string{
		"ovn-sbctl get-ssl":        "Private key: " + key + "\nCertificate: " + cert + "\nCA Certificate: " + ca + "\nBootstrap: false\n",
		"ovn-sbctl get-connection": "read-write role=\"ovn-controller\" pssl:6642:10.198.0.1\n",
	}
	for _, c := range []struct {
		name     string
		out      map[string]string
		ssl, con bool
	}{
		{"unchanged", configured, false, false},
		{"empty", map[string]string{}, true, true},
		{"other paths", map[string]string{"ovn-sbctl get-ssl": "Private key: /x\nCertificate: /y\nCA Certificate: /z\n", "ovn-sbctl get-connection": configured["ovn-sbctl get-connection"]}, true, false},
		{"other connection", map[string]string{"ovn-sbctl get-ssl": configured["ovn-sbctl get-ssl"], "ovn-sbctl get-connection": "read-write pssl:6642:10.198.0.1\n"}, false, true},
	} {
		f := &fakeRun{out: c.out}
		if err := applySB(f.run, goodSB, dir); err != nil {
			t.Fatal(err)
		}
		joined := strings.Join(f.cmds, "\n")
		if strings.Contains(joined, "set-ssl") != c.ssl || strings.Contains(joined, "set-connection") != c.con {
			t.Errorf("%s: %v", c.name, f.cmds)
		}
	}
}

func TestApplySBWritesOnce(t *testing.T) {
	dir := t.TempDir()
	var calls []string
	run := func(args ...string) (string, error) {
		calls = append(calls, strings.Join(args, " "))
		return "", nil
	}
	if err := applySB(run, goodSB, dir); err != nil {
		t.Fatal(err)
	}
	key := filepath.Join(dir, "sb-key.pem")
	fi, err := os.Stat(key)
	if err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("key file %v %v", fi, err)
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 3 {
		t.Fatalf("leftover files: %v", entries)
	}
	for name, want := range map[string]string{"sb-key.pem": "key", "sb-cert.pem": "cert", "sb-ca.pem": "ca"} {
		if b, _ := os.ReadFile(filepath.Join(dir, name)); string(b) != want {
			t.Errorf("%s = %q", name, b)
		}
	}
	old := time.Now().Add(-time.Hour)
	for _, n := range []string{"sb-key.pem", "sb-cert.pem", "sb-ca.pem"} {
		os.Chtimes(filepath.Join(dir, n), old, old)
	}
	calls = nil
	if err := applySB(run, goodSB, dir); err != nil {
		t.Fatal(err)
	}
	for _, n := range []string{"sb-key.pem", "sb-cert.pem", "sb-ca.pem"} {
		if fi, _ := os.Stat(filepath.Join(dir, n)); !fi.ModTime().Equal(old) {
			t.Errorf("%s rewritten", n)
		}
	}
	joined := strings.Join(calls, "\n")
	if !strings.Contains(joined, "set-ssl "+key) || !strings.Contains(joined, "set-connection role=ovn-controller pssl:6642:10.198.0.1") {
		t.Errorf("calls %v", calls)
	}
}

func TestWithQuarantine(t *testing.T) {
	st := proto.GatewayState{Chassis: []proto.GatewayChassis{{NodeID: nodeA}, {NodeID: nodeB, Quarantined: true}, {NodeID: "n3"}}}
	got := withQuarantine(st, map[string]bool{nodeA: true, "hub-gw": true})
	if !got.Chassis[0].Quarantined || !got.Chassis[1].Quarantined || got.Chassis[2].Quarantined {
		t.Fatalf("%+v", got.Chassis)
	}
	if st.Chassis[0].Quarantined {
		t.Fatal("input modified")
	}
}

func TestCleanChassis(t *testing.T) {
	var calls []string
	run := func(args ...string) (string, error) {
		c := strings.Join(args, " ")
		calls = append(calls, c)
		if c == "ovn-sbctl --bare --columns=name list chassis" {
			return nodeA + "\n\n" + nodeB + "\nhub-gw\nstranger\n" + orgC + "\n", nil
		}
		return "", nil
	}
	st := proto.GatewayState{Chassis: []proto.GatewayChassis{{NodeID: nodeA}, {NodeID: nodeB, Quarantined: true}, {NodeID: "offline-node"}}}
	if err := cleanChassis(run, st); err != nil {
		t.Fatal(err)
	}
	var del []string
	for _, c := range calls {
		if strings.Contains(c, "chassis-del") {
			del = append(del, c)
		}
	}
	want := "ovn-sbctl chassis-del " + nodeB + "\novn-sbctl chassis-del stranger\novn-sbctl chassis-del " + orgC
	if strings.Join(del, "\n") != want {
		t.Fatalf("deleted %v", del)
	}
}

func TestApplyCleansChassisAfterQuarantineRules(t *testing.T) {
	var order []string
	run := func(args ...string) (string, error) {
		switch {
		case args[0] == "nft":
			order = append(order, "nft")
		case len(args) > 1 && args[1] == "chassis-del":
			order = append(order, "chassis-del")
		case strings.Join(args, " ") == "ovn-sbctl --bare --columns=name list chassis":
			return "gone\n", nil
		}
		return "", nil
	}
	_ = apply(run, proto.GatewayState{UnderlayKey: "a", ClientsKey: "b", GuestMTU: 1300, SB: goodSB}, t.TempDir())
	if strings.Join(order, ",") != "nft,chassis-del" {
		t.Fatalf("order %v", order)
	}
}

// guardPolls runs the guard on successive SB snapshots, as the gateway loop does, and returns the confirmed offenders of each poll.
func guardPolls(t *testing.T, nb string, polls ...string) [][]string {
	t.Helper()
	var last map[claim]bool
	var out [][]string
	for _, sb := range polls {
		run := func(args ...string) (string, error) {
			c := strings.Join(args, " ")
			switch {
			case strings.Contains(c, "list logical_switch_port"):
				return nb, nil
			case strings.Contains(c, "list chassis"):
				return "ch-a,node-a\nch-b,node-b\nch-gw,hub-gw\n", nil
			case strings.Contains(c, "list port_binding"):
				return sb, nil
			}
			return "", nil
		}
		_, claims, err := guardBindings(run)
		if err != nil {
			t.Fatal(err)
		}
		var offenders []string
		last, offenders = confirmClaims(last, claims)
		out = append(out, offenders)
	}
	return out
}

func TestGuardQuarantineRaces(t *testing.T) {
	nb := "vm1-mgmt,requested-chassis=node-a\ngw0,requested-chassis=hub-gw\n"
	foreign := "u1,vm1-mgmt,ch-b,\n"
	for _, c := range []struct {
		name  string
		nb    string
		polls []string
		want  string
	}{
		{"VM deleted, port gone from NB while still bound", "gw0,requested-chassis=hub-gw\n", []string{"u1,vm1-mgmt,ch-a,\n", "u1,vm1-mgmt,ch-a,\n"}, ""},
		{"network detached, port gone from NB", nb, []string{"u5,vm1-net,ch-a,\n", "u5,vm1-net,ch-a,\n", "u5,vm1-net,ch-a,\n"}, ""},
		{"VM created, SB ahead of the NB read", "", []string{"u1,vm1-mgmt,ch-a,\n", "u1,vm1-mgmt,ch-a,\n"}, ""},
		{"single-poll mismatch", nb, []string{foreign, "u1,vm1-mgmt,ch-a,\n", foreign}, ""},
		{"two-poll mismatch", nb, []string{foreign, foreign}, "node-b"},
		{"two polls on different ports", nb + "vm2-mgmt,requested-chassis=node-a\n", []string{foreign, "u2,vm2-mgmt,ch-b,\n"}, ""},
		{"gateway port claimed twice", nb, []string{"u3,cr-lrp-transit,ch-b,\n", "u3,cr-lrp-transit,ch-b,\n"}, "node-b"},
		{"gw0 claimed twice", nb, []string{"u4,gw0,ch-b,\n", "u4,gw0,ch-b,\n"}, "node-b"},
	} {
		var got []string
		for _, o := range guardPolls(t, c.nb, c.polls...) {
			got = append(got, o...)
		}
		if strings.Join(got, ",") != c.want {
			t.Errorf("%s: quarantined %v", c.name, got)
		}
	}
}

func TestEndpointsParse(t *testing.T) {
	run := func(args ...string) (string, error) {
		if strings.Join(args, " ") != "wg show wg-underlay endpoints" {
			t.Fatalf("args %v", args)
		}
		return "keyA=\t(none)\nkeyB=\t203.0.113.5:40001\n", nil
	}
	got, err := endpoints(run, "wg-underlay")
	if err != nil || len(got) != 1 || got["keyB="] != "203.0.113.5:40001" {
		t.Fatalf("got %v %v", got, err)
	}
}

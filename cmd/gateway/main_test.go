package main

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
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
		case c == "ovn-sbctl --bare --columns=_uuid find chassis name=hub-gw":
			return "ch-gw\n", nil
		case strings.Contains(c, "list port_binding"):
			return strings.Join([]string{
				"u1,vm1-mgmt,ch-a,ch-a,,",     // bound where requested
				"u2,vm2-mgmt,ch-b,ch-a,,",     // foreign claim
				"u3,vm3-mgmt,,ch-a,,",         // unbound
				"u4,cr-lrp-transit,ch-gw,,,",  // gateway port on hub-gw
				"u5,cr-lrp-transit2,ch-b,,,",  // gateway port claimed by a node
				"u6,vm6-mgmt,ch-a,ch-a,ch-b,", // additional chassis not requested
				"u7,vm7-mgmt,ch-a,ch-a,ch-b,ch-b",
				"u8,patch-lr,,,,",
			}, "\n") + "\n", nil
		}
		return "", nil
	}
	got, err := guardBindings(run)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(got, ",") != "vm2-mgmt,cr-lrp-transit2,vm6-mgmt" {
		t.Fatalf("rebound %v", got)
	}
	var clears []string
	for _, c := range calls {
		if strings.Contains(c, " clear ") {
			clears = append(clears, c)
		}
	}
	want := []string{
		"ovn-sbctl clear port_binding u2 chassis",
		"ovn-sbctl clear port_binding u5 chassis",
		"ovn-sbctl clear port_binding u6 additional_chassis",
	}
	if strings.Join(clears, "\n") != strings.Join(want, "\n") {
		t.Fatalf("clears %v", clears)
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

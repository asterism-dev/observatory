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
		if strings.Contains(c, "list port_binding") {
			return "u1,vm1-mgmt,ch-a,ch-a\nu2,vm2-mgmt,ch-b,ch-a\nu3,vm3-mgmt,,ch-a\nu4,cr-lrp-transit,ch-gw,\n", nil
		}
		return "", nil
	}
	got, err := guardBindings(run)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0] != "vm2-mgmt" {
		t.Fatalf("rebound %v", got)
	}
	var clears []string
	for _, c := range calls {
		if strings.Contains(c, " clear ") {
			clears = append(clears, c)
		}
	}
	if len(clears) != 1 || clears[0] != "ovn-sbctl clear port_binding u2 chassis" {
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

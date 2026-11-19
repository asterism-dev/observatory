package chassisconf

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func testConfig() Config {
	return Config{
		Name: "node-1", PrivateKey: "priv", Address: "10.198.0.11/16", HubKey: "hubpub",
		Endpoint: "gw.test:51820", SBRemote: "ssl:10.198.0.1:6642", MTU: 1420,
		CACert: "CA PEM\n", Cert: "CERT PEM\n", KeyFile: "/var/lib/observatory/vms/ovn-key.pem",
	}
}

func TestApplyWritesUnderlayAndSSL(t *testing.T) {
	root := t.TempDir()
	c := testConfig()
	var calls []string
	run := func(name string, args ...string) error {
		calls = append(calls, name+" "+strings.Join(args, " "))
		return nil
	}

	changed, err := Apply(c, root, run)
	if err != nil || !changed {
		t.Fatalf("first call: changed=%v err=%v", changed, err)
	}
	conf, err := os.ReadFile(filepath.Join(root, "etc/wireguard/wg-underlay.conf"))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"AllowedIPs = 10.198.0.0/16", "Endpoint = gw.test:51820", "MTU = 1420", "Address = 10.198.0.11/16", "PersistentKeepalive = 25"} {
		if !strings.Contains(string(conf), want) {
			t.Errorf("conf missing %q:\n%s", want, conf)
		}
	}
	if b, _ := os.ReadFile(filepath.Join(root, "etc/ovn/ovn-ca.pem")); string(b) != c.CACert {
		t.Errorf("ovn-ca.pem = %q", b)
	}
	if b, _ := os.ReadFile(filepath.Join(root, "etc/ovn/ovn-chassis-cert.pem")); string(b) != c.Cert {
		t.Errorf("ovn-chassis-cert.pem = %q", b)
	}
	if id, _ := os.ReadFile(filepath.Join(root, "etc/openvswitch/system-id.conf")); string(id) != "node-1" {
		t.Errorf("system-id.conf = %q", id)
	}
	joined := strings.Join(calls, "\n")
	for _, want := range []string{
		"ovs-vsctl set-ssl " + c.KeyFile + " " + root + "/etc/ovn/ovn-chassis-cert.pem " + root + "/etc/ovn/ovn-ca.pem",
		"external_ids:ovn-remote=ssl:10.198.0.1:6642",
		"external_ids:ovn-encap-ip=10.198.0.11",
		"systemctl restart",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("calls missing %q:\n%s", want, joined)
		}
	}

	calls = nil
	changed, err = Apply(c, root, run)
	if err != nil || changed {
		t.Fatalf("second call: changed=%v err=%v", changed, err)
	}
	if strings.Contains(strings.Join(calls, "\n"), "systemctl restart") {
		t.Errorf("unexpected restart: %v", calls)
	}
}

func TestApplyRestartsWhenCertChanges(t *testing.T) {
	root := t.TempDir()
	c := testConfig()
	var calls []string
	run := func(name string, args ...string) error {
		calls = append(calls, name+" "+strings.Join(args, " "))
		return nil
	}
	if _, err := Apply(c, root, run); err != nil {
		t.Fatal(err)
	}
	calls = nil
	c.Cert = "NEW CERT\n"
	if changed, err := Apply(c, root, run); err != nil || !changed {
		t.Fatalf("changed=%v err=%v", changed, err)
	}
	if !strings.Contains(strings.Join(calls, "\n"), "systemctl restart") {
		t.Errorf("cert change did not restart: %v", calls)
	}
}

func TestApplyRetriesRestartAfterFailure(t *testing.T) {
	root := t.TempDir()
	c := testConfig()
	failing := func(name string, args ...string) error {
		if name == "systemctl" {
			return errors.New("boom")
		}
		return nil
	}
	if _, err := Apply(c, root, failing); err == nil {
		t.Fatal("expected error")
	}
	if _, err := os.Stat(filepath.Join(root, "run/observatory-chassis-restart-pending")); err != nil {
		t.Fatalf("restart marker missing: %v", err)
	}

	var calls []string
	ok := func(name string, args ...string) error {
		calls = append(calls, name+" "+strings.Join(args, " "))
		return nil
	}
	if _, err := Apply(c, root, ok); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(strings.Join(calls, "\n"), "restart") {
		t.Errorf("retry did not restart: %v", calls)
	}

	calls = nil
	if _, err := Apply(c, root, ok); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(strings.Join(calls, "\n"), "restart") {
		t.Errorf("restart after success: %v", calls)
	}
}

func TestApplyRequiresSSLMaterial(t *testing.T) {
	c := testConfig()
	c.KeyFile = ""
	if _, err := Apply(c, t.TempDir(), func(string, ...string) error { return nil }); err == nil {
		t.Fatal("expected error without key file")
	}
}

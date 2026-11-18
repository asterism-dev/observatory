package guest

import (
	"bufio"
	"encoding/json"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"
)

// startAgent runs vmm-agent.py on a unix socket against root and a seed dir holding the boot files.
func startAgent(t *testing.T, root string, env ...string) string {
	t.Helper()
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 not installed")
	}
	dir, err := os.MkdirTemp("", "ga")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	seed := filepath.Join(dir, "seed")
	files := map[string]string{
		"vmm-boot.sh": BootScript, "vmm-agent.py": AgentScript, "vmm-env": `{"A":"1"}`,
		"vmm-authorized-keys": "ssh-ed25519 AAAA one@host\n", "meta-data": "instance-id: i-1\nlocal-hostname: web-1\n",
		"network-config": "version: 2\nethernets:\n  primary:\n    dhcp4: true\n",
	}
	if err := os.Mkdir(seed, 0o755); err != nil {
		t.Fatal(err)
	}
	for name, s := range files {
		if err := os.WriteFile(filepath.Join(seed, name), []byte(s), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	sock := filepath.Join(dir, "s")
	cmd := exec.Command("python3", filepath.Join(seed, "vmm-agent.py"))
	cmd.Env = append(append(os.Environ(), "VMM_AGENT_UNIX="+sock, "VMM_ROOT="+root, "VMM_SEED_DIR="+seed), env...)
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cmd.Process.Kill(); cmd.Wait() })
	for i := 0; i < 100; i++ {
		if _, err := os.Stat(sock); err == nil {
			return sock
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal("agent did not listen")
	return ""
}

type agentReply struct {
	OK    bool   `json:"ok"`
	Error string `json:"error"`
}

func callAgent(t *testing.T, sock, req string) agentReply {
	t.Helper()
	return callAgentRaw(t, sock, []byte(req+"\n"))
}

func callAgentRaw(t *testing.T, sock string, req []byte) agentReply {
	t.Helper()
	c, err := net.Dial("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	c.SetDeadline(time.Now().Add(30 * time.Second))
	// The agent may stop reading an oversized request before it is fully written.
	go c.Write(req)
	line, err := bufio.NewReader(c).ReadBytes('\n')
	if err != nil {
		t.Fatal(err)
	}
	var r agentReply
	if err := json.Unmarshal(line, &r); err != nil {
		t.Fatalf("reply %q: %v", line, err)
	}
	return r
}

func TestAgentResumeAppliesSeed(t *testing.T) {
	root := t.TempDir()
	sock := startAgent(t, root)
	if r := callAgent(t, sock, `{"op":"resume","time":5}`); !r.OK {
		t.Fatalf("resume: %+v", r)
	}
	b, err := os.ReadFile(filepath.Join(root, "etc/vmm/env.sh"))
	if err != nil || !strings.Contains(string(b), "export A='1'") {
		t.Fatalf("env.sh = %q, %v", b, err)
	}
}

func TestAgentTakeoffNewIdentity(t *testing.T) {
	root := t.TempDir()
	old := "0123456789abcdef0123456789abcdef"
	if err := os.MkdirAll(filepath.Join(root, "etc"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "etc/machine-id"), []byte(old+"\n"), 0o444); err != nil {
		t.Fatal(err)
	}
	cloud := filepath.Join(root, "var/lib/cloud")
	if err := os.MkdirAll(filepath.Join(cloud, "instances/tpl-1/sem"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(cloud, "data"), 0o755); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(cloud, "instances/tpl-1/sem/config_ssh"), nil, 0o644)
	os.WriteFile(filepath.Join(cloud, "data/instance-id"), []byte("tpl-1\n"), 0o644)
	if err := os.Symlink("/var/lib/cloud/instances/tpl-1", filepath.Join(cloud, "instance")); err != nil {
		t.Fatal(err)
	}
	sock := startAgent(t, root)
	if r := callAgent(t, sock, `{"op":"takeoff","time":5,"entropy":"AAEC"}`); !r.OK {
		t.Fatalf("takeoff: %+v", r)
	}
	// cloud-init must see the seed's instance as the one already set up, or the first reboot makes new host keys.
	if _, err := os.Stat(filepath.Join(cloud, "instances/i-1/sem/config_ssh")); err != nil {
		t.Errorf("instance dir not moved to i-1: %v", err)
	}
	if l, _ := os.Readlink(filepath.Join(cloud, "instance")); l != "/var/lib/cloud/instances/i-1" {
		t.Errorf("instance link = %q", l)
	}
	if b, _ := os.ReadFile(filepath.Join(cloud, "data/instance-id")); string(b) != "i-1\n" {
		t.Errorf("data/instance-id = %q", b)
	}
	id, _ := os.ReadFile(filepath.Join(root, "etc/machine-id"))
	if s := strings.TrimSpace(string(id)); !regexp.MustCompile(`^[0-9a-f]{32}$`).MatchString(s) || s == old {
		t.Fatalf("machine-id = %q", id)
	}
	if h, _ := os.ReadFile(filepath.Join(root, "etc/hostname")); string(h) != "web-1\n" {
		t.Fatalf("hostname = %q", h)
	}
	if n, _ := os.ReadFile(filepath.Join(root, "etc/netplan/50-cloud-init.yaml")); string(n) != "network:\n  version: 2\n  ethernets:\n    primary:\n      dhcp4: true\n" {
		t.Fatalf("netplan = %q", n)
	}
}

func TestAgentTakeoffDanglingInstanceLink(t *testing.T) {
	root := t.TempDir()
	cloud := filepath.Join(root, "var/lib/cloud")
	for _, d := range []string{cloud, filepath.Join(root, "etc")} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink("/var/lib/cloud/instances/gone", filepath.Join(cloud, "instance")); err != nil {
		t.Fatal(err)
	}
	sock := startAgent(t, root)
	if r := callAgent(t, sock, `{"op":"takeoff","time":5,"entropy":"AAEC"}`); !r.OK {
		t.Fatalf("takeoff: %+v", r)
	}
	if l, _ := os.Readlink(filepath.Join(cloud, "instance")); l != "/var/lib/cloud/instances/i-1" {
		t.Errorf("instance link = %q", l)
	}
	if b, _ := os.ReadFile(filepath.Join(cloud, "data/instance-id")); string(b) != "i-1\n" {
		t.Errorf("data/instance-id = %q", b)
	}
}

func TestAgentUnknownOp(t *testing.T) {
	sock := startAgent(t, t.TempDir())
	if r := callAgent(t, sock, `{"op":"rm"}`); r.OK || r.Error != "unknown op" {
		t.Fatalf("reply = %+v", r)
	}
}

func TestAgentRejectsBadRequests(t *testing.T) {
	sock := startAgent(t, t.TempDir())
	for _, tc := range []struct{ req, err string }{
		{`{"op":"takeoff","macs":["02:00:00:00:00:0G"]}`, "bad macs"},
		{`{"op":"takeoff","entropy":"!!"}`, ""},
		{`{"op":"resume","time":true}`, "bad time"},
		{`{"op":"resume","time":-1}`, "bad time"},
	} {
		if r := callAgent(t, sock, tc.req); r.OK || !strings.Contains(r.Error, tc.err) {
			t.Fatalf("%s: reply = %+v, want error %q", tc.req, r, tc.err)
		}
	}
	if r := callAgentRaw(t, sock, []byte(strings.Repeat("x", 70<<10)+"\n")); r.OK || r.Error != "request too large" {
		t.Fatalf("oversized: reply = %+v", r)
	}
}

func TestAgentTrustInstallsCerts(t *testing.T) {
	root := t.TempDir()
	sock := startAgent(t, root)
	pem := "-----BEGIN CERTIFICATE-----\nMIIBdGVzdA==\n-----END CERTIFICATE-----\n"
	id1, id2 := "0b6a2d1e-7c1f-4c55-9a43-2f0f3c5c1e01", "0b6a2d1e-7c1f-4c55-9a43-2f0f3c5c1e02"
	dir := filepath.Join(root, "usr/local/share/ca-certificates")
	req := func(id, pem string) string {
		b, _ := json.Marshal(map[string]any{"op": "trust", "certs": map[string]string{id: pem}})
		return string(b)
	}
	if r := callAgent(t, sock, req(id1, pem)); !r.OK {
		t.Fatalf("trust: %+v", r)
	}
	if b, err := os.ReadFile(filepath.Join(dir, "observatory-egress-"+id1+".crt")); err != nil || string(b) != pem {
		t.Fatalf("cert 1 = %q, %v", b, err)
	}
	if st, _ := os.Stat(filepath.Join(dir, "observatory-egress-"+id1+".crt")); st.Mode().Perm() != 0o644 {
		t.Fatalf("cert mode = %v", st.Mode())
	}
	if r := callAgent(t, sock, req(id2, pem)); !r.OK {
		t.Fatalf("trust: %+v", r)
	}
	if _, err := os.Stat(filepath.Join(dir, "observatory-egress-"+id1+".crt")); !os.IsNotExist(err) {
		t.Fatalf("cert 1 not removed: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "observatory-egress-"+id2+".crt")); err != nil {
		t.Fatal(err)
	}
	key := "-----BEGIN EC PRIVATE KEY-----\nMHcC\n-----END EC PRIVATE KEY-----\n"
	for _, bad := range []string{req("../x", pem), req(id1, pem+key), req(id1, key), req(id1, "x"), req(id1, strings.Repeat(pem, 400))} {
		if r := callAgent(t, sock, bad); r.OK {
			t.Fatalf("%.80s: accepted", bad)
		}
	}
	if _, err := os.Stat(filepath.Join(dir, "observatory-egress-"+id2+".crt")); err != nil {
		t.Fatalf("rejected request changed the certs: %v", err)
	}
}

func TestAgentTrustRetriesFailedUpdate(t *testing.T) {
	root, bin := t.TempDir(), t.TempDir()
	runs, fail := filepath.Join(bin, "runs"), filepath.Join(bin, "fail")
	update := filepath.Join(bin, "update-ca")
	if err := os.WriteFile(update, []byte("#!/bin/sh\necho \"$*\" >>"+runs+"\n[ ! -e "+fail+" ]\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(fail, nil, 0o644)
	sock := startAgent(t, root, "VMM_UPDATE_CA="+update)
	id := "0b6a2d1e-7c1f-4c55-9a43-2f0f3c5c1e01"
	pem := "-----BEGIN CERTIFICATE-----\nMIIBdGVzdA==\n-----END CERTIFICATE-----\n"
	b, _ := json.Marshal(map[string]any{"op": "trust", "certs": map[string]string{id: pem}})
	crt := filepath.Join(root, "usr/local/share/ca-certificates/observatory-egress-"+id+".crt")
	if r := callAgent(t, sock, string(b)); r.OK {
		t.Fatal("trust succeeded with a failing update")
	}
	os.Remove(fail)
	for i := 0; i < 2; i++ {
		if r := callAgent(t, sock, string(b)); !r.OK {
			t.Fatalf("trust: %+v", r)
		}
	}
	if _, err := os.Stat(crt); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(runs); string(got) != "--fresh\n--fresh\n--fresh\n" {
		t.Fatalf("update runs = %q", got)
	}
}

func TestAgentTakeoffToleratesBadCAs(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "etc"), 0o755); err != nil {
		t.Fatal(err)
	}
	sock := startAgent(t, root)
	if err := os.WriteFile(filepath.Join(filepath.Dir(sock), "seed", "vmm-egress-cas"), []byte(`{"x":"y"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if r := callAgent(t, sock, `{"op":"takeoff","time":5,"entropy":"AAEC"}`); !r.OK {
		t.Fatalf("takeoff: %+v", r)
	}
	if h, _ := os.ReadFile(filepath.Join(root, "etc/hostname")); string(h) != "web-1\n" {
		t.Fatalf("hostname = %q", h)
	}
}

func TestAgentTrustRejectsNonObjectCerts(t *testing.T) {
	sock := startAgent(t, t.TempDir())
	for _, certs := range []string{`[]`, `""`, `0`, `false`, `null`} {
		if r := callAgent(t, sock, `{"op":"trust","certs":`+certs+`}`); r.OK || r.Error != "bad certs" {
			t.Fatalf("certs %s: reply = %+v", certs, r)
		}
	}
}

// A boot whose update failed must update on the next boot even though the cert files are in place by then.
func TestInstallCAsRetriesFailedUpdate(t *testing.T) {
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 not installed")
	}
	root, bin := t.TempDir(), t.TempDir()
	runs, fail := filepath.Join(bin, "runs"), filepath.Join(bin, "fail")
	update := filepath.Join(bin, "update-ca")
	if err := os.WriteFile(update, []byte("#!/bin/sh\necho \"$*\" >>"+runs+"\n[ ! -e "+fail+" ]\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	script, cas := filepath.Join(bin, "vmm-agent.py"), filepath.Join(bin, "cas.json")
	os.WriteFile(script, []byte(AgentScript), 0o755)
	b, _ := json.Marshal(map[string]string{"0b6a2d1e-7c1f-4c55-9a43-2f0f3c5c1e01": "-----BEGIN CERTIFICATE-----\nMIIBdGVzdA==\n-----END CERTIFICATE-----\n"})
	os.WriteFile(cas, b, 0o644)
	install := func() error {
		cmd := exec.Command("python3", "-I", script, "install-cas", cas)
		cmd.Env = append(os.Environ(), "VMM_ROOT="+root, "VMM_UPDATE_CA="+update)
		return cmd.Run()
	}
	os.WriteFile(fail, nil, 0o644)
	if install() == nil {
		t.Fatal("install-cas succeeded with a failing update")
	}
	os.Remove(fail)
	for i := 0; i < 2; i++ {
		if err := install(); err != nil {
			t.Fatal(err)
		}
	}
	if got, _ := os.ReadFile(runs); string(got) != "\n--fresh\n" {
		t.Fatalf("update runs = %q", got)
	}
}

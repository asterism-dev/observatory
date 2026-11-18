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
func startAgent(t *testing.T, root string) string {
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
	cmd.Env = append(os.Environ(), "VMM_AGENT_UNIX="+sock, "VMM_ROOT="+root, "VMM_SEED_DIR="+seed)
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

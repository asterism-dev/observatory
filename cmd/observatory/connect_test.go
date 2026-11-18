package main

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

const (
	ownDevice   = `[{"id":"d1","name":"laptop","ip":"10.9.0.2","owner":{"id":"u1","name":"Ada Lovelace"},"createdAt":"2026-11-19T13:00:00Z","lastHandshake":null,"canDelete":true}]`
	otherDevice = `[{"id":"d2","name":"x","ip":"10.9.0.3","owner":{"id":"u2","name":"Bob"},"createdAt":"2026-11-19T13:00:00Z","lastHandshake":null,"canDelete":false}]`
)

type execCall struct {
	path string
	argv []string
}

func connectHub(t *testing.T, vm, devices string) (*fakeHub, *cli, *[]execCall, *[]execCall, func() string) {
	h := newFakeHub(t)
	meOneOrg(h)
	h.on("GET /api/orgs/o1/vms", 200, `{"vms":[`+vm+`],"nodes":[]}`)
	h.on("GET /api/orgs/o1/devices", 200, devices)
	c, _, errb := newTestCLI(t, h)
	var execs, runs []execCall
	c.exec = func(path string, argv, env []string) error {
		execs = append(execs, execCall{path, argv})
		return nil
	}
	c.runProg = func(path string, argv []string) error {
		runs = append(runs, execCall{path, argv})
		return nil
	}
	c.lookPath = func(n string) (string, error) { return "/bin/" + n, nil }
	return h, c, &execs, &runs, errb.String
}

func withState(state string) string {
	return strings.Replace(vmDev, `"state":"running"`, `"state":"`+state+`"`, 1)
}

func TestSSHExecArgs(t *testing.T) {
	h, c, execs, _, _ := connectHub(t, vmDev, ownDevice)
	if code := run(c, []string{"ssh", "dev"}); code != 0 {
		t.Fatal(code)
	}
	if code := run(c, []string{"ssh", "dev", "--user", "root", "--", "-L", "8080:localhost:8080", "uptime"}); code != 0 {
		t.Fatal(code)
	}
	want := [][]string{{"ssh", "ubuntu@dev.internal"}, {"ssh", "root@dev.internal", "-L", "8080:localhost:8080", "uptime"}}
	for i, w := range want {
		if !reflect.DeepEqual((*execs)[i].argv, w) {
			t.Errorf("argv %v, want %v", (*execs)[i].argv, w)
		}
	}
	for _, r := range h.reqs {
		if r.Method == "POST" || r.Method == "PUT" || r.Method == "DELETE" {
			t.Errorf("unexpected %s %s", r.Method, r.Path)
		}
	}
}

func TestSSHRefusesStopped(t *testing.T) {
	_, c, execs, _, errb := connectHub(t, withState("stopped"), ownDevice)
	if code := run(c, []string{"ssh", "dev"}); code != 1 || len(*execs) != 0 || !strings.Contains(errb(), "observatory vm start dev") {
		t.Errorf("code %d execs %v stderr %q", code, *execs, errb())
	}
	_, c, execs, _, _ = connectHub(t, withState("suspended"), ownDevice)
	if code := run(c, []string{"ssh", "dev"}); code != 0 || len(*execs) != 1 {
		t.Errorf("code %d execs %v", code, *execs)
	}
}

func TestSSHWarnsWithoutDevice(t *testing.T) {
	_, c, execs, _, errb := connectHub(t, vmDev, otherDevice)
	if code := run(c, []string{"ssh", "dev"}); code != 0 || len(*execs) != 1 {
		t.Fatalf("code %d", code)
	}
	if !strings.Contains(errb(), "no device in org orga; run observatory device create <name> and import it") {
		t.Errorf("stderr %q", errb())
	}
}

func TestDesktopRunsThenAttaches(t *testing.T) {
	_, c, execs, runs, _ := connectHub(t, vmDev, ownDevice)
	if code := run(c, []string{"desktop", "dev"}); code != 0 {
		t.Fatal(code)
	}
	if !reflect.DeepEqual((*runs)[0].argv, []string{"ssh", "ubuntu@dev.internal", "vmm-desktop", "start"}) ||
		!reflect.DeepEqual((*execs)[0].argv, []string{"/bin/xpra", "attach", "ssh://ubuntu@dev.internal/100"}) {
		t.Errorf("runs %v execs %v", *runs, *execs)
	}

	_, c, execs, _, _ = connectHub(t, vmDev, ownDevice)
	c.runProg = func(string, []string) error {
		return exec.Command("sh", "-c", "exit 7").Run()
	}
	if code := run(c, []string{"desktop", "dev"}); code != 7 || len(*execs) != 0 {
		t.Errorf("code %d execs %v", code, *execs)
	}

	_, c, execs, _, errb := connectHub(t, vmDev, ownDevice)
	c.lookPath = func(n string) (string, error) {
		if n == "xpra" {
			return "", errors.New("not found")
		}
		return "/bin/" + n, nil
	}
	if _, err := os.Stat("/Applications/Xpra.app/Contents/MacOS/Xpra"); err == nil {
		t.Skip("xpra app installed")
	}
	if code := run(c, []string{"desktop", "dev"}); code != 1 || len(*execs) != 0 || !strings.Contains(errb(), "docs/guest.md") {
		t.Errorf("code %d stderr %q", code, errb())
	}
}

func TestSSHKeyAdd(t *testing.T) {
	h := newFakeHub(t)
	c, _, _ := newTestCLI(t, h)
	h.on("POST /api/me/ssh-keys", 201, `{"id":"k1","name":"ada@laptop","fingerprint":"SHA256:x","createdAt":"2026-11-19T13:00:00Z"}`)
	ssh := filepath.Join(os.Getenv("HOME"), ".ssh")
	if err := os.MkdirAll(ssh, 0o700); err != nil {
		t.Fatal(err)
	}
	pub := "ssh-ed25519 AAAAC3 ada@laptop"
	if err := os.WriteFile(filepath.Join(ssh, "id_ed25519.pub"), []byte(pub+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if code := run(c, []string{"ssh-key", "add"}); code != 0 {
		t.Fatal(code)
	}
	want := `{"name":"ada@laptop","publicKey":"ssh-ed25519 AAAAC3 ada@laptop"}`
	if got := h.calls("POST /api/me/ssh-keys")[0].Body; got != want {
		t.Errorf("body %s, want %s", got, want)
	}

	priv := filepath.Join(ssh, "id_rsa")
	if err := os.WriteFile(priv, []byte("-----BEGIN OPENSSH PRIVATE KEY-----\nx\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if code := run(c, []string{"ssh-key", "add", priv}); code != 2 || len(h.calls("POST /api/me/ssh-keys")) != 1 {
		t.Errorf("code %d", code)
	}
}

func TestSSHKeyDeleteConfirms(t *testing.T) {
	h := newFakeHub(t)
	c, _, _ := newTestCLI(t, h)
	h.on("GET /api/me/ssh-keys", 200, `[{"id":"k1","name":"laptop","fingerprint":"SHA256:x","createdAt":"2026-11-19T13:00:00Z"}]`)
	h.on("DELETE /api/me/ssh-keys/k1", 204, "")
	if code := run(c, []string{"ssh-key", "delete", "laptop"}); code != 2 || len(h.calls("DELETE /api/me/ssh-keys/k1")) != 0 {
		t.Fatalf("code %d", code)
	}
	if code := run(c, []string{"ssh-key", "delete", "laptop", "--yes"}); code != 0 || len(h.calls("DELETE /api/me/ssh-keys/k1")) != 1 {
		t.Errorf("code %d", code)
	}
}

func TestSSHRejectsBadUser(t *testing.T) {
	for _, cmd := range []string{"ssh", "desktop"} {
		for _, u := range []string{"-oProxyCommand=x", "a b"} {
			_, c, execs, runs, _ := connectHub(t, vmDev, ownDevice)
			if code := run(c, []string{cmd, "dev", "--user", u}); code != 2 || len(*execs)+len(*runs) != 0 {
				t.Errorf("%s %q: code %d", cmd, u, code)
			}
		}
	}
}

package guest

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

const chromeBin = "/opt/vmm/chrome/chrome"

// fakeBrowser runs vmm-browser.py against a fake agent-browser that prints its env and argv, a vmm-desktop that logs its calls and a fake proc root.
type fakeBrowser struct {
	dir, home, proc string
}

func newFakeBrowser(t *testing.T) *fakeBrowser {
	t.Helper()
	needPython(t)
	b := &fakeBrowser{dir: t.TempDir()}
	b.home = filepath.Join(b.dir, "home")
	b.proc = filepath.Join(b.dir, "proc")
	for _, d := range []string{b.home, b.proc} {
		if err := os.Mkdir(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	files := map[string]string{
		"vmm-browser.py": BrowserScript,
		"agent-browser":  "#!/bin/sh\nenv\nfor a in \"$@\"; do echo \"ARG=$a\"; done\n",
		"vmm-desktop":    "#!/bin/sh\necho \"$*\" >>\"" + filepath.Join(b.dir, "desktop.log") + "\"\n[ ! -e \"" + filepath.Join(b.dir, "desktop-fails") + "\" ] || { echo 'desktop broke' >&2; exit 1; }\n",
	}
	for name, s := range files {
		if err := os.WriteFile(filepath.Join(b.dir, name), []byte(s), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	return b
}

func (b *fakeBrowser) run(t *testing.T, env []string, args ...string) (int, string, string) {
	t.Helper()
	cmd := exec.Command("python3", append([]string{filepath.Join(b.dir, "vmm-browser.py")}, args...)...)
	cmd.Env = append([]string{"PATH=" + os.Getenv("PATH"), "HOME=" + b.home, "VMM_PROC_ROOT=" + b.proc,
		"VMM_AGENT_BROWSER_BIN=" + filepath.Join(b.dir, "agent-browser"), "VMM_DESKTOP_BIN=" + filepath.Join(b.dir, "vmm-desktop")}, env...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	if ee, ok := err.(*exec.ExitError); ok {
		return ee.ExitCode(), stdout.String(), stderr.String()
	} else if err != nil {
		t.Fatal(err)
	}
	return 0, stdout.String(), stderr.String()
}

func (b *fakeBrowser) desktopCalls() string {
	s, _ := os.ReadFile(filepath.Join(b.dir, "desktop.log"))
	return string(s)
}

func (b *fakeBrowser) profile() string {
	return filepath.Join(b.home, ".local/share/vmm-browser/default")
}

func lines(s string) map[string]bool {
	m := map[string]bool{}
	for l := range strings.Lines(s) {
		m[strings.TrimSuffix(l, "\n")] = true
	}
	return m
}

func TestBrowserStartsDesktopAndSetsDefaults(t *testing.T) {
	b := newFakeBrowser(t)
	code, out, stderr := b.run(t, []string{"DISPLAY=localhost:10"}, "open", "https://example.com", "--json")
	if code != 0 {
		t.Fatalf("exit %d: %s", code, stderr)
	}
	if got := b.desktopCalls(); got != "start\n" {
		t.Errorf("desktop calls = %q", got)
	}
	got := lines(out)
	for _, want := range []string{"DISPLAY=:100", "AGENT_BROWSER_HEADED=1", "AGENT_BROWSER_EXECUTABLE_PATH=" + chromeBin, "AGENT_BROWSER_SESSION=default",
		"AGENT_BROWSER_PROFILE=" + b.profile(), "AGENT_BROWSER_ARGS=--password-store=basic", "ARG=open", "ARG=https://example.com", "ARG=--json"} {
		if !got[want] {
			t.Errorf("output misses %q:\n%s", want, out)
		}
	}
	if !strings.Contains(out, "ARG=open\nARG=https://example.com\nARG=--json\n") {
		t.Errorf("argv not passed through in order:\n%s", out)
	}
}

func TestBrowserUserEnvWins(t *testing.T) {
	b := newFakeBrowser(t)
	code, out, stderr := b.run(t, []string{"AGENT_BROWSER_PROFILE=/tmp/mine", "AGENT_BROWSER_HEADED=0"}, "snapshot")
	if code != 0 {
		t.Fatalf("exit %d: %s", code, stderr)
	}
	got := lines(out)
	for _, want := range []string{"AGENT_BROWSER_PROFILE=/tmp/mine", "AGENT_BROWSER_HEADED=0", "DISPLAY=:100"} {
		if !got[want] {
			t.Errorf("output misses %q:\n%s", want, out)
		}
	}
}

func TestBrowserRefusesRemoteCDP(t *testing.T) {
	b := newFakeBrowser(t)
	for _, c := range []struct {
		env  []string
		args []string
	}{
		{nil, []string{"--cdp", "ws://10.0.0.5:9222", "snapshot"}},
		{nil, []string{"--cdp=http://example.com:9222", "snapshot"}},
		{nil, []string{"--args", "--remote-debugging-address=0.0.0.0", "open", "x"}},
		{nil, []string{"--args", "--no-sandbox,--remote-debugging-address=0.0.0.0", "open", "x"}},
		{[]string{"AGENT_BROWSER_ARGS=--remote-debugging-address=10.0.0.1"}, []string{"open", "x"}},
		{[]string{"AGENT_BROWSER_CDP=ws://192.168.64.1:9222"}, []string{"snapshot"}},
		{nil, []string{"--cdp", "ws://127.0.0.1@evil.example:9222", "snapshot"}},
		{nil, []string{"--cdp", `ws://127.0.0.1\@evil.example:9222`, "snapshot"}},
		{nil, []string{"--cdp", "evil.example:9222", "snapshot"}},
		{nil, []string{"connect", "wss://evil.example/devtools"}},
	} {
		code, out, stderr := b.run(t, c.env, c.args...)
		if code != 2 || out != "" || stderr == "" {
			t.Errorf("%v %v: exit %d, stdout %q, stderr %q; want exit 2 with a message and no agent-browser run", c.env, c.args, code, out, stderr)
		}
	}
	for _, c := range []struct {
		env  []string
		args []string
	}{
		{nil, []string{"--cdp", "9222", "snapshot"}},
		{nil, []string{"--cdp", "ws://127.0.0.1:9222/devtools/browser/x", "snapshot"}},
		{nil, []string{"--cdp=http://localhost:9222", "snapshot"}},
		{nil, []string{"--cdp", "ws://[::1]:9222", "snapshot"}},
		{nil, []string{"--args", "--remote-debugging-address=127.0.0.1", "open", "x"}},
		{nil, []string{"connect", "9222"}},
		{[]string{"AGENT_BROWSER_CDP=ws://localhost:9222"}, []string{"snapshot"}},
	} {
		if code, out, stderr := b.run(t, c.env, c.args...); code != 0 || !strings.Contains(out, "ARG=") {
			t.Errorf("%v %v: exit %d, %s; want agent-browser run", c.env, c.args, code, stderr)
		}
	}
}

func TestBrowserDesktopFailure(t *testing.T) {
	b := newFakeBrowser(t)
	if err := os.WriteFile(filepath.Join(b.dir, "desktop-fails"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	code, out, stderr := b.run(t, nil, "open", "x")
	if code != 1 || out != "" || !strings.Contains(stderr, "desktop broke") {
		t.Fatalf("exit %d, stdout %q, stderr %q", code, out, stderr)
	}
}

func TestBrowserRefusesRoot(t *testing.T) {
	b := newFakeBrowser(t)
	code, out, stderr := b.run(t, []string{"VMM_UID=0"}, "open", "x")
	if code != 1 || out != "" || !strings.Contains(stderr, "agent-browser: run as the ubuntu user, not root") {
		t.Fatalf("exit %d, stdout %q, stderr %q", code, out, stderr)
	}
	if got := b.desktopCalls(); got != "" {
		t.Errorf("desktop started as root: %q", got)
	}
}

func TestBrowserStaleLock(t *testing.T) {
	host, err := os.Hostname()
	if err != nil {
		t.Fatal(err)
	}
	b := newFakeBrowser(t)
	singletons := []string{"SingletonLock", "SingletonSocket", "SingletonCookie"}
	lock := func(target string) {
		t.Helper()
		if err := os.MkdirAll(b.profile(), 0o700); err != nil {
			t.Fatal(err)
		}
		for _, n := range singletons {
			os.Remove(filepath.Join(b.profile(), n))
		}
		for _, n := range singletons {
			if err := os.Symlink(target, filepath.Join(b.profile(), n)); err != nil {
				t.Fatal(err)
			}
		}
	}
	proc := func(pid int, exe string) {
		t.Helper()
		d := filepath.Join(b.proc, strconv.Itoa(pid))
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(exe, filepath.Join(d, "exe")); err != nil {
			t.Fatal(err)
		}
	}
	check := func(name string, wantKept bool) {
		t.Helper()
		if code, _, stderr := b.run(t, nil, "open", "x"); code != 0 {
			t.Fatalf("%s: exit %d: %s", name, code, stderr)
		}
		for _, n := range singletons {
			_, err := os.Lstat(filepath.Join(b.profile(), n))
			if kept := err == nil; kept != wantKept {
				t.Errorf("%s: %s kept = %v, want %v", name, n, kept, wantKept)
			}
		}
	}

	lock("otherhost-123")
	check("foreign host, no chrome", false)
	lock(host + "-4000000")
	check("this host, dead pid", false)
	proc(4242, "/usr/bin/bash")
	lock(host + "-4242")
	check("this host, live non-chrome pid", true)
	proc(4343, chromeBin)
	lock(host + "-4343")
	check("this host, live chrome", true)
	lock("otherhost-123")
	check("foreign host, chrome running", true)
}

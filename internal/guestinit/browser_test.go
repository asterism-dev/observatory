package guestinit

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
)

type fakeBrowser struct {
	*browser
	dir, home     string
	desktopCalls  int
	desktopErr    error
	execs         int
	execArgv, env []string
}

func newFakeBrowser(t *testing.T, env ...string) *fakeBrowser {
	t.Helper()
	b := &fakeBrowser{dir: t.TempDir()}
	b.home = filepath.Join(b.dir, "home")
	proc := filepath.Join(b.dir, "proc")
	for _, d := range []string{b.home, proc} {
		if err := os.Mkdir(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	b.browser = &browser{uid: 1000, env: append([]string{"PATH=/usr/bin", "HOME=" + b.home}, env...), proc: proc, binary: "/fake/agent-browser",
		desktop: func() error { b.desktopCalls++; return b.desktopErr },
		exec: func(path string, argv, env []string) error {
			b.execs++
			b.execArgv, b.env = argv, env
			return nil
		}}
	return b
}

func (b *fakeBrowser) run(t *testing.T, args ...string) (int, string) {
	t.Helper()
	var code int
	out := captureStderr(t, func() { code = b.browser.run(args) })
	return code, out
}

func (b *fakeBrowser) profile() string {
	return filepath.Join(b.home, ".local/share/vmm-browser/default")
}

func TestBrowserStartsDesktopAndSetsDefaults(t *testing.T) {
	b := newFakeBrowser(t, "DISPLAY=localhost:10")
	if code, out := b.run(t, "open", "https://example.com", "--json"); code != 0 || b.execs != 1 {
		t.Fatalf("exit %d (execs %d): %s", code, b.execs, out)
	}
	if b.desktopCalls != 1 {
		t.Errorf("desktop starts = %d", b.desktopCalls)
	}
	for _, want := range []string{"DISPLAY=:100", "AGENT_BROWSER_HEADED=1", "AGENT_BROWSER_EXECUTABLE_PATH=" + chromeBin, "AGENT_BROWSER_SESSION=default",
		"AGENT_BROWSER_PROFILE=" + b.profile(), "AGENT_BROWSER_ARGS=--password-store=basic"} {
		if !slices.Contains(b.env, want) {
			t.Errorf("env misses %q: %v", want, b.env)
		}
	}
	if slices.Contains(b.env, "DISPLAY=localhost:10") {
		t.Errorf("old DISPLAY kept: %v", b.env)
	}
	if got := strings.Join(b.execArgv, " "); got != "/fake/agent-browser open https://example.com --json" {
		t.Errorf("argv = %q", got)
	}
}

func TestBrowserUserEnvWins(t *testing.T) {
	b := newFakeBrowser(t, "AGENT_BROWSER_PROFILE=/tmp/mine", "AGENT_BROWSER_HEADED=0")
	if code, out := b.run(t, "snapshot"); code != 0 {
		t.Fatalf("exit %d: %s", code, out)
	}
	for _, want := range []string{"AGENT_BROWSER_PROFILE=/tmp/mine", "AGENT_BROWSER_HEADED=0", "DISPLAY=:100"} {
		if !slices.Contains(b.env, want) {
			t.Errorf("env misses %q: %v", want, b.env)
		}
	}
}

func TestBrowserRefusesRemoteCDP(t *testing.T) {
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
		{nil, []string{"--session", "work", "--headed", "false", "connect", "evil.example:9222"}},
		{nil, []string{"--args", "-remote-debugging-address=0.0.0.0,--remote-debugging-port=9222", "open", "x"}},
		{[]string{"AGENT_BROWSER_ARGS=--no-sandbox\n-remote-debugging-address=0.0.0.0"}, []string{"open", "x"}},
	} {
		b := newFakeBrowser(t, c.env...)
		if code, out := b.run(t, c.args...); code != 2 || out == "" || b.execs != 0 || b.desktopCalls != 0 {
			t.Errorf("%v %v: exit %d, stderr %q, execs %d; want exit 2 with a message and no agent-browser run", c.env, c.args, code, out, b.execs)
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
		{nil, []string{"--session", "work", "connect", "ws://localhost:9222"}},
		{nil, []string{"fill", "@e1", "connect", "https://example.com"}},
		{[]string{"AGENT_BROWSER_CDP=ws://localhost:9222"}, []string{"snapshot"}},
	} {
		b := newFakeBrowser(t, c.env...)
		if code, out := b.run(t, c.args...); code != 0 || b.execs != 1 {
			t.Errorf("%v %v: exit %d, %s; want agent-browser run", c.env, c.args, code, out)
		}
	}
}

func TestBrowserDesktopFailure(t *testing.T) {
	b := newFakeBrowser(t)
	b.desktopErr = desktopRefused("desktop broke")
	if code, out := b.run(t, "open", "x"); code != 1 || b.execs != 0 || !strings.Contains(out, "desktop broke") {
		t.Fatalf("exit %d, stderr %q", code, out)
	}
}

func TestBrowserDesktopUnreachable(t *testing.T) {
	b := newFakeBrowser(t)
	b.desktopErr = errors.New("no such file")
	if code, out := b.run(t, "open", "x"); code != 1 || b.execs != 0 || !strings.HasPrefix(out, "agent-browser: cannot start the desktop") {
		t.Fatalf("exit %d, stderr %q", code, out)
	}
}

func TestBrowserRefusesRoot(t *testing.T) {
	b := newFakeBrowser(t)
	b.uid = 0
	if code, out := b.run(t, "open", "x"); code != 1 || !strings.Contains(out, "agent-browser: run as the dev user, not root") {
		t.Fatalf("exit %d, stderr %q", code, out)
	}
	if b.desktopCalls != 0 || b.execs != 0 {
		t.Errorf("desktop started or browser run as root")
	}
}

func TestBrowserStaleLock(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("the fake proc entries are owned by the test user, who must not be root")
	}
	host, err := os.Hostname()
	if err != nil {
		t.Fatal(err)
	}
	b := newFakeBrowser(t)
	b.uid = os.Getuid()
	lock := func(target string) {
		t.Helper()
		if err := os.MkdirAll(b.profile(), 0o700); err != nil {
			t.Fatal(err)
		}
		for _, n := range singletons {
			os.Remove(filepath.Join(b.profile(), n))
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
		if code, out := b.run(t, "open", "x"); code != 0 {
			t.Fatalf("%s: exit %d: %s", name, code, out)
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

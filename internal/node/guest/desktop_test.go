package guest

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// fakeDesktop gives vmm-desktop a systemctl that logs its arguments and an xpra whose info answers from the n-th call on.
type fakeDesktop struct {
	dir, run string
}

func newFakeDesktop(t *testing.T, answerAt int, info string) *fakeDesktop {
	t.Helper()
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("sh not installed")
	}
	d := &fakeDesktop{dir: t.TempDir()}
	d.run = filepath.Join(d.dir, "run")
	if err := os.Mkdir(d.run, 0o700); err != nil {
		t.Fatal(err)
	}
	files := map[string]string{
		"vmm-desktop": DesktopScript,
		"systemctl":   "#!/bin/sh\necho \"$*\" >>\"" + filepath.Join(d.dir, "systemctl.log") + "\"\n",
		"xpra": "#!/bin/sh\nf=\"" + filepath.Join(d.dir, "calls") + "\"\nn=$(($(cat \"$f\" 2>/dev/null || echo 0) + 1))\necho $n >\"$f\"\n" +
			"[ \"$*\" = 'info :100' ] && [ $n -ge \"$(cat \"" + filepath.Join(d.dir, "answer-at") + "\")\" ] || exit 1\ncat \"" + filepath.Join(d.dir, "info") + "\"\n",
	}
	for name, s := range files {
		if err := os.WriteFile(filepath.Join(d.dir, name), []byte(s), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	d.set(t, answerAt, info)
	return d
}

func (d *fakeDesktop) set(t *testing.T, answerAt int, info string) {
	t.Helper()
	os.Remove(filepath.Join(d.dir, "calls"))
	os.Remove(filepath.Join(d.dir, "systemctl.log"))
	if err := os.WriteFile(filepath.Join(d.dir, "answer-at"), []byte(strconv.Itoa(answerAt)+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(d.dir, "info"), []byte(info), 0o644); err != nil {
		t.Fatal(err)
	}
}

func (d *fakeDesktop) exec(t *testing.T, env []string, args ...string) (int, string) {
	t.Helper()
	cmd := exec.Command("sh", append([]string{filepath.Join(d.dir, "vmm-desktop")}, args...)...)
	cmd.Env = append(append(os.Environ(), "XDG_RUNTIME_DIR="+d.run, "VMM_UID=1000",
		"VMM_SYSTEMCTL_BIN="+filepath.Join(d.dir, "systemctl"), "VMM_XPRA_BIN="+filepath.Join(d.dir, "xpra")), env...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	err := cmd.Run()
	if ee, ok := err.(*exec.ExitError); ok {
		return ee.ExitCode(), stderr.String()
	} else if err != nil {
		t.Fatal(err)
	}
	return 0, stderr.String()
}

func (d *fakeDesktop) systemctl() string {
	b, _ := os.ReadFile(filepath.Join(d.dir, "systemctl.log"))
	return string(b)
}

func (d *fakeDesktop) stamp() string { return filepath.Join(d.run, "vmm-desktop.idle-since") }

func TestDesktopStartWaitsForSocket(t *testing.T) {
	d := newFakeDesktop(t, 3, "clients=0\n")
	if code, errOut := d.exec(t, nil, "start"); code != 0 {
		t.Fatalf("start = %d: %s", code, errOut)
	}
	if got := d.systemctl(); got != "--user start vmm-desktop.service vmm-desktop-idle.timer\n" {
		t.Fatalf("systemctl = %q", got)
	}
	if b, _ := os.ReadFile(filepath.Join(d.dir, "calls")); strings.TrimSpace(string(b)) != "3" {
		t.Fatalf("xpra calls = %q", b)
	}

	d.set(t, 1000000, "clients=0\n")
	start := time.Now()
	if code, _ := d.exec(t, nil, "start"); code != 1 {
		t.Fatalf("start without an answer = %d", code)
	}
	if el := time.Since(start); el > 6*time.Second {
		t.Fatalf("start gave up after %v", el)
	}
}

func TestDesktopIdleStop(t *testing.T) {
	const idle = "clients=0\nclients.unauthenticated=0\nstate.windows=0\n"
	const stop = "--user stop vmm-desktop.service vmm-desktop-idle.timer\n"
	d := newFakeDesktop(t, 1, idle)
	age := func(min int) {
		t.Helper()
		if err := os.WriteFile(d.stamp(), nil, 0o644); err != nil {
			t.Fatal(err)
		}
		at := time.Now().Add(-time.Duration(min) * time.Minute)
		if err := os.Chtimes(d.stamp(), at, at); err != nil {
			t.Fatal(err)
		}
	}
	check := func(env ...string) {
		t.Helper()
		if code, errOut := d.exec(t, env, "idle-check"); code != 0 {
			t.Fatalf("idle-check = %d: %s", code, errOut)
		}
	}

	check()
	if _, err := os.Stat(d.stamp()); err != nil {
		t.Fatalf("no stamp after the first idle check: %v", err)
	}
	if got := d.systemctl(); got != "" {
		t.Fatalf("first idle check ran systemctl %q", got)
	}

	age(31)
	check()
	if got := d.systemctl(); got != stop {
		t.Fatalf("idle 31 min: systemctl = %q", got)
	}

	d.set(t, 1, idle)
	age(2)
	check("VMM_DESKTOP_IDLE_MINUTES=1")
	if got := d.systemctl(); got != stop {
		t.Fatalf("idle 2 min with a 1 min limit: systemctl = %q", got)
	}

	d.set(t, 1, "clients=1\nclients.unauthenticated=0\nstate.windows=0\n")
	age(31)
	check()
	if _, err := os.Stat(d.stamp()); !os.IsNotExist(err) {
		t.Fatalf("stamp kept with a client attached: %v", err)
	}
	if got := d.systemctl(); got != "" {
		t.Fatalf("client attached: systemctl = %q", got)
	}

	d.set(t, 1, "clients=0\nclients.unauthenticated=0\nstate.windows=2\n")
	age(31)
	check()
	if got := d.systemctl(); got != "" {
		t.Fatalf("window open: systemctl = %q", got)
	}
}

func TestDesktopRefusesRoot(t *testing.T) {
	d := newFakeDesktop(t, 1, "clients=0\n")
	code, errOut := d.exec(t, []string{"VMM_UID=0"}, "start")
	if code != 1 || !strings.Contains(errOut, "ubuntu") {
		t.Fatalf("start as root = %d: %q", code, errOut)
	}
	if got := d.systemctl(); got != "" {
		t.Fatalf("systemctl ran as root: %q", got)
	}
}

func TestDesktopUnitRemovesStaleSockets(t *testing.T) {
	unit, err := os.ReadFile(filepath.Join(installGuest(t), "etc/systemd/user/vmm-desktop.service"))
	if err != nil {
		t.Fatal(err)
	}
	var pre string
	for _, l := range strings.Split(string(unit), "\n") {
		if s, ok := strings.CutPrefix(l, "ExecStartPre=-/bin/sh -c "); ok {
			pre = strings.Trim(s, "'")
		}
	}
	if pre == "" {
		t.Fatalf("no ExecStartPre in:\n%s", unit)
	}
	run, home := t.TempDir(), t.TempDir()
	stale := []string{run + "/xpra/dev-100", run + "/xpra/100/socket", home + "/.xpra/dev-100", home + "/.xpra/src-100"}
	kept := []string{run + "/xpra/dev-101", run + "/xpra/100/xauthority", home + "/.xpra/dev-101"}
	for _, f := range append(stale, kept...) {
		if err := os.MkdirAll(filepath.Dir(f), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(f, nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	// %t and %h are systemd's runtime dir and home specifiers.
	cmd := strings.NewReplacer("%t", run, "%h", home).Replace(pre)
	if out, err := exec.Command("sh", "-c", cmd).CombinedOutput(); err != nil {
		t.Fatalf("%s: %v: %s", cmd, err, out)
	}
	for _, f := range stale {
		if _, err := os.Stat(f); err == nil {
			t.Errorf("stale socket %s left; xpra waits for it to time out on start", f)
		}
	}
	for _, f := range kept {
		if _, err := os.Stat(f); err != nil {
			t.Errorf("%s removed: %v", f, err)
		}
	}
}

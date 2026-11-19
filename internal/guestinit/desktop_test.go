package guestinit

import (
	"bytes"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

func captureStderr(t *testing.T, f func()) string {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	orig := os.Stderr
	os.Stderr = w
	f()
	os.Stderr = orig
	w.Close()
	out, _ := io.ReadAll(r)
	return string(out)
}

const fakeXpra = `#!/bin/sh
d=$FAKE_DIR
case $1 in
info)
	n=$(($(cat "$d/calls" 2>/dev/null || echo 0) + 1))
	echo $n >"$d/calls"
	[ "$*" = 'info :100' ] && [ $n -ge "$(cat "$d/answer-at")" ] || exit 1
	cat "$d/info"
	;;
start)
	echo "$*" >"$d/start-args"
	trap 'exit 1' TERM
	while :; do sleep 0.05; done
	;;
esac
`

type fakeDesktop struct {
	*Desktop
	dir string
}

func newFakeDesktop(t *testing.T, answerAt int, info string) *fakeDesktop {
	t.Helper()
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("sh not installed")
	}
	dir := t.TempDir()
	root := Root{filepath.Join(dir, "root")}
	if err := os.MkdirAll(root.join("usr/bin"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(root.join("usr/bin/xpra"), []byte(fakeXpra), 0o755); err != nil {
		t.Fatal(err)
	}
	spawn := func(cmd *exec.Cmd) error {
		cmd.Path = root.join(cmd.Path)
		return cmd.Start()
	}
	d := NewDesktop(root, spawn, func() []string { return append(os.Environ(), "FAKE_DIR="+dir) })
	d.IdleEvery = time.Hour
	d.PollEvery = 5 * time.Millisecond
	f := &fakeDesktop{Desktop: d, dir: dir}
	f.set(t, answerAt, info)
	t.Cleanup(d.Stop)
	return f
}

func (f *fakeDesktop) set(t *testing.T, answerAt int, info string) {
	t.Helper()
	os.Remove(filepath.Join(f.dir, "calls"))
	if err := os.WriteFile(filepath.Join(f.dir, "answer-at"), []byte(strconv.Itoa(answerAt)+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(f.dir, "info"), []byte(info), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestDesktopStartWaitsForSocket(t *testing.T) {
	d := newFakeDesktop(t, 3, "clients=0\n")
	if err := d.Start(); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(filepath.Join(d.dir, "calls")); strings.TrimSpace(string(b)) != "3" {
		t.Fatalf("xpra info calls = %q", b)
	}
	args, _ := os.ReadFile(filepath.Join(d.dir, "start-args"))
	want := "start :100 --daemon=no --systemd-run=no --bind=noabstract --socket-dir=/run/user/1000/xpra --mdns=no --html=off --pulseaudio=no --printing=no --file-transfer=off --notifications=no --clipboard=yes " +
		"--xvfb=Xvfb +extension GLX +extension Composite -screen 0 5120x2880x24+32 -nolisten tcp -nolisten local -noreset -auth $XAUTHORITY -dpi 96\n"
	if string(args) != want {
		t.Fatalf("xpra args = %q", args)
	}
	if got := d.Status(); got != "active" {
		t.Fatalf("status = %q", got)
	}

	d.Stop()
	d.set(t, 1000000, "clients=0\n")
	d.StartTimeout = 300 * time.Millisecond
	start := time.Now()
	if err := d.Start(); err == nil || !strings.Contains(err.Error(), "did not answer") {
		t.Fatalf("start without an answer = %v", err)
	}
	if el := time.Since(start); el > 6*time.Second {
		t.Fatalf("start gave up after %v", el)
	}
}

func TestDesktopStopIsSuccess(t *testing.T) {
	d := newFakeDesktop(t, 1, "clients=0\n")
	if err := d.Start(); err != nil {
		t.Fatal(err)
	}
	out := captureStderr(t, d.Stop)
	if d.Status() != "inactive" || out != "" {
		t.Fatalf("status = %q after stop, log %q; xpra's exit 1 on SIGTERM is a normal stop", d.Status(), out)
	}
}

func TestDesktopIdleStop(t *testing.T) {
	const idle = "clients=0\nclients.unauthenticated=0\nstate.windows=0\n"
	d := newFakeDesktop(t, 1, idle)
	d.IdleAfter = 30 * time.Minute
	start := func() {
		t.Helper()
		if err := d.Start(); err != nil {
			t.Fatal(err)
		}
	}
	age := func(min int) {
		d.mu.Lock()
		d.idleSince = time.Now().Add(-time.Duration(min) * time.Minute)
		d.mu.Unlock()
	}
	start()

	d.idleCheck()
	if d.idleSince.IsZero() || d.Status() != "active" {
		t.Fatalf("first idle check: stamp %v, status %q", d.idleSince, d.Status())
	}

	age(31)
	d.idleCheck()
	if d.Status() != "inactive" {
		t.Fatal("idle 31 min did not stop the desktop")
	}

	start()
	d.IdleAfter = time.Minute
	age(2)
	d.idleCheck()
	if d.Status() != "inactive" {
		t.Fatal("idle 2 min with a 1 min limit did not stop the desktop")
	}

	d.IdleAfter = 30 * time.Minute
	start()
	d.set(t, 1, "clients=1\nclients.unauthenticated=0\nstate.windows=0\n")
	age(31)
	d.idleCheck()
	if !d.idleSince.IsZero() || d.Status() != "active" {
		t.Fatalf("client attached: stamp %v, status %q", d.idleSince, d.Status())
	}

	d.set(t, 1, "clients=0\nclients.unauthenticated=0\nstate.windows=2\n")
	age(31)
	d.idleCheck()
	if d.Status() != "active" {
		t.Fatal("window open stopped the desktop")
	}

	d.set(t, 1000000, idle)
	age(31)
	d.idleCheck()
	if d.Status() != "active" {
		t.Fatal("unanswered info stopped the desktop")
	}
}

func TestDesktopStartRemovesStaleSockets(t *testing.T) {
	d := newFakeDesktop(t, 1, "clients=0\n")
	run, home := d.Root.join("run/user/1000"), d.Root.join("home/dev")
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
	if err := d.Start(); err != nil {
		t.Fatal(err)
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
	if fi, err := os.Stat(run); err != nil || fi.Mode().Perm() != 0o700 {
		t.Errorf("run dir = %v, %v; want mode 0700", fi, err)
	}
}

func TestDesktopNoXpra(t *testing.T) {
	d := NewDesktop(Root{t.TempDir()}, func(*exec.Cmd) error { t.Error("spawned without xpra"); return nil }, func() []string { return nil })
	if err := d.Start(); err == nil || err.Error() != "this image has no desktop (xpra not installed)" {
		t.Fatalf("start = %v", err)
	}
}

func serveDesktop(t *testing.T, d *Desktop, allow bool) {
	t.Helper()
	// A unix socket path must fit sun_path, which t.TempDir often does not on macOS.
	dir, err := os.MkdirTemp("/tmp", "gi")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	d.Allow = func(net.Conn) bool { return allow }
	l, err := net.Listen("unix", filepath.Join(dir, "d.sock"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { l.Close() })
	go d.Serve(l)
	orig, origUID := desktopSock, currentUID
	desktopSock, currentUID = filepath.Join(dir, "d.sock"), func() int { return 1000 }
	t.Cleanup(func() { desktopSock, currentUID = orig, origUID })
}

func TestDesktopNoXpraOverSocket(t *testing.T) {
	d := NewDesktop(Root{}, nil, func() []string { return nil })
	serveDesktop(t, d, true)
	var code int
	out := captureStderr(t, func() { code = desktopMain([]string{"start"}) })
	if code != 1 || !strings.Contains(out, "this image has no desktop (xpra not installed)") {
		t.Fatalf("start = %d: %q", code, out)
	}
}

func TestDesktopCommandsOverSocket(t *testing.T) {
	d := newFakeDesktop(t, 1, "clients=0\n")
	serveDesktop(t, d.Desktop, true)
	var buf bytes.Buffer
	orig := stdout
	stdout = &buf
	t.Cleanup(func() { stdout = orig })

	if code := desktopMain([]string{"status"}); code != 3 || buf.String() != "inactive\n" {
		t.Fatalf("status = %d %q", code, buf.String())
	}
	if code := desktopMain([]string{"start"}); code != 0 {
		t.Fatalf("start = %d", code)
	}
	buf.Reset()
	if code := desktopMain([]string{"status"}); code != 0 || buf.String() != "active\n" {
		t.Fatalf("status = %d %q", code, buf.String())
	}
	if code := desktopMain([]string{"stop"}); code != 0 || d.Status() != "inactive" {
		t.Fatalf("stop = %d, status %q", code, d.Status())
	}
	if code := desktopMain([]string{"bogus"}); code != 2 {
		t.Fatalf("bogus = %d", code)
	}
}

func TestDesktopRejectsDisallowedPeer(t *testing.T) {
	d := NewDesktop(Root{}, nil, func() []string { return nil })
	serveDesktop(t, d, false)
	var code int
	captureStderr(t, func() { code = desktopMain([]string{"status"}) })
	if code != 1 {
		t.Fatalf("status = %d, want a failure when the peer is not allowed", code)
	}
}

func TestDesktopRefusesRoot(t *testing.T) {
	orig := currentUID
	currentUID = func() int { return 0 }
	t.Cleanup(func() { currentUID = orig })
	var code int
	out := captureStderr(t, func() { code = desktopMain([]string{"start"}) })
	if code != 1 || !strings.Contains(out, "dev") {
		t.Fatalf("start as root = %d: %q", code, out)
	}
}

func TestDesktopListenSocketMode(t *testing.T) {
	dir, err := os.MkdirTemp("/tmp", "gi")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	d := NewDesktop(Root{dir}, nil, nil)
	for range 2 {
		l, err := d.Listen()
		if err != nil {
			t.Fatal(err)
		}
		defer l.Close()
	}
	if fi, err := os.Stat(filepath.Join(dir, desktopSockRel)); err != nil || fi.Mode().Perm() != 0o666 {
		t.Fatalf("socket = %v, %v; want mode 0666", fi, err)
	}
	if fi, err := os.Stat(filepath.Dir(filepath.Join(dir, desktopSockRel))); err != nil || fi.Mode().Perm() != 0o755 {
		t.Fatalf("dir = %v, %v; want mode 0755", fi, err)
	}
}

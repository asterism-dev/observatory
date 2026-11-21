package guestinit

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"syscall"
	"time"
)

const (
	desktopSockRel = "run/observatory/desktop.sock"
	desktopRunDir  = "/run/user/1000"
	xpraBin        = "/usr/bin/xpra"
	xpraStopWait   = 10 * time.Second
	noXpraMessage  = "this image has no desktop (xpra not installed)"
)

// A custom --xvfb gets no -auth from xpra, so it is passed here; $XAUTHORITY is left for xpra to expand.
var xpraStartArgs = []string{"start", ":100", "--daemon=no", "--systemd-run=no", "--bind=noabstract", "--socket-dir=" + desktopRunDir + "/xpra",
	"--mdns=no", "--html=off", "--pulseaudio=no", "--printing=no", "--file-transfer=off", "--notifications=no", "--clipboard=yes",
	"--xvfb=Xvfb +extension GLX +extension Composite -screen 0 5120x2880x24+32 -nolisten tcp -nolisten local -noreset -auth $XAUTHORITY -dpi 96"}

// Desktop runs dev's xpra desktop on :100 inside the image and answers the vmm-desktop and agent-browser helpers over a unix socket.
type Desktop struct {
	Root  Root
	Spawn func(*exec.Cmd) error
	Env   func() []string
	// Allow decides per connection whether the peer may talk to the desktop.
	Allow func(net.Conn) bool

	Xpra         string
	IdleAfter    time.Duration
	IdleEvery    time.Duration
	StartTimeout time.Duration
	PollEvery    time.Duration

	mu        sync.Mutex
	cmd       *exec.Cmd
	done      chan struct{}
	idleSince time.Time
}

func NewDesktop(root Root, spawn func(*exec.Cmd) error, env func() []string) *Desktop {
	return &Desktop{Root: root, Spawn: spawn, Env: env, Xpra: xpraBin, IdleAfter: 30 * time.Minute, IdleEvery: time.Minute,
		StartTimeout: 5 * time.Second, PollEvery: 200 * time.Millisecond}
}

func (d *Desktop) running() bool {
	if d.done == nil {
		return false
	}
	select {
	case <-d.done:
		return false
	default:
		return true
	}
}

// Start launches xpra unless it runs and waits until it answers.
func (d *Desktop) Start() error {
	d.mu.Lock()
	defer d.mu.Unlock()
	// A stamp left by an earlier session would stop the new one at its first idle check.
	d.idleSince = time.Time{}
	if !d.running() {
		if !d.Root.executable(d.Xpra) {
			return errors.New(noXpraMessage)
		}
		if err := d.launch(); err != nil {
			return err
		}
	}
	deadline := time.Now().Add(d.StartTimeout)
	for {
		if _, err := d.info(); err == nil {
			return nil
		}
		if !d.running() {
			return errors.New("the desktop exited during start")
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("the desktop did not answer within %.0f s", d.StartTimeout.Seconds())
		}
		time.Sleep(d.PollEvery)
	}
}

func (d *Desktop) launch() error {
	run := d.Root.join(desktopRunDir)
	if err := os.MkdirAll(run, 0o700); err != nil {
		return err
	}
	// Without systemd nothing else creates /run/user, and dev must be able to traverse it.
	if err := os.Chmod(filepath.Dir(run), 0o755); err != nil {
		return err
	}
	if err := os.Chmod(run, 0o700); err != nil {
		return err
	}
	if err := chown(run, devID, devID); err != nil {
		return err
	}
	// xpra waits about 6 s for sockets a stopped server left behind to time out; only this server uses :100, so they are stale.
	for _, pattern := range []string{run + "/xpra/*-100", run + "/xpra/100/socket", d.Root.join(devHome) + "/.xpra/*-100"} {
		matches, _ := filepath.Glob(pattern)
		for _, m := range matches {
			os.Remove(m)
		}
	}
	cmd := d.command(context.Background(), xpraStartArgs...)
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	if err := d.Spawn(cmd); err != nil {
		return fmt.Errorf("start xpra: %w", err)
	}
	done := make(chan struct{})
	d.cmd, d.done = cmd, done
	go func() {
		err := cmd.Wait()
		// xpra exits 1 on SIGTERM, so exit status 1 is a normal stop.
		var ee *exec.ExitError
		if errors.As(err, &ee) && ee.ExitCode() > 1 {
			logf("init: desktop: %v", err)
		}
		close(done)
	}()
	go d.idleLoop(done)
	return nil
}

func (d *Desktop) command(ctx context.Context, args ...string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, d.Xpra, args...)
	cmd.Dir = "/"
	cmd.Env = d.Env()
	return cmd
}

func (d *Desktop) info() (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	cmd := d.command(ctx, "info", ":100")
	var out bytes.Buffer
	cmd.Stdout = &out
	if err := d.Spawn(cmd); err != nil {
		return "", err
	}
	if err := cmd.Wait(); err != nil {
		return "", err
	}
	return out.String(), nil
}

// Stop ends xpra if it runs.
func (d *Desktop) Stop() {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.stop()
}

func (d *Desktop) stop() {
	d.idleSince = time.Time{}
	if !d.running() {
		return
	}
	_ = d.cmd.Process.Signal(syscall.SIGTERM)
	select {
	case <-d.done:
	case <-time.After(xpraStopWait):
		_ = d.cmd.Process.Kill()
		<-d.done
	}
}

func (d *Desktop) Status() string {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.running() {
		return "active"
	}
	return "inactive"
}

func (d *Desktop) idleLoop(done <-chan struct{}) {
	tick := time.NewTicker(d.IdleEvery)
	defer tick.Stop()
	for {
		select {
		case <-done:
			return
		case <-tick.C:
			d.idleCheck()
		}
	}
}

// idleCheck stops the desktop once it has had no client and no window for IdleAfter; an unanswered info changes nothing.
func (d *Desktop) idleCheck() {
	info, err := d.info()
	if err != nil {
		return
	}
	lines := strings.Split(info, "\n")
	idle := slices.Contains(lines, "clients=0") && slices.Contains(lines, "state.windows=0")
	d.mu.Lock()
	defer d.mu.Unlock()
	switch {
	case !idle:
		d.idleSince = time.Time{}
	case d.idleSince.IsZero():
		d.idleSince = time.Now()
	case time.Since(d.idleSince) > d.IdleAfter:
		d.stop()
	}
}

// Listen binds the helpers' socket inside the image's /run; every user may connect, Serve checks who they are.
func (d *Desktop) Listen() (net.Listener, error) {
	path := d.Root.join(desktopSockRel)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}
	os.Remove(path)
	l, err := net.Listen("unix", path)
	if err != nil {
		return nil, err
	}
	if err := os.Chmod(path, 0o666); err != nil {
		l.Close()
		return nil, err
	}
	return l, nil
}

type desktopReply struct {
	OK    bool   `json:"ok"`
	Error string `json:"error,omitempty"`
	State string `json:"state,omitempty"`
}

func (d *Desktop) Serve(l net.Listener) error {
	for {
		c, err := l.Accept()
		if err != nil {
			return err
		}
		go d.serveConn(c)
	}
}

func (d *Desktop) serveConn(c net.Conn) {
	defer c.Close()
	if d.Allow == nil || !d.Allow(c) {
		return
	}
	_ = c.SetDeadline(time.Now().Add(30 * time.Second))
	line, err := bufio.NewReader(io.LimitReader(c, 4<<10)).ReadBytes('\n')
	if err != nil && err != io.EOF {
		return
	}
	var req struct {
		Op string `json:"op"`
	}
	reply := desktopReply{OK: true}
	switch {
	case json.Unmarshal(line, &req) != nil:
		reply = desktopReply{Error: "bad request"}
	case req.Op == "start":
		if err := d.Start(); err != nil {
			reply = desktopReply{Error: err.Error()}
		}
	case req.Op == "stop":
		d.Stop()
	case req.Op == "status":
		reply.State = d.Status()
	case req.Op == "idle-check":
		d.idleCheck()
	default:
		reply = desktopReply{Error: "unknown op"}
	}
	out, _ := json.Marshal(reply)
	_, _ = c.Write(append(out, '\n'))
}

// desktopRefused is the init's answer that it could not do what was asked, as opposed to not being reachable.
type desktopRefused string

func (e desktopRefused) Error() string { return string(e) }

var (
	desktopSock = "/" + desktopSockRel
	currentUID  = os.Getuid
)

func desktopCall(op string) (desktopReply, error) {
	conn, err := net.DialTimeout("unix", desktopSock, 3*time.Second)
	if err != nil {
		return desktopReply{}, err
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(30 * time.Second))
	req, _ := json.Marshal(map[string]string{"op": op})
	if _, err := conn.Write(append(req, '\n')); err != nil {
		return desktopReply{}, err
	}
	line, err := bufio.NewReader(io.LimitReader(conn, 16<<10)).ReadBytes('\n')
	if err != nil && err != io.EOF {
		return desktopReply{}, err
	}
	var reply desktopReply
	if err := json.Unmarshal(line, &reply); err != nil {
		return desktopReply{}, errors.New("unreadable answer from the init")
	}
	if !reply.OK {
		return reply, desktopRefused(reply.Error)
	}
	return reply, nil
}

func desktopMain(args []string) int {
	if len(args) != 1 || !slices.Contains([]string{"start", "stop", "status", "idle-check"}, args[0]) {
		fmt.Fprintln(os.Stderr, "usage: vmm-desktop start|stop|status|idle-check")
		return 2
	}
	if currentUID() == 0 {
		fmt.Fprintln(os.Stderr, "vmm-desktop: run as the dev user, not root")
		return 1
	}
	reply, err := desktopCall(args[0])
	if err != nil {
		fmt.Fprintln(os.Stderr, "vmm-desktop: "+err.Error())
		return 1
	}
	if args[0] == "status" {
		fmt.Fprintln(stdout, reply.State)
		if reply.State != "active" {
			return 3
		}
	}
	return 0
}

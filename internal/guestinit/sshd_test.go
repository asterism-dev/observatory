package guestinit

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	"golang.org/x/crypto/ssh"
)

type testSSH struct {
	addr   string
	client ssh.Signer
	root   Root

	mu      sync.Mutex
	spawned []*exec.Cmd
}

func newTestSigner(t *testing.T) ssh.Signer {
	t.Helper()
	_, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	s, err := ssh.NewSignerFromKey(key)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// startTestSSH serves with a Spawn that records the command, then runs it as the test user outside any chroot.
func startTestSSH(t *testing.T, shell string, keys func(Root, ssh.Signer) []ssh.PublicKey) *testSSH {
	t.Helper()
	ts := &testSSH{client: newTestSigner(t), root: Root{Path: t.TempDir()}}
	if keys == nil {
		keys = func(Root, ssh.Signer) []ssh.PublicKey { return []ssh.PublicKey{ts.client.PublicKey()} }
	}
	work := t.TempDir()
	srv := &SSHServer{
		Keys:     func() []ssh.PublicKey { return keys(ts.root, ts.client) },
		HostKeys: []ssh.Signer{newTestSigner(t)},
		Spawn: func(cmd *exec.Cmd) error {
			rec := *cmd
			rec.SysProcAttr = new(*cmd.SysProcAttr)
			if cmd.SysProcAttr.Credential != nil {
				rec.SysProcAttr.Credential = new(*cmd.SysProcAttr.Credential)
			}
			ts.mu.Lock()
			ts.spawned = append(ts.spawned, &rec)
			ts.mu.Unlock()
			cmd.SysProcAttr.Credential = nil
			cmd.Dir = work
			return cmd.Start()
		},
		Env: func() map[string]string {
			return map[string]string{"SEED_VAR": "from-seed", "PATH": platformBin + ":" + defaultPath}
		},
		Shell: func() string { return shell },
		Root:  ts.root,
	}
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { l.Close() })
	go srv.Serve(l)
	ts.addr = l.Addr().String()
	return ts
}

func (ts *testSSH) dial(user string, key ssh.Signer) (*ssh.Client, error) {
	return ssh.Dial("tcp", ts.addr, &ssh.ClientConfig{
		User:            user,
		Auth:            []ssh.AuthMethod{ssh.PublicKeys(key)},
		HostKeyCallback: ssh.InsecureIgnoreHostKey(),
	})
}

func (ts *testSSH) mustDial(t *testing.T) *ssh.Client {
	t.Helper()
	c, err := ts.dial("dev", ts.client)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	return c
}

// echoServer echoes every connection back and closes the write side at EOF.
func echoServer(t *testing.T) net.Listener {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { l.Close() })
	go serveEcho(l)
	return l
}

func serveEcho(l net.Listener) {
	for {
		c, err := l.Accept()
		if err != nil {
			return
		}
		go func() {
			defer c.Close()
			_, _ = io.Copy(c, c)
		}()
	}
}

func roundTrip(t *testing.T, c net.Conn) {
	t.Helper()
	defer c.Close()
	if _, err := c.Write([]byte("ping")); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 4)
	if _, err := io.ReadFull(c, buf); err != nil || string(buf) != "ping" {
		t.Fatalf("echo = %q, %v", buf, err)
	}
}

func TestSSHRejectsRoot(t *testing.T) {
	ts := startTestSSH(t, "/bin/sh", nil)
	if c, err := ts.dial("root", ts.client); err == nil {
		c.Close()
		t.Fatal("root logged in")
	}
}

func TestSSHRejectsUnknownKey(t *testing.T) {
	ts := startTestSSH(t, "/bin/sh", nil)
	if c, err := ts.dial("dev", newTestSigner(t)); err == nil {
		c.Close()
		t.Fatal("unknown key logged in")
	}
}

func TestSSHAcceptsAuthorizedKeysFile(t *testing.T) {
	seedKey := newTestSigner(t)
	ts := startTestSSH(t, "/bin/sh", func(r Root, _ ssh.Signer) []ssh.PublicKey {
		return r.AuthorizedKeys([]string{string(ssh.MarshalAuthorizedKey(seedKey.PublicKey()))})
	})
	dir := ts.root.join(devHome + "/.ssh")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	line := "# mine\nno-pty " + string(ssh.MarshalAuthorizedKey(ts.client.PublicKey()))
	if err := os.WriteFile(filepath.Join(dir, "authorized_keys"), []byte(line), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, key := range []ssh.Signer{ts.client, seedKey} {
		c, err := ts.dial("dev", key)
		if err != nil {
			t.Fatal(err)
		}
		c.Close()
	}
}

func TestSSHExecRunsAsDev(t *testing.T) {
	ts := startTestSSH(t, "/bin/sh", nil)
	if err := os.MkdirAll(ts.root.join("etc"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(ts.root.join("etc/group"), []byte("docker:x:999:dev\ndev:x:1000:\nother:x:5:root\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	sess, err := ts.mustDial(t).NewSession()
	if err != nil {
		t.Fatal(err)
	}
	if err := sess.Setenv("LANG", "C.UTF-8"); err != nil {
		t.Fatal(err)
	}
	if err := sess.Setenv("EVIL", "x"); err == nil {
		t.Fatal("env request for EVIL accepted")
	}
	out, err := sess.Output("echo $SEED_VAR $LANG")
	if err != nil {
		t.Fatal(err)
	}
	if string(out) != "from-seed C.UTF-8\n" {
		t.Fatalf("out = %q", out)
	}
	cmd := ts.spawned[0]
	if c := cmd.SysProcAttr.Credential; c == nil || c.Uid != 1000 || c.Gid != 1000 || !slices.Equal(c.Groups, []uint32{1000, 999}) {
		t.Fatalf("credential = %+v", cmd.SysProcAttr.Credential)
	}
	if cmd.Dir != "/home/dev" || cmd.Path != "/bin/sh" {
		t.Fatalf("dir %q path %q", cmd.Dir, cmd.Path)
	}
	for _, kv := range []string{"HOME=/home/dev", "USER=dev", "SEED_VAR=from-seed", "PATH=" + platformBin + ":" + defaultPath} {
		if !slices.Contains(cmd.Env, kv) {
			t.Errorf("env lacks %s: %v", kv, cmd.Env)
		}
	}
	if !slices.ContainsFunc(cmd.Env, func(kv string) bool { return strings.HasPrefix(kv, "SSH_CONNECTION=127.0.0.1 ") }) {
		t.Errorf("env lacks SSH_CONNECTION: %v", cmd.Env)
	}
}

func TestSSHExitStatus(t *testing.T) {
	ts := startTestSSH(t, "/bin/sh", nil)
	sess, err := ts.mustDial(t).NewSession()
	if err != nil {
		t.Fatal(err)
	}
	var ee *ssh.ExitError
	if err := sess.Run("exit 3"); !errors.As(err, &ee) || ee.ExitStatus() != 3 {
		t.Fatalf("err = %v", err)
	}
}

// Review Focus 3: an image without a shell still serves port forwarding on the same connection.
func TestSSHNoShellMessage(t *testing.T) {
	ts := startTestSSH(t, "", nil)
	c := ts.mustDial(t)
	sess, err := c.NewSession()
	if err != nil {
		t.Fatal(err)
	}
	var stderr bytes.Buffer
	sess.Stderr = &stderr
	var ee *ssh.ExitError
	if err := sess.Run("ls"); !errors.As(err, &ee) || ee.ExitStatus() != 127 {
		t.Fatalf("err = %v", err)
	}
	if !strings.Contains(stderr.String(), "this image has no shell; port forwarding still works") {
		t.Fatalf("stderr = %q", stderr.String())
	}
	if len(ts.spawned) != 0 {
		t.Fatal("spawned without a shell")
	}
	conn, err := c.Dial("tcp", echoServer(t).Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	roundTrip(t, conn)
}

func TestSSHLocalForward(t *testing.T) {
	ts := startTestSSH(t, "/bin/sh", nil)
	conn, err := ts.mustDial(t).Dial("tcp", echoServer(t).Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	roundTrip(t, conn)
}

func TestSSHRemoteForward(t *testing.T) {
	ts := startTestSSH(t, "/bin/sh", nil)
	c := ts.mustDial(t)
	l, err := c.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	go serveEcho(l)
	conn, err := net.Dial("tcp", l.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	roundTrip(t, conn)
	if _, err := c.Listen("tcp", "127.0.0.1:80"); err == nil {
		t.Fatal("privileged remote forward accepted")
	}
}

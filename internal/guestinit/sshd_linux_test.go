package guestinit

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/pem"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"

	"golang.org/x/crypto/ssh"
)

// writeClientKey writes a fresh private key for OpenSSH clients and returns its file and signer.
func writeClientKey(t *testing.T) (string, ssh.Signer) {
	t.Helper()
	_, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	block, err := ssh.MarshalPrivateKey(key, "")
	if err != nil {
		t.Fatal(err)
	}
	keyFile := filepath.Join(t.TempDir(), "id")
	if err := os.WriteFile(keyFile, pem.EncodeToMemory(block), 0o600); err != nil {
		t.Fatal(err)
	}
	signer, err := ssh.NewSignerFromKey(key)
	if err != nil {
		t.Fatal(err)
	}
	return keyFile, signer
}

func TestSSHOpenSSHClient(t *testing.T) {
	sshBin, err := exec.LookPath("ssh")
	if err != nil {
		t.Skip("no ssh client")
	}
	keyFile, signer := writeClientKey(t)
	ts := startTestSSH(t, "/bin/sh", func(Root, ssh.Signer) []ssh.PublicKey { return []ssh.PublicKey{signer.PublicKey()} })
	host, port, _ := net.SplitHostPort(ts.addr)
	sshCmd := func(args ...string) *exec.Cmd {
		base := []string{"-F", "/dev/null", "-i", keyFile, "-p", port, "-o", "BatchMode=yes", "-o", "IdentitiesOnly=yes",
			"-o", "StrictHostKeyChecking=no", "-o", "UserKnownHostsFile=/dev/null", "-o", "LogLevel=ERROR", "-l", "dev"}
		cmd := exec.Command(sshBin, append(base, args...)...)
		cmd.Env = append(os.Environ(), "TERM=xterm-test")
		return cmd
	}

	out, err := sshCmd(host, "echo exec-ok; exit 4").Output()
	if ee, ok := err.(*exec.ExitError); !ok || ee.ExitCode() != 4 || string(out) != "exec-ok\n" {
		t.Fatalf("exec: out %q err %v", out, err)
	}

	out, err = sshCmd("-tt", host, "tty; echo $TERM").Output()
	if err != nil || !strings.Contains(string(out), "/dev/pts/") || !strings.Contains(string(out), "xterm-test") {
		t.Fatalf("pty: out %q err %v", out, err)
	}

	echo := echoServer(t)
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	local := strconv.Itoa(l.Addr().(*net.TCPAddr).Port)
	l.Close()
	fwd := sshCmd("-N", "-o", "ExitOnForwardFailure=yes", "-L", "127.0.0.1:"+local+":"+echo.Addr().String(), host)
	if err := fwd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = fwd.Process.Kill(); _ = fwd.Wait() })
	var conn net.Conn
	waitFor(t, func() bool {
		conn, err = net.Dial("tcp", "127.0.0.1:"+local)
		return err == nil
	})
	roundTrip(t, conn)
}

func TestAuthorizedKeysSymlinkResolvesInRoot(t *testing.T) {
	root := Root{Path: t.TempDir()}
	key := newTestSigner(t).PublicKey()
	if err := os.MkdirAll(root.join("elsewhere"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(root.join("elsewhere/keys"), ssh.MarshalAuthorizedKey(key), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(root.join(devHome+"/.ssh"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("/elsewhere/keys", root.join(authorizedKeysFile)); err != nil {
		t.Fatal(err)
	}
	if keys := root.AuthorizedKeys(nil); len(keys) != 1 {
		t.Fatalf("got %d keys, want the one inside the root", len(keys))
	}
}

// TestSFTPHelperProcess is the sftp-server helper when re-executed by TestSSHOpenSSHSFTP.
func TestSFTPHelperProcess(t *testing.T) {
	if os.Getenv("GUESTINIT_SFTP_HELPER") != "1" {
		return
	}
	os.Exit(Main([]string{"observatory-init", "sftp-server"}))
}

// TestSSHOpenSSHSFTP runs the helper as dev only when root; as anyone else it runs as the test user.
func TestSSHOpenSSHSFTP(t *testing.T) {
	sftpBin, err := exec.LookPath("sftp")
	if err != nil {
		t.Skip("no sftp client")
	}
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	asRoot := os.Geteuid() == 0
	// Not t.TempDir: its parent is 0700, so dev could not reach it.
	work, err := os.MkdirTemp("", "sftp-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(work) })
	if asRoot {
		if err := os.Chown(work, devID, devID); err != nil {
			t.Fatal(err)
		}
	}
	keyFile, signer := writeClientKey(t)
	srv := &SSHServer{
		Keys:     func() []ssh.PublicKey { return []ssh.PublicKey{signer.PublicKey()} },
		HostKeys: []ssh.Signer{newTestSigner(t)},
		Spawn: func(cmd *exec.Cmd) error {
			cmd.Path, cmd.Args = exe, []string{exe, "-test.run=^TestSFTPHelperProcess$"}
			cmd.Env = append(cmd.Env, "GUESTINIT_SFTP_HELPER=1")
			cmd.Dir = work
			if !asRoot {
				cmd.SysProcAttr.Credential = nil
			}
			return cmd.Start()
		},
		Env:   func() map[string]string { return nil },
		Shell: func() string { return "" },
		Root:  Root{Path: t.TempDir()},
	}
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { l.Close() })
	go srv.Serve(l)
	_, port, _ := net.SplitHostPort(l.Addr().String())
	local := t.TempDir()
	sftpBatch := func(batch string) (string, error) {
		batchFile := filepath.Join(local, "batch")
		if err := os.WriteFile(batchFile, []byte(batch), 0o600); err != nil {
			t.Fatal(err)
		}
		out, err := exec.Command(sftpBin, "-F", "/dev/null", "-i", keyFile, "-P", port, "-o", "BatchMode=yes", "-o", "IdentitiesOnly=yes",
			"-o", "StrictHostKeyChecking=no", "-o", "UserKnownHostsFile=/dev/null", "-o", "LogLevel=ERROR",
			"-b", batchFile, "dev@127.0.0.1").CombinedOutput()
		return string(out), err
	}

	up := filepath.Join(local, "up.txt")
	if err := os.WriteFile(up, []byte("sftp-ok\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	down := filepath.Join(local, "down.txt")
	out, err := sftpBatch("put " + up + " up.txt\nls -l\nget up.txt " + down + "\n")
	if err != nil || !strings.Contains(out, "up.txt") {
		t.Fatalf("put/ls/get: %v\n%s", err, out)
	}
	if got, err := os.ReadFile(down); err != nil || string(got) != "sftp-ok\n" {
		t.Fatalf("got %q, %v", got, err)
	}
	if asRoot {
		if fi, err := os.Stat(filepath.Join(work, "up.txt")); err != nil || fi.Sys().(*syscall.Stat_t).Uid != devID {
			t.Fatalf("uploaded file not owned by dev: %v", err)
		}
	}
	if out, err := sftpBatch("rm up.txt\n"); err != nil {
		t.Fatalf("rm: %v\n%s", err, out)
	}
	if _, err := os.Stat(filepath.Join(work, "up.txt")); !os.IsNotExist(err) {
		t.Fatalf("up.txt still there: %v", err)
	}

	t.Run("root-owned file unreadable", func(t *testing.T) {
		if !asRoot {
			t.Skip("needs root to run the helper as dev")
		}
		if err := os.WriteFile(filepath.Join(work, "secret"), []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
		stolen := filepath.Join(local, "stolen")
		if out, err := sftpBatch("get secret " + stolen + "\n"); err == nil {
			t.Fatalf("get of a root-owned 0600 file succeeded:\n%s", out)
		}
		if _, err := os.Stat(stolen); err == nil {
			if data, _ := os.ReadFile(stolen); len(data) > 0 {
				t.Fatal("secret content leaked")
			}
		}
	})
}

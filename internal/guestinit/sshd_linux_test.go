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
	"testing"

	"golang.org/x/crypto/ssh"
)

func TestSSHOpenSSHClient(t *testing.T) {
	sshBin, err := exec.LookPath("ssh")
	if err != nil {
		t.Skip("no ssh client")
	}
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

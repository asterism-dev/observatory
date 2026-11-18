package guest

import (
	"bufio"
	"encoding/json"
	"errors"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const (
	dummyGitHub = "ghp_dummy0000000000000000"
	dummyLinear = "lin_api_dummy000000000000"
	notConn     = `{"ok":false,"error":"not-connected"}`
	unavail     = `{"ok":false,"error":"unavailable"}`
)

func okAnswer(tok string) string {
	b, _ := json.Marshal(map[string]any{"ok": true, "token": tok, "expiresAt": nil})
	return string(b)
}

func needPython(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 not installed")
	}
}

// tokenServer answers the token protocol from answers (kind → reply line); a missing kind is not connected, nil answers never reply.
func tokenServer(t *testing.T, answers map[string]string) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "gi")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	sock := filepath.Join(dir, "s")
	l, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { l.Close() })
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			go func() {
				if answers == nil {
					time.Sleep(10 * time.Second)
					c.Close()
					return
				}
				defer c.Close()
				line, _ := bufio.NewReader(c).ReadBytes('\n')
				var req struct{ Op, Kind string }
				json.Unmarshal(line, &req)
				a, ok := answers[req.Kind]
				if req.Op != "token" || !ok {
					a = notConn
				}
				c.Write([]byte(a + "\n"))
			}()
		}
	}()
	return sock
}

// guestBin puts vmm-token and git-credential-vmm shims for the test's script copy on PATH, talking to sock.
func guestBin(t *testing.T, sock string) []string {
	t.Helper()
	needPython(t)
	dir := t.TempDir()
	script := filepath.Join(dir, "vmm-integrations.py")
	if err := os.WriteFile(script, []byte(IntegrationsScript), 0o644); err != nil {
		t.Fatal(err)
	}
	for name, sub := range map[string]string{"vmm-token": "token", "git-credential-vmm": "credential"} {
		shim := "#!/bin/sh\nexec python3 -I " + script + " " + sub + " \"$@\"\n"
		if err := os.WriteFile(filepath.Join(dir, name), []byte(shim), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	return []string{"PATH=" + dir + ":" + os.Getenv("PATH"), "VMM_TOKEN_UNIX=" + sock}
}

// run returns stdout and the exit code of argv under env.
func run(t *testing.T, env []string, stdin string, argv ...string) (string, int) {
	t.Helper()
	// Through sh, so argv[0] is looked up on env's PATH.
	cmd := exec.Command("sh", append([]string{"-c", `exec "$@"`, "sh"}, argv...)...)
	cmd.Env = append(os.Environ(), env...)
	cmd.Stdin = strings.NewReader(stdin)
	cmd.Stderr = os.Stderr
	out, err := cmd.Output()
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		return string(out), ee.ExitCode()
	} else if err != nil {
		t.Fatal(err)
	}
	return string(out), 0
}

// installGuest runs vmm-boot.sh against a fresh VMM_ROOT and returns it.
func installGuest(t *testing.T) string {
	t.Helper()
	needPython(t)
	root, seed := t.TempDir(), t.TempDir()
	for name, s := range map[string]string{
		"vmm-boot.sh": BootScript, "vmm-agent.py": AgentScript, "vmm-integrations.py": IntegrationsScript,
		"vmm-env": `{}`, "vmm-authorized-keys": "",
	} {
		if err := os.WriteFile(filepath.Join(seed, name), []byte(s), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	cmd := exec.Command("sh", filepath.Join(seed, "vmm-boot.sh"), seed)
	cmd.Env = append(os.Environ(), "VMM_ROOT="+root)
	if out, err := cmd.CombinedOutput(); err != nil || len(out) > 0 {
		t.Fatalf("vmm-boot.sh: %v: %s", err, out)
	}
	return root
}

func TestVmmTokenExitCodes(t *testing.T) {
	sock := tokenServer(t, map[string]string{
		"github": okAnswer(dummyGitHub), "linear": unavail, "claude": okAnswer("ghp_a\nusername=evil"),
		"short": okAnswer("abc"), "spaced": okAnswer("  " + dummyLinear + "\n"),
	})
	env := guestBin(t, sock)
	for _, tc := range []struct {
		kind, out string
		code      int
	}{
		{"github", dummyGitHub + "\n", 0},
		{"none", "", 3},
		{"linear", "", 4},
		{"claude", "", 4},
		{"short", "", 4},
		{"spaced", dummyLinear + "\n", 0},
	} {
		if out, code := run(t, env, "", "vmm-token", tc.kind); out != tc.out || code != tc.code {
			t.Errorf("vmm-token %s = %q, %d; want %q, %d", tc.kind, out, code, tc.out, tc.code)
		}
	}
	noServer := append(env, "VMM_TOKEN_UNIX="+filepath.Join(t.TempDir(), "gone"))
	if out, code := run(t, noServer, "", "vmm-token", "github"); out != "" || code != 4 {
		t.Errorf("without server = %q, %d", out, code)
	}
}

func TestVmmTokenTimeout(t *testing.T) {
	env := guestBin(t, tokenServer(t, nil))
	start := time.Now()
	out, code := run(t, env, "", "vmm-token", "github")
	if out != "" || code != 4 || time.Since(start) > 4*time.Second {
		t.Fatalf("hung server = %q, %d after %v", out, code, time.Since(start))
	}
}

func TestGitCredentialVmm(t *testing.T) {
	env := guestBin(t, tokenServer(t, map[string]string{"github": okAnswer(dummyGitHub)}))
	gh := "protocol=https\nhost=github.com\n\n"
	for _, tc := range []struct{ args, in, out string }{
		{"get", gh, "username=x-access-token\npassword=" + dummyGitHub + "\n"},
		{"get", "protocol=https\nhost=github.com\npath=o/r.git\n", "username=x-access-token\npassword=" + dummyGitHub + "\n"},
		{"get", "protocol=https\nhost=gitlab.com\n\n", ""},
		{"get", "protocol=http\nhost=github.com\n\n", ""},
		{"store", gh + "username=x-access-token\npassword=x\n\n", ""},
		{"erase", gh, ""},
	} {
		if out, code := run(t, env, tc.in, "git-credential-vmm", tc.args); out != tc.out || code != 0 {
			t.Errorf("%s %q = %q, %d", tc.args, tc.in, out, code)
		}
	}
	env = guestBin(t, tokenServer(t, map[string]string{}))
	if out, code := run(t, env, gh, "git-credential-vmm", "get"); out != "" || code != 0 {
		t.Errorf("not connected = %q, %d", out, code)
	}
}

func gitEnv(t *testing.T, root string, sock string) []string {
	return append(guestBin(t, sock), "VMM_ROOT="+root, "GIT_CONFIG_SYSTEM="+filepath.Join(root, "etc/gitconfig"),
		"GIT_CONFIG_GLOBAL=/dev/null", "HOME="+t.TempDir(), "GIT_TERMINAL_PROMPT=0", "GIT_ASKPASS=", "SSH_ASKPASS=")
}

func TestGitCredentialWithGit(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	root := installGuest(t)
	env := gitEnv(t, root, tokenServer(t, map[string]string{"github": okAnswer(dummyGitHub)}))
	if _, code := run(t, env, "", "python3", "-I", filepath.Join(root, "usr/local/lib/vmm/vmm-integrations.py"), "sync"); code != 0 {
		t.Fatalf("sync exit %d", code)
	}
	out, code := run(t, env, "url=https://github.com/o/r.git\n\n", "git", "credential", "fill")
	// Never print what git answered: a misconfigured test host could hand out a real credential.
	if code != 0 || !strings.Contains(out, "\npassword="+dummyGitHub+"\n") || !strings.Contains(out, "username=x-access-token\n") {
		t.Fatalf("git credential fill did not answer from vmm (exit %d)", code)
	}
	for _, u := range []string{"git@github.com:o/r.git", "ssh://git@github.com/o/r.git"} {
		if out, _ := run(t, env, "", "git", "ls-remote", "--get-url", u); out != "https://github.com/o/r.git\n" {
			t.Errorf("get-url %s = %q", u, out)
		}
	}
}

func TestSyncGitInclude(t *testing.T) {
	needPython(t)
	root := t.TempDir()
	answers := map[string]string{"github": okAnswer(dummyGitHub)}
	env := append(guestBin(t, tokenServer(t, answers)), "VMM_ROOT="+root)
	script := filepath.Join(t.TempDir(), "vmm-integrations.py")
	os.WriteFile(script, []byte(IntegrationsScript), 0o644)
	gitconfig := filepath.Join(root, "etc/gitconfig")
	os.MkdirAll(filepath.Dir(gitconfig), 0o755)
	user := "[user]\n\tname = x\n"
	os.WriteFile(gitconfig, []byte(user), 0o644)
	include := "[include]\n\tpath = " + root + "/etc/vmm/gitconfig\n"
	sync := func() string {
		t.Helper()
		if _, code := run(t, env, "", "python3", "-I", script, "sync"); code != 0 {
			t.Fatalf("sync exit %d", code)
		}
		b, _ := os.ReadFile(gitconfig)
		return string(b)
	}
	if got := sync(); got != user+include {
		t.Fatalf("connected: %q", got)
	}
	if got := sync(); got != user+include {
		t.Fatalf("second sync: %q", got)
	}
	answers["github"] = unavail
	if got := sync(); got != user+include {
		t.Fatalf("unavailable: %q", got)
	}
	delete(answers, "github")
	if got := sync(); got != user {
		t.Fatalf("not connected: %q", got)
	}
	os.Remove(gitconfig)
	answers["github"] = okAnswer(dummyGitHub)
	if got := sync(); got != include {
		t.Fatalf("no gitconfig before: %q", got)
	}
}

func TestGhWrapperRespectsUserLogin(t *testing.T) {
	root := installGuest(t)
	bin := t.TempDir()
	fake := filepath.Join(bin, "gh")
	os.WriteFile(fake, []byte("#!/bin/sh\nprintf %s \"$GH_TOKEN\"\nexit 7\n"), 0o755)
	home := t.TempDir()
	base := append(guestBin(t, tokenServer(t, map[string]string{"github": okAnswer(dummyGitHub)})),
		"VMM_GH_BIN="+fake, "HOME="+home, "GH_TOKEN=", "GITHUB_TOKEN=", "GH_CONFIG_DIR=")
	wrapper := filepath.Join(root, "usr/local/bin/gh")
	for _, tc := range []struct {
		name string
		env  []string
		want string
	}{
		{"plain", nil, dummyGitHub},
		{"own GH_TOKEN", []string{"GH_TOKEN=mine"}, "mine"},
		{"GITHUB_TOKEN", []string{"GITHUB_TOKEN=theirs"}, ""},
	} {
		if out, code := run(t, append(base, tc.env...), "", wrapper, "api", "user"); out != tc.want || code != 7 {
			t.Errorf("%s: %q, %d", tc.name, out, code)
		}
	}
	os.MkdirAll(filepath.Join(home, ".config/gh"), 0o755)
	os.WriteFile(filepath.Join(home, ".config/gh/hosts.yml"), []byte("github.com:\n    user: me\n"), 0o600)
	if out, _ := run(t, base, "", wrapper); out != "" {
		t.Errorf("own hosts.yml: %q", out)
	}
	cfg := t.TempDir()
	if out, _ := run(t, append(base, "GH_CONFIG_DIR="+cfg), "", wrapper); out != dummyGitHub {
		t.Errorf("GH_CONFIG_DIR without login: %q", out)
	}
	env := append(guestBin(t, tokenServer(t, map[string]string{})), "VMM_GH_BIN="+fake, "HOME="+t.TempDir(), "GH_TOKEN=", "GITHUB_TOKEN=", "GH_CONFIG_DIR=")
	if out, code := run(t, env, "", wrapper); out != "" || code != 7 {
		t.Errorf("not connected: %q, %d", out, code)
	}
}

func TestLinearProfile(t *testing.T) {
	root := installGuest(t)
	env := guestBin(t, tokenServer(t, map[string]string{"linear": okAnswer(dummyLinear)}))
	profile := filepath.Join(root, "etc/profile.d/vmm-linear.sh")
	script := `unset LINEAR_API_KEY; . "$1"; echo "$LINEAR_API_KEY"`
	if out, _ := run(t, env, "", "sh", "-c", script, "sh", profile); out != dummyLinear+"\n" {
		t.Errorf("unset: %q", out)
	}
	if out, _ := run(t, env, "", "sh", "-c", `LINEAR_API_KEY=mine; . "$1"; echo "$LINEAR_API_KEY"`, "sh", profile); out != "mine\n" {
		t.Errorf("own key: %q", out)
	}
	env = guestBin(t, tokenServer(t, map[string]string{}))
	if out, _ := run(t, env, "", "sh", "-c", `unset LINEAR_API_KEY; . "$1"; echo "${LINEAR_API_KEY-unset}"`, "sh", profile); out != "unset\n" {
		t.Errorf("not connected: %q", out)
	}
}

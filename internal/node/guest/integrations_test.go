package guest

import (
	"bufio"
	"encoding/json"
	"errors"
	"net"
	"os"
	"os/exec"
	"os/user"
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
		"vmm-AGENTS.md": ContextFile, "vmm-desktop": DesktopScript, "vmm-env": `{}`, "vmm-authorized-keys": "",
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

const (
	dummyClaude  = "sk-ant-oat01-dummy000000000"
	dummyClaude2 = "sk-ant-oat01-dummy111111111"
	dummyLinear2 = "lin_api_dummy111111111111"
)

// userGuest is a VMM_ROOT whose guest user is the test's own user, so the user part runs without root.
type userGuest struct {
	t                           *testing.T
	env                         []string
	script, home, state, claude string
	log, flag, root             string
}

func newUserGuest(t *testing.T, answers map[string]string) *userGuest {
	t.Helper()
	needPython(t)
	u, err := user.Current()
	if err != nil {
		t.Skip("no current user")
	}
	root := t.TempDir()
	g := &userGuest{t: t, root: root, home: filepath.Join(root, "home", u.Username), state: filepath.Join(root, "var/lib/vmm", u.Username, "integrations.json")}
	g.script = filepath.Join(t.TempDir(), "vmm-integrations.py")
	os.WriteFile(g.script, []byte(IntegrationsScript), 0o644)
	os.MkdirAll(g.home, 0o755)
	aux := t.TempDir()
	g.log, g.flag = filepath.Join(aux, "argv"), filepath.Join(aux, "own-entry")
	g.claude = filepath.Join(g.home, ".local/bin/claude")
	g.env = append(guestBin(t, tokenServer(t, answers)), "VMM_ROOT="+root, "VMM_USER="+u.Username)
	// Keep the host's own claude out of reach: the sync must only ever run the fake.
	py, _ := exec.LookPath("python3")
	os.Symlink(py, filepath.Join(aux, "python3"))
	path := strings.Split(g.env[0], ":")[0] + ":" + aux + ":/usr/bin:/bin"
	if _, err := exec.Command("sh", "-c", `PATH="$1" command -v claude`, "sh", strings.TrimPrefix(path, "PATH=")).Output(); err == nil {
		t.Skip("a claude binary in /usr/bin or /bin")
	}
	g.env[0] = path
	return g
}

// addClaude installs a fake claude that logs its argv, echoes it to both streams and answers `mcp get` from the flag file.
func (g *userGuest) addClaude() {
	os.MkdirAll(filepath.Dir(g.claude), 0o755)
	fake := "#!/bin/sh\nprintf '%s\\n' \"$*\" >>" + g.log + "\necho \"$*\"; echo \"$*\" >&2\n" +
		"if [ \"$1 $2\" = \"mcp get\" ]; then test -e " + g.flag + "; exit; fi\n" +
		"if [ \"$1 $2\" = \"mcp remove\" ] && [ -e " + g.flag + ".rmfail ]; then exit 1; fi\n"
	if err := os.WriteFile(g.claude, []byte(fake), 0o755); err != nil {
		g.t.Fatal(err)
	}
}

// sync runs `sync`, fails on a non-zero exit or any token in its output, and returns the claude calls made.
func (g *userGuest) sync() []string {
	g.t.Helper()
	cmd := exec.Command("python3", "-I", g.script, "sync")
	cmd.Env = append(os.Environ(), g.env...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		g.t.Fatalf("sync: %v", err)
	}
	for _, tok := range []string{dummyClaude, dummyClaude2, dummyLinear, dummyLinear2, dummyGitHub} {
		if strings.Contains(string(out), tok) {
			g.t.Fatalf("sync output contains a token")
		}
	}
	b, _ := os.ReadFile(g.log)
	os.Remove(g.log)
	if len(b) == 0 {
		return nil
	}
	return strings.Split(strings.TrimSuffix(string(b), "\n"), "\n")
}

func (g *userGuest) creds() string { return filepath.Join(g.home, ".claude/.credentials.json") }

func (g *userGuest) read(path string) string {
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return "<missing>"
	}
	return string(b)
}

func credsFile(tok string) string {
	return `{"claudeAiOauth":{"accessToken":"` + tok + `","refreshToken":null,"expiresAt":null,"scopes":["user:inference"]}}`
}

func TestClaudeCredentialsWritten(t *testing.T) {
	answers := map[string]string{"claude": okAnswer(dummyClaude)}
	g := newUserGuest(t, answers)
	g.sync()
	if got := g.read(g.creds()); got != credsFile(dummyClaude) {
		t.Fatalf("credentials = %q", got)
	}
	if fi, _ := os.Stat(g.creds()); fi.Mode().Perm() != 0o600 {
		t.Errorf("file mode %v", fi.Mode().Perm())
	}
	if fi, _ := os.Stat(filepath.Dir(g.creds())); fi.Mode().Perm() != 0o700 {
		t.Errorf("dir mode %v", fi.Mode().Perm())
	}
	answers["claude"] = okAnswer(dummyClaude2)
	g.sync()
	if got := g.read(g.creds()); got != credsFile(dummyClaude2) {
		t.Fatalf("after token change = %q", got)
	}
	delete(answers, "claude")
	g.sync()
	if got := g.read(g.creds()); got != "<missing>" {
		t.Fatalf("not connected = %q", got)
	}
}

func TestClaudeUserFileLeftAlone(t *testing.T) {
	answers := map[string]string{"claude": okAnswer(dummyClaude)}
	g := newUserGuest(t, answers)
	own := credsFile("sk-ant-oat01-users-own-login") + "\n"
	os.MkdirAll(filepath.Dir(g.creds()), 0o700)
	os.WriteFile(g.creds(), []byte(own), 0o600)
	g.sync()
	if got := g.read(g.creds()); got != own {
		t.Fatalf("after ok = %q", got)
	}
	delete(answers, "claude")
	g.sync()
	if got := g.read(g.creds()); got != own {
		t.Fatalf("after not-connected = %q", got)
	}
}

func TestClaudeFileOursByToken(t *testing.T) {
	answers := map[string]string{"claude": okAnswer(dummyClaude)}
	g := newUserGuest(t, answers)
	g.sync()
	pretty := "{\n  \"claudeAiOauth\": {\n    \"accessToken\": \"" + dummyClaude + "\",\n    \"refreshToken\": null,\n    \"expiresAt\": null,\n    \"scopes\": [\"user:inference\"],\n    \"subscriptionType\": \"max\"\n  }\n}\n"
	os.WriteFile(g.creds(), []byte(pretty), 0o600)
	g.sync()
	if got := g.read(g.creds()); got != pretty {
		t.Fatalf("same token rewritten: %q", got)
	}
	answers["claude"] = okAnswer(dummyClaude2)
	g.sync()
	if got := g.read(g.creds()); got != credsFile(dummyClaude2) {
		t.Fatalf("re-serialised file treated as the user's: %q", got)
	}
}

func TestClaudeUnavailableKeepsFile(t *testing.T) {
	answers := map[string]string{"claude": okAnswer(dummyClaude)}
	g := newUserGuest(t, answers)
	g.sync()
	state := g.read(g.state)
	answers["claude"] = unavail
	g.sync()
	if got := g.read(g.creds()); got != credsFile(dummyClaude) {
		t.Fatalf("credentials = %q", got)
	}
	if got := g.read(g.state); got != state || state == "<missing>" {
		t.Fatalf("state %q → %q", state, got)
	}
}

func TestLinearMCPAddRemove(t *testing.T) {
	answers := map[string]string{"linear": okAnswer(dummyLinear)}
	g := newUserGuest(t, answers)
	g.addClaude()
	add := func(tok string) string {
		return "mcp add --transport http --scope user linear https://mcp.linear.app/mcp --header Authorization: Bearer " + tok
	}
	remove := "mcp remove --scope user linear"
	get := "mcp get linear"
	check := func(step string, want ...string) {
		t.Helper()
		if got := g.sync(); strings.Join(got, "|") != strings.Join(want, "|") {
			t.Fatalf("%s: calls %q, want %q", step, got, want)
		}
	}
	check("ok", get, add(dummyLinear))
	check("same token")
	answers["linear"] = okAnswer(dummyLinear2)
	check("new token", remove, add(dummyLinear2))
	answers["linear"] = unavail
	check("unavailable")
	delete(answers, "linear")
	check("not connected", remove)
	check("not connected again")
	os.WriteFile(g.flag, nil, 0o644)
	answers["linear"] = okAnswer(dummyLinear)
	check("user's own entry", get)
	delete(answers, "linear")
	check("user's own entry, not connected")
}

func TestLinearMCPRemoveFailureKeepsRecord(t *testing.T) {
	answers := map[string]string{"linear": okAnswer(dummyLinear)}
	g := newUserGuest(t, answers)
	g.addClaude()
	remove := "mcp remove --scope user linear"
	g.sync()
	os.WriteFile(g.flag+".rmfail", nil, 0o644)
	answers["linear"] = okAnswer(dummyLinear2)
	if got := g.sync(); strings.Join(got, "|") != remove {
		t.Fatalf("new token, remove fails: calls %q", got)
	}
	delete(answers, "linear")
	for i := 0; i < 2; i++ {
		if got := g.sync(); strings.Join(got, "|") != remove {
			t.Fatalf("not connected, remove fails (%d): calls %q", i, got)
		}
	}
	os.Remove(g.flag + ".rmfail")
	if got := g.sync(); strings.Join(got, "|") != remove {
		t.Fatalf("not connected, remove works: calls %q", got)
	}
	if got := g.sync(); got != nil {
		t.Fatalf("after removal: calls %q", got)
	}
}

func TestSyncWithoutClaude(t *testing.T) {
	g := newUserGuest(t, map[string]string{"claude": okAnswer(dummyClaude), "linear": okAnswer(dummyLinear)})
	if calls := g.sync(); calls != nil {
		t.Fatalf("calls without claude: %q", calls)
	}
	if got := g.read(g.creds()); got != credsFile(dummyClaude) {
		t.Fatalf("credentials = %q", got)
	}
	g.addClaude()
	if calls := g.sync(); len(calls) != 2 || !strings.HasPrefix(calls[1], "mcp add ") {
		t.Fatalf("calls once claude exists: %q", calls)
	}
}

func TestSyncNeverPrintsToken(t *testing.T) {
	answers := map[string]string{"github": okAnswer(dummyGitHub), "claude": okAnswer(dummyClaude), "linear": okAnswer(dummyLinear)}
	g := newUserGuest(t, answers)
	g.addClaude()
	// The fake claude echoes its argv, token included, on both streams; sync() fails on any token in the output.
	g.sync()
	answers["claude"], answers["linear"] = okAnswer(dummyClaude2), okAnswer(dummyLinear2)
	g.sync()
	os.WriteFile(g.claude, []byte("#!/bin/sh\necho \"$*\" >&2\nexit 1\n"), 0o755)
	answers["linear"] = okAnswer(dummyLinear)
	g.sync()
	delete(answers, "claude")
	delete(answers, "linear")
	g.sync()
}

func TestSyncWriteErrorsExitZero(t *testing.T) {
	answers := map[string]string{"github": okAnswer(dummyGitHub), "claude": okAnswer(dummyClaude)}
	g := newUserGuest(t, answers)
	root := strings.TrimPrefix(g.env[len(g.env)-2], "VMM_ROOT=")
	// A file where a directory has to be: every write fails.
	os.WriteFile(filepath.Join(root, "etc"), nil, 0o644)
	os.WriteFile(filepath.Join(g.home, ".claude"), nil, 0o644)
	g.sync()
	os.Remove(filepath.Join(g.home, ".claude"))
	g.sync()
	if got := g.read(g.creds()); got != credsFile(dummyClaude) {
		t.Fatalf("credentials after the error = %q", got)
	}
}

// Over vsock: on a host without AF_VSOCK (macOS) or without a listener this must be a clean "unavailable".
func TestVmmTokenVsockUnavailable(t *testing.T) {
	needPython(t)
	cmd := exec.Command("python3", "-I", "-c", IntegrationsScript, "token", "github")
	cmd.Env = append(os.Environ(), "VMM_TOKEN_UNIX=")
	out, err := cmd.CombinedOutput()
	var ee *exec.ExitError
	if !errors.As(err, &ee) || ee.ExitCode() != 4 || len(out) > 0 {
		t.Fatalf("vsock = %v: %q", err, out)
	}
}

func TestLinearProfileWithoutVmmToken(t *testing.T) {
	profile := filepath.Join(installGuest(t), "etc/profile.d/vmm-linear.sh")
	cmd := exec.Command("/bin/sh", "-c", `unset LINEAR_API_KEY; . "$1"; echo "${LINEAR_API_KEY-unset}"`, "sh", profile)
	cmd.Env = []string{"PATH=" + t.TempDir()}
	if out, err := cmd.CombinedOutput(); err != nil || string(out) != "unset\n" {
		t.Fatalf("login without vmm-token: %v %q", err, out)
	}
}

func (g *userGuest) setContext(text string) {
	src := filepath.Join(g.root, "usr/local/share/vmm/AGENTS.md")
	os.MkdirAll(filepath.Dir(src), 0o755)
	if err := os.WriteFile(src, []byte(text), 0o644); err != nil {
		g.t.Fatal(err)
	}
}

func (g *userGuest) codexAgents() string { return filepath.Join(g.home, ".codex/AGENTS.md") }

func TestCodexAgentsWritten(t *testing.T) {
	g := newUserGuest(t, map[string]string{})
	g.sync()
	if got := g.read(g.codexAgents()); got != "<missing>" {
		t.Fatalf("without a source = %q", got)
	}
	g.setContext("platform v1\n")
	g.sync()
	if got := g.read(g.codexAgents()); got != "platform v1\n" {
		t.Fatalf("written = %q", got)
	}
	if fi, _ := os.Stat(g.codexAgents()); fi.Mode().Perm() != 0o644 {
		t.Errorf("file mode %v", fi.Mode().Perm())
	}
	if fi, _ := os.Stat(filepath.Dir(g.codexAgents())); fi.Mode().Perm() != 0o700 {
		t.Errorf("dir mode %v", fi.Mode().Perm())
	}
	if !strings.Contains(g.read(g.state), `"codex_agents"`) {
		t.Fatalf("state = %q", g.read(g.state))
	}
	g.setContext("platform v2\n")
	g.sync()
	if got := g.read(g.codexAgents()); got != "platform v2\n" {
		t.Fatalf("after a source change = %q", got)
	}
}

func TestCodexAgentsUserFileLeftAlone(t *testing.T) {
	g := newUserGuest(t, map[string]string{})
	g.setContext("platform v1\n")
	os.MkdirAll(filepath.Dir(g.codexAgents()), 0o755)
	os.WriteFile(g.codexAgents(), []byte("my own rules\n"), 0o600)
	g.sync()
	g.sync()
	if got := g.read(g.codexAgents()); got != "my own rules\n" {
		t.Fatalf("own file = %q", got)
	}

	g = newUserGuest(t, map[string]string{})
	g.setContext("platform v1\n")
	g.sync()
	if err := os.WriteFile(g.codexAgents(), []byte("platform v1\nmy addition\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	g.setContext("platform v2\n")
	g.sync()
	g.sync()
	if got := g.read(g.codexAgents()); got != "platform v1\nmy addition\n" {
		t.Fatalf("edited file = %q", got)
	}
}

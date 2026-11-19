package guestinit

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

const (
	dummyGitHub  = "ghp_dummy0000000000000000"
	dummyLinear  = "lin_api_dummy000000000000"
	dummyClaude  = "sk-ant-oat01-dummy000000000"
	dummyClaude2 = "sk-ant-oat01-dummy111111111"
	dummyLinear2 = "lin_api_dummy111111111111"
	notConn      = `{"ok":false,"error":"not-connected"}`
	unavail      = `{"ok":false,"error":"unavailable"}`
)

func okAnswer(tok string) string {
	b, _ := json.Marshal(map[string]any{"ok": true, "token": tok, "expiresAt": nil})
	return string(b)
}

func TestHelperProcess(t *testing.T) {
	if os.Getenv("GUESTINIT_HELPER") != "1" {
		return
	}
	args := os.Args
	for i, a := range args {
		if a == "--" {
			os.Exit(Main(args[i+1:]))
		}
	}
	os.Exit(2)
}

// tokenAnswers maps kind to reply line; tests change it while the server goroutine reads it.
type tokenAnswers struct {
	mu sync.Mutex
	m  map[string]string
}

func newAnswers(m map[string]string) *tokenAnswers { return &tokenAnswers{m: m} }

func (a *tokenAnswers) set(kind, reply string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.m[kind] = reply
}

func (a *tokenAnswers) del(kind string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	delete(a.m, kind)
}

func (a *tokenAnswers) get(kind string) (string, bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	r, ok := a.m[kind]
	return r, ok
}

// tokenServer answers the token protocol from answers; a missing kind is not connected, nil answers never reply.
func tokenServer(t *testing.T, answers *tokenAnswers) string {
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
				a, ok := answers.get(req.Kind)
				if req.Op != "token" || !ok {
					a = notConn
				}
				c.Write([]byte(a + "\n"))
			}()
		}
	}()
	return sock
}

// serve points Token at a fresh server for the test.
func serve(t *testing.T, answers *tokenAnswers) {
	t.Helper()
	t.Setenv("VMM_TOKEN_UNIX", tokenServer(t, answers))
}

// guestBin puts vmm-token and git-credential-vmm shims, which run this test binary as the tool, on PATH, talking to sock.
func guestBin(t *testing.T, sock string) []string {
	t.Helper()
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	for _, name := range []string{"vmm-token", "git-credential-vmm"} {
		shim := "#!/bin/sh\nGUESTINIT_HELPER=1 exec " + self + " -test.run='^TestHelperProcess$' -- " + name + " \"$@\"\n"
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

// runTool runs the multi-call tool name as a separate process.
func runTool(t *testing.T, env []string, name string, args ...string) (string, int) {
	t.Helper()
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	return run(t, append(env, "GUESTINIT_HELPER=1"), "", append([]string{self, "-test.run=^TestHelperProcess$", "--", name}, args...)...)
}

func TestIntegrationsTokenExitCodes(t *testing.T) {
	serve(t, newAnswers(map[string]string{
		"github": okAnswer(dummyGitHub), "linear": unavail, "claude": okAnswer("ghp_a\nusername=evil"),
		"short": okAnswer("abc"), "spaced": okAnswer("  " + dummyLinear + "\n"),
	}))
	for _, tc := range []struct {
		kind, tok string
		code      int
	}{
		{"github", dummyGitHub, 0},
		{"none", "", 3},
		{"linear", "", 4},
		{"claude", "", 4},
		{"short", "", 4},
		{"spaced", dummyLinear, 0},
	} {
		if tok, code := Token(tc.kind); tok != tc.tok || code != tc.code {
			t.Errorf("Token(%s) = %q, %d; want %q, %d", tc.kind, tok, code, tc.tok, tc.code)
		}
	}
	t.Setenv("VMM_TOKEN_UNIX", filepath.Join(t.TempDir(), "gone"))
	if tok, code := Token("github"); tok != "" || code != 4 {
		t.Errorf("without server = %q, %d", tok, code)
	}
}

func TestIntegrationsVmmTokenTool(t *testing.T) {
	env := guestBin(t, tokenServer(t, newAnswers(map[string]string{"github": okAnswer(dummyGitHub)})))
	if out, code := run(t, env, "", "vmm-token", "github"); out != dummyGitHub+"\n" || code != 0 {
		t.Errorf("vmm-token github = %q, %d", out, code)
	}
	if out, code := run(t, env, "", "vmm-token", "none"); out != "" || code != 3 {
		t.Errorf("vmm-token none = %q, %d", out, code)
	}
}

func TestIntegrationsTokenTimeout(t *testing.T) {
	serve(t, nil)
	start := time.Now()
	tok, code := Token("github")
	if tok != "" || code != 4 || time.Since(start) > 4*time.Second {
		t.Fatalf("hung server = %q, %d after %v", tok, code, time.Since(start))
	}
}

// On a host without AF_VSOCK or without a listener this must be a clean "unavailable".
func TestIntegrationsTokenVsockUnavailable(t *testing.T) {
	t.Setenv("VMM_TOKEN_UNIX", "")
	if tok, code := Token("github"); tok != "" || code != 4 {
		t.Fatalf("vsock = %q, %d", tok, code)
	}
}

func TestIntegrationsGitCredential(t *testing.T) {
	env := guestBin(t, tokenServer(t, newAnswers(map[string]string{"github": okAnswer(dummyGitHub)})))
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
	env = guestBin(t, tokenServer(t, newAnswers(map[string]string{})))
	if out, code := run(t, env, gh, "git-credential-vmm", "get"); out != "" || code != 0 {
		t.Errorf("not connected = %q, %d", out, code)
	}
}

func TestIntegrationsGitCredentialWithGit(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	root := Root{t.TempDir()}
	env := guestBin(t, tokenServer(t, newAnswers(map[string]string{"github": okAnswer(dummyGitHub)})))
	t.Setenv("VMM_TOKEN_UNIX", strings.TrimPrefix(env[1], "VMM_TOKEN_UNIX="))
	if err := root.SyncIntegrations(context.Background(), nil, nil); err != nil {
		t.Fatal(err)
	}
	// The include is an image path; git here runs on the host, so it reads the included file directly.
	cfg, err := os.ReadFile(root.join(gitconfigRel))
	if err != nil {
		t.Fatal(err)
	}
	cfgPath := filepath.Join(t.TempDir(), "gitconfig")
	os.WriteFile(cfgPath, cfg, 0o644)
	env = append(env, "GIT_CONFIG_SYSTEM="+cfgPath, "GIT_CONFIG_GLOBAL=/dev/null", "HOME="+t.TempDir(), "GIT_TERMINAL_PROMPT=0", "GIT_ASKPASS=", "SSH_ASKPASS=")
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

func TestIntegrationsSyncGitInclude(t *testing.T) {
	root := Root{t.TempDir()}
	answers := newAnswers(map[string]string{"github": okAnswer(dummyGitHub)})
	serve(t, answers)
	gitconfig := root.join("etc/gitconfig")
	user := "[user]\n\tname = x\n"
	if err := writeFileAtomic(gitconfig, []byte(user), 0o644); err != nil {
		t.Fatal(err)
	}
	include := "[include]\n\tpath = /etc/observatory/gitconfig\n"
	sync := func() string {
		t.Helper()
		if err := root.SyncIntegrations(context.Background(), nil, nil); err != nil {
			t.Fatalf("sync: %v", err)
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
	answers.set("github", unavail)
	if got := sync(); got != user+include {
		t.Fatalf("unavailable: %q", got)
	}
	answers.del("github")
	if got := sync(); got != user {
		t.Fatalf("not connected: %q", got)
	}
	os.Remove(gitconfig)
	answers.set("github", okAnswer(dummyGitHub))
	if got := sync(); got != include {
		t.Fatalf("no gitconfig before: %q", got)
	}
}

func ghEnv(t *testing.T, gh string, extra ...string) []string {
	t.Helper()
	return append([]string{"PATH=" + filepath.Dir(gh), "HOME=" + t.TempDir(), "GH_TOKEN=", "GITHUB_TOKEN=", "GH_CONFIG_DIR="}, extra...)
}

func TestIntegrationsGHWrapperRespectsUserLogin(t *testing.T) {
	bin := t.TempDir()
	fake := filepath.Join(bin, "gh")
	os.WriteFile(fake, []byte("#!/bin/sh\nprintf %s \"$GH_TOKEN\"\nexit 7\n"), 0o755)
	sock := tokenServer(t, newAnswers(map[string]string{"github": okAnswer(dummyGitHub)}))
	home := t.TempDir()
	base := append(ghEnv(t, fake, "VMM_TOKEN_UNIX="+sock), "HOME="+home)
	for _, tc := range []struct {
		name string
		env  []string
		want string
	}{
		{"plain", nil, dummyGitHub},
		{"own GH_TOKEN", []string{"GH_TOKEN=mine"}, "mine"},
		{"GITHUB_TOKEN", []string{"GITHUB_TOKEN=theirs"}, ""},
	} {
		if out, code := runTool(t, append(base, tc.env...), "gh", "api", "user"); out != tc.want || code != 7 {
			t.Errorf("%s: %q, %d", tc.name, out, code)
		}
	}
	os.MkdirAll(filepath.Join(home, ".config/gh"), 0o755)
	os.WriteFile(filepath.Join(home, ".config/gh/hosts.yml"), []byte("github.com:\n    user: me\n"), 0o600)
	if out, _ := runTool(t, base, "gh"); out != "" {
		t.Errorf("own hosts.yml: %q", out)
	}
	if out, _ := runTool(t, append(base, "GH_CONFIG_DIR="+t.TempDir()), "gh"); out != dummyGitHub {
		t.Errorf("GH_CONFIG_DIR without login: %q", out)
	}
	env := ghEnv(t, fake, "VMM_TOKEN_UNIX="+tokenServer(t, newAnswers(map[string]string{})))
	if out, code := runTool(t, env, "gh"); out != "" || code != 7 {
		t.Errorf("not connected: %q, %d", out, code)
	}
}

func TestGHWrapperNoGHInImage(t *testing.T) {
	empty := filepath.Join(t.TempDir(), "gh")
	if out, code := runTool(t, ghEnv(t, empty, "VMM_TOKEN_UNIX="+filepath.Join(t.TempDir(), "gone"), "PATH="+filepath.Dir(empty)), "gh", "version"); out != "" || code != 127 {
		t.Fatalf("no gh = %q, %d", out, code)
	}
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "gh"), []byte("#!/bin/sh\n"), 0o755)
	t.Setenv("PATH", dir)
	if got := lookPathWithout("gh", dir); got != "" {
		t.Fatalf("platform dir's gh found: %q", got)
	}
	if got := lookPathWithout("gh", platformBin); got != filepath.Join(dir, "gh") {
		t.Fatalf("image gh = %q", got)
	}
}

func TestIntegrationsLinearProfile(t *testing.T) {
	root := Root{t.TempDir()}
	env := guestBin(t, tokenServer(t, newAnswers(map[string]string{"linear": okAnswer(dummyLinear)})))
	t.Setenv("VMM_TOKEN_UNIX", "")
	if err := root.SyncIntegrations(context.Background(), nil, nil); err != nil {
		t.Logf("sync without a token service: %v", err)
	}
	profile := root.join("etc/profile.d/observatory-linear.sh")
	if out, _ := run(t, env, "", "sh", "-c", `unset LINEAR_API_KEY; . "$1"; echo "$LINEAR_API_KEY"`, "sh", profile); out != dummyLinear+"\n" {
		t.Errorf("unset: %q", out)
	}
	if out, _ := run(t, env, "", "sh", "-c", `LINEAR_API_KEY=mine; . "$1"; echo "$LINEAR_API_KEY"`, "sh", profile); out != "mine\n" {
		t.Errorf("own key: %q", out)
	}
	env = guestBin(t, tokenServer(t, newAnswers(map[string]string{})))
	if out, _ := run(t, env, "", "sh", "-c", `unset LINEAR_API_KEY; . "$1"; echo "${LINEAR_API_KEY-unset}"`, "sh", profile); out != "unset\n" {
		t.Errorf("not connected: %q", out)
	}
	cmd := exec.Command("/bin/sh", "-c", `unset LINEAR_API_KEY; . "$1"; echo "${LINEAR_API_KEY-unset}"`, "sh", profile)
	cmd.Env = []string{"PATH=" + t.TempDir()}
	if out, err := cmd.CombinedOutput(); err != nil || string(out) != "unset\n" {
		t.Fatalf("login without vmm-token: %v %q", err, out)
	}
}

// userGuest is a root whose dev home is a plain directory; claude runs from the host, as the test's user.
type userGuest struct {
	t                   *testing.T
	root                Root
	home, state, claude string
	log, flag           string
	env                 map[string]string
	startIn             func(*exec.Cmd) error
}

func newUserGuest(t *testing.T, answers *tokenAnswers) *userGuest {
	t.Helper()
	serve(t, answers)
	root := Root{t.TempDir()}
	g := &userGuest{t: t, root: root, home: root.join(devHome), state: root.join(stateRel)}
	os.MkdirAll(g.home, 0o755)
	aux := t.TempDir()
	g.log, g.flag = filepath.Join(aux, "argv"), filepath.Join(aux, "own-entry")
	g.claude = filepath.Join(g.home, ".local/bin/claude")
	g.env = map[string]string{"PATH": "/usr/bin:/bin"}
	g.startIn = func(cmd *exec.Cmd) error {
		cmd.Path = root.join(cmd.Path)
		cmd.Dir = g.home
		return cmd.Start()
	}
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

// sync fails on any token in the returned error and returns the claude calls made.
func (g *userGuest) sync() []string {
	g.t.Helper()
	calls, err := g.trySync()
	if err != nil {
		g.t.Fatalf("sync: %v", err)
	}
	return calls
}

func (g *userGuest) trySync() ([]string, error) {
	g.t.Helper()
	err := g.root.SyncIntegrations(context.Background(), g.startIn, g.env)
	if err != nil {
		for _, tok := range []string{dummyClaude, dummyClaude2, dummyLinear, dummyLinear2, dummyGitHub} {
			if strings.Contains(err.Error(), tok) {
				g.t.Fatalf("sync error contains a token")
			}
		}
	}
	b, _ := os.ReadFile(g.log)
	os.Remove(g.log)
	if len(b) == 0 {
		return nil, err
	}
	return strings.Split(strings.TrimSuffix(string(b), "\n"), "\n"), err
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

func TestIntegrationsClaudeCredentialsWritten(t *testing.T) {
	answers := newAnswers(map[string]string{"claude": okAnswer(dummyClaude)})
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
	if fi, _ := os.Stat(g.state); fi.Mode().Perm() != 0o600 {
		t.Errorf("state mode %v", fi.Mode().Perm())
	}
	answers.set("claude", okAnswer(dummyClaude2))
	g.sync()
	if got := g.read(g.creds()); got != credsFile(dummyClaude2) {
		t.Fatalf("after token change = %q", got)
	}
	answers.del("claude")
	g.sync()
	if got := g.read(g.creds()); got != "<missing>" {
		t.Fatalf("not connected = %q", got)
	}
}

func TestIntegrationsClaudeUserFileLeftAlone(t *testing.T) {
	answers := newAnswers(map[string]string{"claude": okAnswer(dummyClaude)})
	g := newUserGuest(t, answers)
	own := credsFile("sk-ant-oat01-users-own-login") + "\n"
	os.MkdirAll(filepath.Dir(g.creds()), 0o700)
	os.WriteFile(g.creds(), []byte(own), 0o600)
	g.sync()
	if got := g.read(g.creds()); got != own {
		t.Fatalf("after ok = %q", got)
	}
	answers.del("claude")
	g.sync()
	if got := g.read(g.creds()); got != own {
		t.Fatalf("after not-connected = %q", got)
	}
}

func TestIntegrationsClaudeFileOursByToken(t *testing.T) {
	answers := newAnswers(map[string]string{"claude": okAnswer(dummyClaude)})
	g := newUserGuest(t, answers)
	g.sync()
	pretty := "{\n  \"claudeAiOauth\": {\n    \"accessToken\": \"" + dummyClaude + "\",\n    \"refreshToken\": null,\n    \"expiresAt\": null,\n    \"scopes\": [\"user:inference\"],\n    \"subscriptionType\": \"max\"\n  }\n}\n"
	os.WriteFile(g.creds(), []byte(pretty), 0o600)
	g.sync()
	if got := g.read(g.creds()); got != pretty {
		t.Fatalf("same token rewritten: %q", got)
	}
	answers.set("claude", okAnswer(dummyClaude2))
	g.sync()
	if got := g.read(g.creds()); got != credsFile(dummyClaude2) {
		t.Fatalf("re-serialised file treated as the user's: %q", got)
	}
}

func TestIntegrationsClaudeUnavailableKeepsFile(t *testing.T) {
	answers := newAnswers(map[string]string{"claude": okAnswer(dummyClaude)})
	g := newUserGuest(t, answers)
	g.sync()
	state := g.read(g.state)
	answers.set("claude", unavail)
	g.sync()
	if got := g.read(g.creds()); got != credsFile(dummyClaude) {
		t.Fatalf("credentials = %q", got)
	}
	if got := g.read(g.state); got != state || state == "<missing>" {
		t.Fatalf("state %q -> %q", state, got)
	}
}

func TestIntegrationsClaudeLinkNotFollowed(t *testing.T) {
	g := newUserGuest(t, newAnswers(map[string]string{"claude": okAnswer(dummyClaude)}))
	outside := t.TempDir()
	os.Symlink(outside, filepath.Join(g.home, ".claude"))
	if _, err := g.trySync(); err == nil {
		t.Fatal("a linked ~/.claude was written through")
	}
	if entries, _ := os.ReadDir(outside); len(entries) != 0 {
		t.Fatalf("wrote through the link: %v", entries)
	}
}

func TestIntegrationsLinearMCPAddRemove(t *testing.T) {
	answers := newAnswers(map[string]string{"linear": okAnswer(dummyLinear)})
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
	answers.set("linear", okAnswer(dummyLinear2))
	check("new token", remove, add(dummyLinear2))
	answers.set("linear", unavail)
	check("unavailable")
	answers.del("linear")
	check("not connected", remove)
	check("not connected again")
	os.WriteFile(g.flag, nil, 0o644)
	answers.set("linear", okAnswer(dummyLinear))
	check("user's own entry", get)
	answers.del("linear")
	check("user's own entry, not connected")
}

func TestIntegrationsLinearMCPRemoveFailureKeepsRecord(t *testing.T) {
	answers := newAnswers(map[string]string{"linear": okAnswer(dummyLinear)})
	g := newUserGuest(t, answers)
	g.addClaude()
	remove := "mcp remove --scope user linear"
	g.sync()
	os.WriteFile(g.flag+".rmfail", nil, 0o644)
	answers.set("linear", okAnswer(dummyLinear2))
	if got, _ := g.trySync(); strings.Join(got, "|") != remove {
		t.Fatalf("new token, remove fails: calls %q", got)
	}
	answers.del("linear")
	for i := 0; i < 2; i++ {
		if got, _ := g.trySync(); strings.Join(got, "|") != remove {
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

func TestIntegrationsSyncWithoutClaude(t *testing.T) {
	g := newUserGuest(t, newAnswers(map[string]string{"claude": okAnswer(dummyClaude), "linear": okAnswer(dummyLinear)}))
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

func TestIntegrationsSyncNeverPrintsToken(t *testing.T) {
	answers := newAnswers(map[string]string{"github": okAnswer(dummyGitHub), "claude": okAnswer(dummyClaude), "linear": okAnswer(dummyLinear)})
	g := newUserGuest(t, answers)
	g.addClaude()
	// The fake claude echoes its argv, token included, on both streams; trySync fails on any token in the error.
	g.sync()
	answers.set("claude", okAnswer(dummyClaude2))
	answers.set("linear", okAnswer(dummyLinear2))
	g.sync()
	os.WriteFile(g.claude, []byte("#!/bin/sh\necho \"$*\" >&2\nexit 1\n"), 0o755)
	answers.set("linear", okAnswer(dummyLinear))
	g.trySync()
	answers.del("claude")
	answers.del("linear")
	g.trySync()
}

func TestIntegrationsSyncWriteErrors(t *testing.T) {
	answers := newAnswers(map[string]string{"github": okAnswer(dummyGitHub), "claude": okAnswer(dummyClaude)})
	g := newUserGuest(t, answers)
	// A file where a directory has to be: every write fails.
	os.WriteFile(g.root.join("etc"), nil, 0o644)
	os.WriteFile(filepath.Join(g.home, ".claude"), nil, 0o644)
	if _, err := g.trySync(); err == nil {
		t.Fatal("no error from failing writes")
	}
	os.Remove(filepath.Join(g.home, ".claude"))
	os.Remove(g.root.join("etc"))
	g.sync()
	if got := g.read(g.creds()); got != credsFile(dummyClaude) {
		t.Fatalf("credentials after the error = %q", got)
	}
}

func (g *userGuest) setContext(text string) {
	if err := writeFileAtomic(g.root.join(contextRel), []byte(text), 0o644); err != nil {
		g.t.Fatal(err)
	}
}

func (g *userGuest) codexAgents() string { return filepath.Join(g.home, ".codex/AGENTS.md") }

func TestIntegrationsCodexAgentsWritten(t *testing.T) {
	g := newUserGuest(t, newAnswers(map[string]string{}))
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

func TestIntegrationsCodexAgentsUserFileLeftAlone(t *testing.T) {
	g := newUserGuest(t, newAnswers(map[string]string{}))
	g.setContext("platform v1\n")
	os.MkdirAll(filepath.Dir(g.codexAgents()), 0o755)
	os.WriteFile(g.codexAgents(), []byte("my own rules\n"), 0o600)
	g.sync()
	g.sync()
	if got := g.read(g.codexAgents()); got != "my own rules\n" {
		t.Fatalf("own file = %q", got)
	}

	g = newUserGuest(t, newAnswers(map[string]string{}))
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

func TestIntegrationsWriteContext(t *testing.T) {
	root := Root{t.TempDir()}
	if err := root.WriteContext(""); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(root.join(contextRel)); err == nil {
		t.Fatal("empty context written")
	}
	if err := root.WriteContext("rules\n"); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(root.join(contextRel)); string(b) != "rules\n" {
		t.Fatalf("context = %q", b)
	}
	if l, _ := os.Readlink(root.join("etc/claude-code/CLAUDE.md")); l != "/usr/local/share/observatory/AGENTS.md" {
		t.Fatalf("link = %q", l)
	}
	own := Root{t.TempDir()}
	os.MkdirAll(own.join("etc/claude-code"), 0o755)
	os.WriteFile(own.join("etc/claude-code/CLAUDE.md"), []byte("mine\n"), 0o644)
	if err := own.WriteContext("rules\n"); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(own.join("etc/claude-code/CLAUDE.md")); string(b) != "mine\n" {
		t.Fatalf("admin file replaced: %q", b)
	}
}

package guestinit

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"time"
)

const (
	tokenPort            = 1051
	tokenTimeout         = 3 * time.Second
	tokenNotConnected    = 3
	tokenUnavailable     = 4
	linearMCP            = "https://mcp.linear.app/mcp"
	integrationsInterval = 5 * time.Minute
	claudeCLITimeout     = time.Minute
	contextRel           = "usr/local/share/observatory/AGENTS.md"
	stateRel             = "var/lib/observatory/integrations.json"
	gitconfigRel         = "etc/observatory/gitconfig"
	gitconfigBody        = "[credential \"https://github.com\"]\n\thelper = vmm\n[url \"https://github.com/\"]\n\tinsteadOf = git@github.com:\n\tinsteadOf = ssh://git@github.com/\n"
	gcloudADCRel         = devHome + "/.config/gcloud/application_default_credentials.json"
	linearProfile        = "if [ -z \"${LINEAR_API_KEY+x}\" ] && command -v vmm-token >/dev/null 2>&1 && vmm_linear=$(vmm-token linear); then export LINEAR_API_KEY=\"$vmm_linear\"; fi\nunset vmm_linear\n"
)

// A token with a newline would inject lines into git's credential protocol; Go caps repeat counts at 1000, so the upper bound is checked apart.
var tokenRE = regexp.MustCompile(`\A[A-Za-z0-9._~+/=_-]{12,}\z`)

var gcloudProjectRE = regexp.MustCompile(`^[a-z][a-z0-9-]{4,28}[a-z0-9]$`)

type tokenConn interface {
	io.ReadWriteCloser
	SetDeadline(time.Time) error
}

func dialToken() (tokenConn, error) {
	if path := os.Getenv("VMM_TOKEN_UNIX"); path != "" {
		return net.DialTimeout("unix", path, tokenTimeout)
	}
	return dialVsock(tokenTimeout)
}

// Token asks the node's token service for kind; the code is 0, tokenNotConnected or tokenUnavailable.
func Token(kind string) (string, int) {
	tok, _, code := TokenInfo(kind)
	return tok, code
}

// TokenInfo is Token plus the integration's quota project, empty when missing or not a valid project ID.
func TokenInfo(kind string) (token, project string, code int) {
	conn, err := dialToken()
	if err != nil {
		return "", "", tokenUnavailable
	}
	defer conn.Close()
	if conn.SetDeadline(time.Now().Add(tokenTimeout)) != nil {
		return "", "", tokenUnavailable
	}
	req, _ := json.Marshal(map[string]string{"op": "token", "kind": kind})
	if _, err := conn.Write(append(req, '\n')); err != nil {
		return "", "", tokenUnavailable
	}
	line, err := bufio.NewReader(io.LimitReader(conn, 16<<10)).ReadBytes('\n')
	if err != nil && err != io.EOF {
		return "", "", tokenUnavailable
	}
	var reply struct {
		OK      bool   `json:"ok"`
		Token   string `json:"token"`
		Project string `json:"project"`
		Error   string `json:"error"`
	}
	if json.Unmarshal(line, &reply) != nil {
		return "", "", tokenUnavailable
	}
	if reply.OK {
		if tok := strings.TrimSpace(reply.Token); len(tok) <= 4096 && tokenRE.MatchString(tok) {
			if gcloudProjectRE.MatchString(reply.Project) {
				project = reply.Project
			}
			return tok, project, 0
		}
		return "", "", tokenUnavailable
	}
	if reply.Error == "not-connected" {
		return "", "", tokenNotConnected
	}
	return "", "", tokenUnavailable
}

func tokenMain(args []string) int {
	if len(args) != 1 {
		fmt.Fprintln(os.Stderr, "usage: vmm-token <kind>")
		return 2
	}
	tok, code := Token(args[0])
	if code == 0 {
		fmt.Fprintln(stdout, tok)
	}
	return code
}

// credentialMain answers only get for https://github.com; anything else, and any failure, is silence so git tries its next helper.
func credentialMain(args []string) int {
	if len(args) != 1 {
		fmt.Fprintln(os.Stderr, "usage: git-credential-vmm get|store|erase")
		return 2
	}
	attrs := map[string]string{}
	sc := bufio.NewScanner(os.Stdin)
	for sc.Scan() && sc.Text() != "" {
		k, v, _ := strings.Cut(sc.Text(), "=")
		attrs[k] = v
	}
	if args[0] != "get" || attrs["protocol"] != "https" || attrs["host"] != "github.com" {
		return 0
	}
	if tok, code := Token("github"); code == 0 {
		fmt.Fprintf(stdout, "username=x-access-token\npassword=%s\n", tok)
	}
	return 0
}

var execGH = syscall.Exec

// ghMain runs the image's own gh with the VM's GitHub token, unless the user brought a token or a login.
func ghMain(args []string) int {
	real := lookPathWithout("gh", platformBin)
	if real == "" {
		fmt.Fprintln(os.Stderr, "gh: not installed in this image")
		return 127
	}
	env := os.Environ()
	if os.Getenv("GH_TOKEN") == "" && os.Getenv("GITHUB_TOKEN") == "" && !ghLoggedIn() {
		if tok, code := Token("github"); code == 0 {
			env = append(withoutEnv(env, "GH_TOKEN"), "GH_TOKEN="+tok)
		}
	}
	err := execGH(real, append([]string{"gh"}, args...), env)
	fmt.Fprintln(os.Stderr, "gh:", err)
	return 126
}

// gcloudMain runs the image's own gcloud on the placeholder ADC file, unless the user brought credentials or a login.
func gcloudMain(args []string) int {
	real := lookPathWithout("gcloud", platformBin)
	if real == "" {
		fmt.Fprintln(os.Stderr, "gcloud: not installed in this VM")
		return 127
	}
	env := os.Environ()
	home := os.Getenv("HOME")
	if home == "" {
		home = devHome
	}
	adc := filepath.Join(home, ".config/gcloud/application_default_credentials.json")
	if os.Getenv("CLOUDSDK_AUTH_CREDENTIAL_FILE_OVERRIDE") == "" && !gcloudLoggedIn(home) {
		if tok, project, code := TokenInfo("gcloud"); code == 0 {
			if data, err := readNoFollow(adc); err == nil && string(data) == gcloudADC(tok, project) {
				env = append(withoutEnv(env, "CLOUDSDK_AUTH_CREDENTIAL_FILE_OVERRIDE"), "CLOUDSDK_AUTH_CREDENTIAL_FILE_OVERRIDE="+adc)
				if ca := os.Getenv("SSL_CERT_FILE"); ca != "" && os.Getenv("CLOUDSDK_CORE_CUSTOM_CA_CERTS_FILE") == "" {
					env = append(withoutEnv(env, "CLOUDSDK_CORE_CUSTOM_CA_CERTS_FILE"), "CLOUDSDK_CORE_CUSTOM_CA_CERTS_FILE="+ca)
				}
			}
		}
	}
	err := execGH(real, append([]string{"gcloud"}, args...), env)
	fmt.Fprintln(os.Stderr, "gcloud:", err)
	return 126
}

// gcloudLoggedIn reports whether the active configuration names an account; no other gcloud file is read.
func gcloudLoggedIn(home string) bool {
	dir := os.Getenv("CLOUDSDK_CONFIG")
	if dir == "" {
		dir = filepath.Join(home, ".config/gcloud")
	}
	name := "default"
	if data, err := os.ReadFile(filepath.Join(dir, "active_config")); err == nil && strings.TrimSpace(string(data)) != "" {
		name = strings.TrimSpace(string(data))
	}
	data, err := os.ReadFile(filepath.Join(dir, "configurations", "config_"+name))
	if err != nil {
		return false
	}
	section := ""
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "[") && strings.HasSuffix(line, "]") {
			section = strings.TrimSpace(line[1 : len(line)-1])
			continue
		}
		if k, v, ok := strings.Cut(line, "="); ok && section == "core" && strings.TrimSpace(k) == "account" && strings.TrimSpace(v) != "" {
			return true
		}
	}
	return false
}

func withoutEnv(env []string, name string) []string {
	out := make([]string, 0, len(env))
	for _, kv := range env {
		if !strings.HasPrefix(kv, name+"=") {
			out = append(out, kv)
		}
	}
	return out
}

func lookPathWithout(name, excludeDir string) string {
	for _, dir := range filepath.SplitList(os.Getenv("PATH")) {
		if dir == "" || filepath.Clean(dir) == excludeDir {
			continue
		}
		p := filepath.Join(dir, name)
		if fi, err := os.Stat(p); err == nil && fi.Mode().IsRegular() && fi.Mode().Perm()&0o111 != 0 {
			return p
		}
	}
	return ""
}

func ghLoggedIn() bool {
	dir := os.Getenv("GH_CONFIG_DIR")
	if dir == "" {
		dir = filepath.Join(os.Getenv("HOME"), ".config/gh")
	}
	data, err := os.ReadFile(filepath.Join(dir, "hosts.yml"))
	if err != nil {
		return false
	}
	return bytes.HasPrefix(data, []byte("github.com:")) || bytes.Contains(data, []byte("\ngithub.com:"))
}

func digest(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

// WriteContext publishes the seed's AGENTS.md and links Claude Code's managed memory to it; a file or link the admin put there stays.
func (r Root) WriteContext(text string) error {
	if text == "" {
		return nil
	}
	if err := writeFileAtomic(r.join(contextRel), []byte(text), 0o644); err != nil {
		return err
	}
	link := r.join("etc/claude-code/CLAUDE.md")
	if _, err := os.Lstat(link); err == nil {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(link), 0o755); err != nil {
		return err
	}
	return os.Symlink("/"+contextRel, link)
}

// SyncIntegrations brings git, Claude, Linear, Codex and gcloud in line with the node's token service; startIn runs a command in the image as dev (nil starts it directly), env is the image's environment.
func (r Root) SyncIntegrations(ctx context.Context, startIn func(*exec.Cmd) error, env map[string]string) error {
	if startIn == nil {
		startIn = (*exec.Cmd).Start
	}
	errs := []error{}
	if err := r.syncGit(); err != nil {
		errs = append(errs, fmt.Errorf("gitconfig: %w", err))
	}
	if err := writeFileAtomic(r.join("etc/profile.d/observatory-linear.sh"), []byte(linearProfile), 0o644); err != nil {
		errs = append(errs, fmt.Errorf("profile: %w", err))
	}
	if err := r.syncDev(ctx, startIn, env); err != nil {
		errs = append(errs, err)
	}
	return errors.Join(errs...)
}

func (r Root) syncGit() error {
	if err := writeFileAtomic(r.join(gitconfigRel), []byte(gitconfigBody), 0o644); err != nil {
		return err
	}
	_, code := Token("github")
	if code == tokenUnavailable {
		return nil
	}
	path := r.join("etc/gitconfig")
	oldBytes, err := os.ReadFile(path)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	old := string(oldBytes)
	include := "[include]\n\tpath = /" + gitconfigRel + "\n"
	keep := strings.ReplaceAll(old, include, "")
	if keep != "" && !strings.HasSuffix(keep, "\n") {
		keep += "\n"
	}
	if code == 0 {
		keep += include
	}
	if keep == old {
		return nil
	}
	return writeFileAtomic(path, []byte(keep), 0o644)
}

type syncState map[string]string

func (r Root) syncDev(ctx context.Context, startIn func(*exec.Cmd) error, env map[string]string) error {
	home := r.join(devHome)
	if fi, err := os.Lstat(home); err != nil || !fi.IsDir() {
		return nil
	}
	state := syncState{}
	if data, err := os.ReadFile(r.join(stateRel)); err == nil {
		if json.Unmarshal(data, &state) != nil || state == nil {
			state = syncState{}
		}
	}
	old := syncState{}
	for k, v := range state {
		old[k] = v
	}
	var errs []error
	for _, part := range []struct {
		name string
		run  func() error
	}{
		{"claude credentials", func() error { return r.syncClaude(ctx, startIn, env, state) }},
		{"linear mcp", func() error { return r.syncLinear(ctx, startIn, env, state) }},
		{"codex agents", func() error { return r.syncCodex(ctx, startIn, env, state) }},
		{"gcloud adc", func() error { return r.syncGcloud(ctx, startIn, env, state) }},
	} {
		if err := part.run(); err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", part.name, err))
		}
	}
	if !mapsEqual(state, old) {
		data, _ := json.Marshal(state)
		if err := os.MkdirAll(filepath.Dir(r.join(stateRel)), 0o700); err != nil {
			errs = append(errs, fmt.Errorf("state: %w", err))
		} else if err := writeFileAtomic(r.join(stateRel), data, 0o600); err != nil {
			errs = append(errs, fmt.Errorf("state: %w", err))
		}
	}
	return errors.Join(errs...)
}

func mapsEqual(a, b syncState) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if w, ok := b[k]; !ok || w != v {
			return false
		}
	}
	return true
}

// runDevHelper runs `observatory-init <args>` in the image as dev, with content on stdin and no output kept, so a token never reaches argv or a log.
func runDevHelper(ctx context.Context, startIn func(*exec.Cmd) error, env map[string]string, stdin string, args ...string) error {
	ctx, cancel := context.WithTimeout(ctx, claudeCLITimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, platformBin+"/observatory-init", args...)
	cmd.Env = append(envList(env), "HOME="+devHome)
	cmd.Dir = "/"
	cmd.Stdin = strings.NewReader(stdin)
	if err := startIn(cmd); err != nil {
		return err
	}
	return cmd.Wait()
}

// writePrivateMain is `observatory-init write-private <path> <octal mode>`: it runs as dev, so links dev planted lead nowhere dev could not already write.
func writePrivateMain(args []string) int {
	var mode uint64
	var err error
	if len(args) == 2 {
		mode, err = strconv.ParseUint(args[1], 8, 32)
	}
	if len(args) != 2 || err != nil {
		fmt.Fprintln(os.Stderr, "usage: observatory-init write-private <path> <octal mode>")
		return 2
	}
	data, err := io.ReadAll(os.Stdin)
	if err == nil {
		err = writePrivate(args[0], data, os.FileMode(mode))
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "write-private:", err)
		return 1
	}
	return 0
}

func writePrivate(path string, data []byte, mode os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	tmp := path + ".observatory-new"
	if err := os.Remove(tmp); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL|syscall.O_NOFOLLOW, mode)
	if err != nil {
		return err
	}
	err = f.Chmod(mode)
	if err == nil {
		_, err = f.Write(data)
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Rename(tmp, path)
	}
	if err != nil {
		os.Remove(tmp)
	}
	return err
}

// removePrivateMain is `observatory-init remove-private <path>`, run as dev for the same reason.
func removePrivateMain(args []string) int {
	if len(args) != 1 {
		fmt.Fprintln(os.Stderr, "usage: observatory-init remove-private <path>")
		return 2
	}
	if err := os.Remove(args[0]); err != nil && !errors.Is(err, os.ErrNotExist) {
		fmt.Fprintln(os.Stderr, "remove-private:", err)
		return 1
	}
	return 0
}

// readNoFollow returns the file's content; a link or a FIFO is reported as an error rather than followed or waited on.
func readNoFollow(path string) ([]byte, error) {
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	if fi, err := f.Stat(); err != nil || !fi.Mode().IsRegular() {
		return nil, errors.New("not a regular file")
	}
	return io.ReadAll(f)
}

// credFileHash is the hash of the file's claudeAiOauth.accessToken: "" when missing, "?" when unreadable or without one.
func credFileHash(path string) string {
	data, err := readNoFollow(path)
	if errors.Is(err, os.ErrNotExist) {
		return ""
	}
	var doc struct {
		Claude struct {
			AccessToken *string `json:"accessToken"`
		} `json:"claudeAiOauth"`
	}
	if err != nil || json.Unmarshal(data, &doc) != nil || doc.Claude.AccessToken == nil {
		return "?"
	}
	return digest(*doc.Claude.AccessToken)
}

func (r Root) syncClaude(ctx context.Context, startIn func(*exec.Cmd) error, env map[string]string, state syncState) error {
	tok, code := Token("claude")
	if code == tokenUnavailable {
		return nil
	}
	path := r.join(devHome + "/.claude/.credentials.json")
	current := credFileHash(path)
	ours := current != "?" && current != "" && current == state["claude"]
	if code == tokenNotConnected {
		var err error
		if ours {
			err = runDevHelper(ctx, startIn, env, "", "remove-private", devHome+"/.claude/.credentials.json")
		}
		delete(state, "claude")
		return err
	}
	if current != "" && !ours {
		delete(state, "claude")
		return nil
	}
	if current != digest(tok) {
		cred := `{"claudeAiOauth":{"accessToken":` + jsonString(tok) + `,"refreshToken":null,"expiresAt":null,"scopes":["user:inference"]}}`
		if err := runDevHelper(ctx, startIn, env, cred, "write-private", devHome+"/.claude/.credentials.json", "600"); err != nil {
			return err
		}
	}
	state["claude"] = digest(tok)
	return nil
}

// gcloudADC is the placeholder ADC file; the egress proxy answers its refresh with the real token.
func gcloudADC(placeholder, project string) string {
	ph := jsonString(placeholder)
	adc := `{"type":"authorized_user","client_id":` + ph + `,"client_secret":` + ph + `,"refresh_token":` + ph
	if project != "" {
		adc += `,"quota_project_id":` + jsonString(project)
	}
	return adc + "}\n"
}

func (r Root) syncGcloud(ctx context.Context, startIn func(*exec.Cmd) error, env map[string]string, state syncState) error {
	tok, project, code := TokenInfo("gcloud")
	if code == tokenUnavailable {
		return nil
	}
	current := ""
	if data, err := readNoFollow(r.join(gcloudADCRel)); err == nil {
		current = digest(string(data))
	} else if !errors.Is(err, os.ErrNotExist) {
		current = "?"
	}
	ours := current != "" && current == state["gcloud"]
	if code == tokenNotConnected {
		var err error
		if ours {
			err = runDevHelper(ctx, startIn, env, "", "remove-private", gcloudADCRel)
		}
		delete(state, "gcloud")
		return err
	}
	if current != "" && !ours {
		delete(state, "gcloud")
		return nil
	}
	adc := gcloudADC(tok, project)
	if current != digest(adc) {
		if err := runDevHelper(ctx, startIn, env, adc, "write-private", gcloudADCRel, "600"); err != nil {
			return err
		}
	}
	state["gcloud"] = digest(adc)
	return nil
}

func jsonString(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

// findClaude returns the image-side path of the first executable claude in dev's ~/.local/bin or on PATH.
func (r Root) findClaude(path string) string {
	for _, dir := range append([]string{devHome + "/.local/bin"}, filepath.SplitList(path)...) {
		if dir != "" && filepath.IsAbs(dir) && r.executable(filepath.Join(dir, "claude")) {
			return filepath.Join(dir, "claude")
		}
	}
	return ""
}

// claudeCLI runs the image's claude as dev with no output kept: the add call carries the token on argv.
func claudeCLI(ctx context.Context, startIn func(*exec.Cmd) error, claude string, env []string, args ...string) int {
	ctx, cancel := context.WithTimeout(ctx, claudeCLITimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, claude, args...)
	cmd.Env = env
	cmd.Dir = devHome
	if err := startIn(cmd); err != nil {
		return -1
	}
	if err := cmd.Wait(); err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			return ee.ExitCode()
		}
		return -1
	}
	return 0
}

func (r Root) syncLinear(ctx context.Context, startIn func(*exec.Cmd) error, env map[string]string, state syncState) error {
	claude := r.findClaude(env["PATH"])
	if claude == "" {
		return nil
	}
	tok, code := Token("linear")
	if code == tokenUnavailable {
		return nil
	}
	recorded := state["linear_mcp"]
	if code == 0 && recorded == digest(tok) {
		return nil
	}
	cliEnv := append(envList(env), "HOME="+devHome)
	cli := func(args ...string) int { return claudeCLI(ctx, startIn, claude, cliEnv, args...) }
	if recorded != "" {
		// Keep the record on failure, so the next sync retries instead of treating the entry as the user's.
		if cli("mcp", "remove", "--scope", "user", "linear") != 0 {
			return errors.New("claude mcp remove failed")
		}
		delete(state, "linear_mcp")
	}
	if code == tokenNotConnected {
		return nil
	}
	if recorded == "" && cli("mcp", "get", "linear") == 0 {
		return nil
	}
	if cli("mcp", "add", "--transport", "http", "--scope", "user", "linear", linearMCP, "--header", "Authorization: Bearer "+tok) != 0 {
		return errors.New("claude mcp add failed")
	}
	state["linear_mcp"] = digest(tok)
	return nil
}

func (r Root) syncCodex(ctx context.Context, startIn func(*exec.Cmd) error, env map[string]string, state syncState) error {
	src, err := os.ReadFile(r.join(contextRel))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	text := string(src)
	path := r.join(devHome + "/.codex/AGENTS.md")
	current := ""
	if data, err := readNoFollow(path); err == nil {
		current = digest(string(data))
	} else if !errors.Is(err, os.ErrNotExist) {
		current = "?"
	}
	if current != "" && current != state["codex_agents"] {
		// The user's own file, or ours edited by the user.
		delete(state, "codex_agents")
		return nil
	}
	if current != digest(text) {
		if err := runDevHelper(ctx, startIn, env, text, "write-private", devHome+"/.codex/AGENTS.md", "644"); err != nil {
			return err
		}
	}
	state["codex_agents"] = digest(text)
	return nil
}

// runIntegrationSync syncs now, every interval and whenever trigger fires, until ctx ends.
func runIntegrationSync(ctx context.Context, interval time.Duration, trigger <-chan struct{}, sync func(context.Context) error) {
	tick := time.NewTicker(interval)
	defer tick.Stop()
	for {
		if err := sync(ctx); err != nil {
			fmt.Fprintln(os.Stderr, "init: integrations:", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		case <-trigger:
		}
	}
}

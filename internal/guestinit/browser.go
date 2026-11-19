package guestinit

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
)

const (
	chromeBin      = "/opt/vmm/chrome/chrome"
	agentBrowser   = "/opt/vmm/agent-browser/bin/agent-browser"
	browserDisplay = ":100"
)

var browserDefaults = [][2]string{
	{"AGENT_BROWSER_HEADED", "1"},
	{"AGENT_BROWSER_EXECUTABLE_PATH", chromeBin},
	{"AGENT_BROWSER_SESSION", "default"},
	{"AGENT_BROWSER_PROFILE", "~/.local/share/vmm-browser/default"},
	{"AGENT_BROWSER_ARGS", "--password-store=basic"},
}

// Strict on purpose: userinfo or a backslash would let a URL parser pick another host than the one we checked.
var loopbackCDP = regexp.MustCompile(`(?is)\A(?:\d+|(?:wss?|https?)://(?:127\.0\.0\.1|localhost|\[::1\])(?::\d+)?(?:[/?#].*)?)\z`)

// Chromium on Linux also takes single-dash switches; a value is only ever attached with "=".
var debugAddress = regexp.MustCompile(`(?:^|[^\w-])--?remote-debugging-address=([^,\s]*)`)

// agent-browser 0.43.0's options that take a value (flags.rs FLAGS_WITH_VALUE), so the subcommand is found after them.
var valueFlags = map[string]bool{}

func init() {
	for _, f := range strings.Fields(`--session --restore-save --restore-check-url --restore-check-text --restore-check-fn --namespace --headers
--executable-path --cdp --cdp-headers --extension --init-script --enable --profile --state --proxy --proxy-bypass --args --user-agent
-p --provider --device --session-name --color-scheme --download-path --max-output --allowed-domains --capture --action-policy
--confirm-actions --engine --input-mode --screenshot-dir --screenshot-quality --screenshot-format --idle-timeout --ca-cert --model
--config`) {
		valueFlags[f] = true
	}
}

var singletons = []string{"SingletonLock", "SingletonSocket", "SingletonCookie"}

// browser holds what agent-browser reads from its surroundings, so tests can stand in for them.
type browser struct {
	uid     int
	env     []string
	proc    string
	binary  string
	desktop func() error
	exec    func(path string, argv, env []string) error
}

func browserMain(args []string) int {
	b := &browser{uid: currentUID(), env: os.Environ(), proc: "/proc", binary: agentBrowser, exec: syscall.Exec,
		desktop: func() error { _, err := desktopCall("start"); return err }}
	return b.run(args)
}

func lookupEnv(env []string, name string) (string, bool) {
	for _, e := range env {
		if v, ok := strings.CutPrefix(e, name+"="); ok {
			return v, true
		}
	}
	return "", false
}

func refuse(msg string) int {
	fmt.Fprintln(os.Stderr, "agent-browser: "+msg)
	return 2
}

func positionals(argv []string) []string {
	var words []string
	skip := false
	for i, a := range argv {
		switch {
		case skip:
			skip = false
		case strings.HasPrefix(a, "-"):
			skip = valueFlags[a] || (i+1 < len(argv) && (argv[i+1] == "true" || argv[i+1] == "false"))
		default:
			words = append(words, a)
		}
	}
	return words
}

// checkDevtools returns why argv or env would expose DevTools beyond loopback, or "".
func checkDevtools(argv, env []string) string {
	var cdps []string
	if v, _ := lookupEnv(env, "AGENT_BROWSER_CDP"); v != "" {
		cdps = append(cdps, v)
	}
	for i, a := range argv {
		if v, ok := strings.CutPrefix(a, "--cdp="); ok {
			cdps = append(cdps, v)
		} else if a == "--cdp" && i+1 < len(argv) {
			cdps = append(cdps, argv[i+1])
		}
	}
	if words := positionals(argv); len(words) > 1 && words[0] == "connect" {
		cdps = append(cdps, words[1])
	}
	for _, c := range cdps {
		if !loopbackCDP.MatchString(c) {
			return fmt.Sprintf("CDP endpoint %q is not a port or a loopback URL (127.0.0.1, localhost, [::1])", c)
		}
	}
	extra, _ := lookupEnv(env, "AGENT_BROWSER_ARGS")
	for _, a := range append(append([]string{}, argv...), extra) {
		for _, m := range debugAddress.FindAllStringSubmatch(a, -1) {
			if m[1] != "127.0.0.1" {
				return "remote-debugging-address=" + m[1] + " is not 127.0.0.1"
			}
		}
	}
	return ""
}

func (b *browser) chromeRunning() bool {
	entries, err := os.ReadDir(b.proc)
	if err != nil {
		return false
	}
	for _, e := range entries {
		dir := filepath.Join(b.proc, e.Name())
		var st syscall.Stat_t
		if syscall.Stat(dir, &st) != nil || int(st.Uid) != b.uid {
			continue
		}
		if exe, err := os.Readlink(filepath.Join(dir, "exe")); err == nil && exe == chromeBin {
			return true
		}
	}
	return false
}

// clearStaleLock: after a fork or a disk-only restore the lock names a host or PID that is gone, and Chrome would refuse the profile.
func (b *browser) clearStaleLock(profile string) {
	owner, err := os.Readlink(filepath.Join(profile, "SingletonLock"))
	if err != nil {
		return
	}
	i := strings.LastIndex(owner, "-")
	host, pid := "", owner
	if i >= 0 {
		host, pid = owner[:i], owner[i+1:]
	}
	if self, _ := os.Hostname(); host == self && isDigits(pid) {
		if _, err := os.Stat(filepath.Join(b.proc, pid)); err == nil {
			return
		}
	}
	if b.chromeRunning() {
		return
	}
	for _, n := range singletons {
		os.Remove(filepath.Join(profile, n))
	}
}

func expandHome(p, home string) string {
	if p == "~" || strings.HasPrefix(p, "~/") {
		return home + p[1:]
	}
	return p
}

func isDigits(s string) bool {
	return s != "" && strings.Trim(s, "0123456789") == ""
}

func (b *browser) run(argv []string) int {
	if b.uid == 0 {
		fmt.Fprintln(os.Stderr, "agent-browser: run as the dev user, not root")
		return 1
	}
	if msg := checkDevtools(argv, b.env); msg != "" {
		return refuse(msg)
	}
	if err := b.desktop(); err != nil {
		var refused desktopRefused
		if errors.As(err, &refused) {
			fmt.Fprintln(os.Stderr, "vmm-desktop: "+err.Error())
		} else {
			fmt.Fprintln(os.Stderr, "agent-browser: cannot start the desktop: "+err.Error())
		}
		return 1
	}
	env := append(withoutEnv(b.env, "DISPLAY"), "DISPLAY="+browserDisplay)
	home, ok := lookupEnv(env, "HOME")
	if !ok {
		home = devHome
	}
	for _, d := range browserDefaults {
		if _, set := lookupEnv(env, d[0]); !set {
			env = append(env, d[0]+"="+expandHome(d[1], home))
		}
	}
	profile, _ := lookupEnv(env, "AGENT_BROWSER_PROFILE")
	profile = expandHome(profile, home)
	// agent-browser takes a value without "/" as the name of a profile of the system Chrome, not as a directory.
	if strings.Contains(profile, "/") {
		b.clearStaleLock(profile)
	}
	if err := b.exec(b.binary, append([]string{b.binary}, argv...), env); err != nil {
		fmt.Fprintln(os.Stderr, "agent-browser: "+err.Error())
		return 1
	}
	return 0
}

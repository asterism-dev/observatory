package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"slices"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"

	"golang.org/x/term"
)

type globals struct {
	hub, org  string
	json, yes bool
	timeout   time.Duration
}

type cli struct {
	ctx            context.Context
	stdin          io.Reader
	stdout, stderr io.Writer
	getenv         func(string) string
	terminal       bool
	poll           time.Duration
	http           *http.Client
	exec           func(path string, argv, env []string) error
	runProg        func(path string, argv []string) error
	lookPath       func(string) (string, error)
	readPassword   func() ([]byte, error)
	now            func() time.Time
	g              globals
	cfgDir         string
	cfg            config
	org            *orgJSON
	warnedExpiry   bool
	progressWhat   string
	lastProgress   string
}

func newCLI(ctx context.Context) *cli {
	return &cli{
		ctx: ctx, stdin: os.Stdin, stdout: os.Stdout, stderr: os.Stderr,
		getenv:   os.Getenv,
		terminal: term.IsTerminal(int(os.Stdin.Fd())),
		poll:     2 * time.Second,
		http:     &http.Client{Timeout: 30 * time.Second},
		exec:     syscall.Exec,
		runProg: func(path string, argv []string) error {
			cmd := exec.Command(path)
			cmd.Args = argv
			cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
			return cmd.Run()
		},
		lookPath:     exec.LookPath,
		readPassword: func() ([]byte, error) { return term.ReadPassword(int(os.Stdin.Fd())) },
		now:          time.Now,
	}
}

type command struct {
	path, args, summary string
	fn                  func(c *cli, args []string) error
}

func commands() []command {
	return slices.Concat([]command{
		{"login", "", "log in with your browser (device flow)", cmdLogin},
		{"logout", "", "revoke this CLI session", cmdLogout},
		{"whoami", "", "show the user, orgs and session", cmdWhoami},
		{"version", "", "print the CLI version", cmdVersion},
		{"help", "[noun]", "show this help", cmdHelp},
		{"org list", "", "list your orgs", cmdOrgList},
		{"org use", "<org>", "set the default org", cmdOrgUse},
		{"org show", "", "show the current org and your permissions", cmdOrgShow},
		{"org create", "<name>", "create an org", cmdOrgCreate},
		{"org rename", "<name>", "rename the current org", cmdOrgRename},
		{"org audit", "[flags]", "show the org's audit log", cmdOrgAudit},
	}, vmCommands())
}

func main() {
	os.Exit(run(newCLI(context.Background()), os.Args[1:]))
}

func isHelpFlag(s string) bool { return s == "-h" || s == "-help" || s == "--help" }

func run(c *cli, args []string) int {
	if len(args) == 0 {
		c.help(c.stderr, "")
		return 2
	}
	if isHelpFlag(args[0]) {
		c.help(c.stdout, "")
		return 0
	}
	cmd, rest, ok := findCommand(args)
	if !ok {
		if !isNoun(args[0]) {
			fmt.Fprintf(c.stderr, "observatory: unknown command %q\n", args[0])
			c.help(c.stderr, "")
			return 2
		}
		if slices.ContainsFunc(args[1:], isHelpFlag) {
			c.help(c.stdout, args[0])
			return 0
		}
		if len(args) > 1 {
			fmt.Fprintf(c.stderr, "observatory: unknown command %q\n", args[0]+" "+args[1])
		}
		c.help(c.stderr, args[0])
		return 2
	}
	dir, err := configDir(c.getenv)
	if err == nil {
		c.cfgDir = dir
		c.cfg, err = loadConfig(dir)
	}
	// A corrupt config must not block logging in again, which overwrites it.
	if err != nil && cmd.path != "login" && cmd.path != "help" && cmd.path != "version" {
		fmt.Fprintf(c.stderr, "observatory: %v\n", err)
		return 1
	}
	err = cmd.fn(c, rest)
	if errors.Is(err, flag.ErrHelp) {
		return 0
	}
	if err != nil && err.Error() != "" {
		fmt.Fprintf(c.stderr, "observatory: %v\n", err)
	}
	return exitCode(err)
}

func findCommand(args []string) (command, []string, bool) {
	var best command
	n := 0
	for _, cmd := range commands() {
		words := strings.Fields(cmd.path)
		if len(words) > n && len(words) <= len(args) && slices.Equal(words, args[:len(words)]) {
			best, n = cmd, len(words)
		}
	}
	return best, args[min(n, len(args)):], n > 0
}

func isNoun(s string) bool {
	return slices.ContainsFunc(commands(), func(cmd command) bool { return strings.HasPrefix(cmd.path, s+" ") })
}

func (c *cli) help(w io.Writer, noun string) {
	fmt.Fprintln(w, "usage: observatory <command> [args] [flags]")
	tw := tabwriter.NewWriter(w, 0, 8, 2, ' ', 0)
	group := "\n"
	for _, cmd := range commands() {
		g, _, nested := strings.Cut(cmd.path, " ")
		if (noun != "" && g != noun) || (noun == "" && nested) {
			continue
		}
		fmt.Fprintf(tw, "%s  %s\t%s\n", group, strings.TrimSpace(cmd.path+" "+cmd.args), cmd.summary)
		group = ""
	}
	if noun == "" {
		var nouns []string
		for _, cmd := range commands() {
			if g, _, nested := strings.Cut(cmd.path, " "); nested && !slices.Contains(nouns, g) {
				nouns = append(nouns, g)
			}
		}
		for _, n := range nouns {
			fmt.Fprintf(tw, "\n%s:\n", n)
			for _, cmd := range commands() {
				if strings.HasPrefix(cmd.path, n+" ") {
					fmt.Fprintf(tw, "  %s\t%s\n", strings.TrimSpace(cmd.path+" "+cmd.args), cmd.summary)
				}
			}
		}
	}
	tw.Flush()
	fmt.Fprintln(w, "\nglobal flags: --hub URL, --org NAME|ID, --json, --yes/-y, --timeout DURATION (default 5m), --help")
}

func cmdHelp(c *cli, args []string) error {
	if len(args) > 0 && !isNoun(args[0]) {
		return usagef("unknown command %q", args[0])
	}
	c.help(c.stdout, strings.Join(args[:min(1, len(args))], ""))
	return nil
}

func (c *cli) flags(path string) *flag.FlagSet {
	fs := flag.NewFlagSet(path, flag.ContinueOnError)
	fs.SetOutput(c.stderr)
	fs.StringVar(&c.g.hub, "hub", c.hub(), "hub `URL`")
	fs.StringVar(&c.g.org, "org", c.g.org, "org `NAME|ID`")
	fs.BoolVar(&c.g.json, "json", false, "print the API's JSON")
	fs.BoolVar(&c.g.yes, "yes", false, "confirm without asking")
	fs.BoolVar(&c.g.yes, "y", false, "short for --yes")
	fs.DurationVar(&c.g.timeout, "timeout", 5*time.Minute, "how long --wait waits")
	fs.Usage = func() {
		fmt.Fprintln(fs.Output(), usageLine(path))
		fs.PrintDefaults()
	}
	return fs
}

func usageLine(path string) string {
	for _, cmd := range commands() {
		if cmd.path == path {
			return strings.TrimSpace("usage: observatory " + path + " " + cmd.args)
		}
	}
	return "usage: observatory " + path
}

func (c *cli) parse(fs *flag.FlagSet, args []string, min, max int) ([]string, error) {
	var pos, tail []string
	if i := slices.Index(args, "--"); i >= 0 {
		args, tail = args[:i], args[i+1:]
	}
	for {
		if err := fs.Parse(args); err != nil {
			if errors.Is(err, flag.ErrHelp) {
				return nil, err
			}
			// flag has already printed the error and the usage.
			return nil, &exitError{2, ""}
		}
		args = fs.Args()
		if len(args) == 0 {
			break
		}
		pos, args = append(pos, args[0]), args[1:]
	}
	pos = append(pos, tail...)
	if len(pos) < min || (max >= 0 && len(pos) > max) {
		return nil, usagef("%s", usageLine(fs.Name()))
	}
	return pos, nil
}

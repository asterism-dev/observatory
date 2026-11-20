// Package guestinit is the guest's PID 1 and its multi-call helper tools.
package guestinit

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime/debug"

	"github.com/asterism-dev/observatory/internal/version"
)

var stdout io.Writer = os.Stdout

// handlers maps the argv[0] basename to its tool.
var handlers = map[string]func(args []string) int{
	"init":               func([]string) int { return runInit() },
	"observatory-init":   runSubcommand,
	"vmm-token":          tokenMain,
	"git-credential-vmm": credentialMain,
	"gh":                 ghMain,
	"agent-browser":      browserMain,
	"vmm-desktop":        desktopMain,
	"gcloud":             gcloudMain,
}

// runSubcommand serves the internal commands of the binary under its own name, such as the image's nsinit shim.
func runSubcommand(args []string) int {
	if len(args) > 0 {
		switch args[0] {
		case "nsinit":
			return runNsinit(args[1:])
		case "write-private":
			return writePrivateMain(args[1:])
		case "remove-private":
			return removePrivateMain(args[1:])
		case "sftp-server":
			return sftpServerMain()
		}
	}
	fmt.Fprintln(os.Stderr, "usage: observatory-init nsinit [-dir DIR] [-stop-signal SIG] -- ARGV...")
	return 2
}

type bootStep struct {
	name string
	run  func() error
}

// runSteps logs a failing step and goes on: once the image root is in place, a guest with a broken step is still reachable, while PID 1 exiting panics the kernel.
func runSteps(steps []bootStep) {
	for _, s := range steps {
		if err := s.safeRun(); err != nil {
			fmt.Fprintf(os.Stderr, "init: %s: %v\n", s.name, err)
		}
	}
}

func (s bootStep) safeRun() (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("panic: %v\n%s", r, debug.Stack())
		}
	}()
	return s.run()
}

func runInit() int {
	if err := Run(); err != nil {
		fmt.Fprintln(os.Stderr, "init:", err)
		return 1
	}
	return 0
}

// Main dispatches on the basename of argv[0].
func Main(argv []string) int {
	if len(argv) == 0 {
		return 2
	}
	if len(argv) == 2 && argv[1] == "--version" && filepath.Base(argv[0]) == "observatory-init" {
		fmt.Fprintln(stdout, version.Version)
		return 0
	}
	h, ok := handlers[filepath.Base(argv[0])]
	if !ok {
		fmt.Fprintf(os.Stderr, "unknown tool %q\n", filepath.Base(argv[0]))
		return 2
	}
	return h(argv[1:])
}

// Command build-agent is the entrypoint of the builder image: it runs one build and powers the builder VM off.
package main

import (
	"context"
	"fmt"
	"os"
	"syscall"
	"time"

	"github.com/asterism-dev/observatory/internal/buildagent"
)

func main() {
	a, err := buildagent.FromEnv("/run/observatory-build")
	if err == nil {
		err = a.Run(context.Background())
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "build-agent:", err)
	}
	// PID 1 here is the guest's nsinit, which would restart the agent; SIGTERM makes it exit, and the guest init powers the VM off.
	if err := syscall.Kill(1, syscall.SIGTERM); err != nil {
		fmt.Fprintln(os.Stderr, "build-agent: stop nsinit:", err)
		if err := powerOff(); err != nil {
			fmt.Fprintln(os.Stderr, "build-agent: poweroff:", err)
		}
	}
	// Never exit: nsinit would rerun the build. Sleep rather than select{}, which the runtime may end as a deadlock.
	for {
		time.Sleep(time.Hour)
	}
}

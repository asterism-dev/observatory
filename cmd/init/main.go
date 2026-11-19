// Command init is the guest's PID 1 and, under other names, the guest-side helper tools.
package main

import (
	"os"

	"github.com/asterism-dev/observatory/internal/guestinit"
)

func main() {
	os.Exit(guestinit.Main(os.Args))
}

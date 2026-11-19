// Command init is the guest's PID 1; this is a placeholder until the real init lands.
package main

import (
	"fmt"
	"os"

	"github.com/asterism-dev/observatory/internal/version"
)

func main() {
	if len(os.Args) == 2 && os.Args[1] == "--version" {
		fmt.Println(version.Version)
		return
	}
	fmt.Fprintln(os.Stderr, "not implemented")
	os.Exit(1)
}

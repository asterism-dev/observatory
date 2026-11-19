//go:build !linux

package guestinit

import (
	"errors"
	"fmt"
	"os"
)

func Run() error { return errors.New("the guest init runs on linux only") }

func runNsinit([]string) int {
	fmt.Fprintln(os.Stderr, "nsinit runs on linux only")
	return 1
}

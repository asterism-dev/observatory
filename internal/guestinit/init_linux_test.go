package guestinit

import (
	"syscall"
	"testing"
)

func TestShutdownFor(t *testing.T) {
	if got := shutdownFor(syscall.SIGINT); got != syscall.LINUX_REBOOT_CMD_POWER_OFF {
		t.Fatalf("SIGINT -> %#x", got)
	}
	if got := shutdownFor(syscall.SIGHUP); got != syscall.LINUX_REBOOT_CMD_RESTART {
		t.Fatalf("SIGHUP -> %#x", got)
	}
}

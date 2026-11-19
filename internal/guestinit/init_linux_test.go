package guestinit

import (
	"os/exec"
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

func TestImageShutdownCmd(t *testing.T) {
	for script, want := range map[string]int{
		"kill -INT $$": syscall.LINUX_REBOOT_CMD_POWER_OFF,
		"kill -HUP $$": syscall.LINUX_REBOOT_CMD_RESTART,
		"exit 0":       syscall.LINUX_REBOOT_CMD_POWER_OFF,
	} {
		cmd := exec.Command("/bin/sh", "-c", script)
		_ = cmd.Run()
		if got := (&Image{cmd: cmd}).shutdownCmd(); got != want {
			t.Errorf("%s: %#x, want %#x", script, got, want)
		}
	}
}

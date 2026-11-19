package guestinit

import (
	"os/exec"
	"slices"
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

func TestSwitchRootOps(t *testing.T) {
	want := []mountOp{
		{"/.observatory", "/newroot/.observatory", syscall.MS_BIND | syscall.MS_REC},
		{"", "/newroot/.observatory", syscall.MS_BIND | syscall.MS_REMOUNT | syscall.MS_RDONLY},
		{"/dev", "/newroot/dev", syscall.MS_MOVE},
		{"/proc", "/newroot/proc", syscall.MS_MOVE},
		{"/sys", "/newroot/sys", syscall.MS_MOVE},
		{"/newroot", "/", syscall.MS_MOVE},
	}
	if got := switchRootOps("/newroot"); !slices.Equal(got, want) {
		t.Fatalf("got %+v\nwant %+v", got, want)
	}
}

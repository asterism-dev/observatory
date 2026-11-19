package main

import "golang.org/x/sys/unix"

// powerOff from inside the image's PID namespace makes the kernel kill nsinit with SIGINT, which the guest init maps to poweroff.
func powerOff() error { return unix.Reboot(unix.LINUX_REBOOT_CMD_POWER_OFF) }

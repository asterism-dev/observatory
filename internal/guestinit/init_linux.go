package guestinit

import (
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"syscall"

	"github.com/asterism-dev/observatory/internal/proto"
)

const (
	rootDevice = "/dev/vda"
	// SeedDevice is the drive after the root disk, where the node attaches the seed ISO.
	SeedDevice = "/dev/vdb"
)

// Run is PID 1: it boots the guest, then blocks until the kernel asks for poweroff or reboot.
func Run() error {
	// Registered first so a signal during boot is not lost.
	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, syscall.SIGINT, syscall.SIGHUP)

	if err := mountBase(); err != nil {
		return err
	}
	seed, err := ReadSeed(SeedDevice)
	if err != nil {
		return err
	}
	root, err := mountRoot(rootDevice)
	if err != nil {
		return err
	}
	if err := growRoot(rootDevice); err != nil {
		return err
	}
	// Tasks 4-10 add their steps here, in boot order, each taking root and seed.
	_ = seed
	_ = root

	announceReady()

	// The kernel sends SIGINT to PID 1 on poweroff and SIGHUP on reboot.
	sig := <-sigs
	cmd := syscall.LINUX_REBOOT_CMD_POWER_OFF
	if sig == syscall.SIGHUP {
		cmd = syscall.LINUX_REBOOT_CMD_RESTART
	}
	shutdown(cmd)
	return nil
}

// announceReady writes the ready marker to the first console that opens; Task 6 gates it on the primary NIC address.
func announceReady() {
	for _, path := range []string{"/dev/hvc0", "/dev/ttyS0"} {
		f, err := os.OpenFile(path, os.O_WRONLY, 0)
		if err != nil {
			continue
		}
		fmt.Fprintln(f, proto.ReadyMarker)
		f.Close()
		return
	}
}

// shutdown stops the image (Task 7 hook), unmounts /newroot and powers off or reboots; it only returns on failure.
func shutdown(cmd int) {
	// Task 7: stop the image process here.
	if err := exec.Command("/bin/umount", "-R", "/newroot").Run(); err != nil {
		fmt.Fprintln(os.Stderr, "init: umount /newroot:", err)
	}
	syscall.Sync()
	if err := syscall.Reboot(cmd); err != nil {
		fmt.Fprintln(os.Stderr, "init: reboot:", err)
	}
}

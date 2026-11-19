package guestinit

import (
	"errors"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/asterism-dev/observatory/internal/proto"
)

const rootDevice = "/dev/vda"

// Run is PID 1: it boots the guest, then blocks until the kernel asks for poweroff or reboot.
func Run() error {
	// Registered first so a signal during boot is not lost.
	// With ctrl-alt-del off the kernel delivers it to PID 1 as SIGINT instead of rebooting hard.
	if err := syscall.Reboot(syscall.LINUX_REBOOT_CMD_CAD_OFF); err != nil {
		return fmt.Errorf("disable ctrl-alt-del: %w", err)
	}
	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, syscall.SIGINT, syscall.SIGHUP)

	if err := mountBase(); err != nil {
		return err
	}
	seedDev, err := FindSeedDevice("/sys/block")
	if err != nil {
		return err
	}
	seed, err := ReadSeed(seedDev)
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
	// Tasks 6-10 add their steps here, in boot order, each taking root and seed.
	if err := root.EnsureDev(); err != nil {
		return err
	}
	// A guest without the CAs still runs; only hosts bound to the egress proxy fail TLS.
	if _, err := root.InstallCAs(seed.EgressCAs); err != nil {
		fmt.Fprintln(os.Stderr, "init: egress CAs:", err)
	}
	if _, err := root.WriteEnv(seed.Env, seed.Image.Env); err != nil {
		fmt.Fprintln(os.Stderr, "init: environment:", err)
	}

	announceReady()

	// The kernel signals PID 1 only for ctrl-alt-del (SIGINT); the image's own poweroff/restart reaches us as its exit status.
	sig := <-sigs
	shutdown(shutdownFor(sig.(syscall.Signal)))
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

// shutdownFor maps the signal that ends a guest to a reboot(2) command: SIGHUP restarts, anything else powers off.
func shutdownFor(sig syscall.Signal) int {
	if sig == syscall.SIGHUP {
		return syscall.LINUX_REBOOT_CMD_RESTART
	}
	return syscall.LINUX_REBOOT_CMD_POWER_OFF
}

// shutdown stops the image (Task 7 hook), unmounts /newroot and powers off or reboots; it only returns on failure.
func shutdown(cmd int) {
	// Task 7: stop the image process here.
	if err := unmountRoot(); err != nil {
		fmt.Fprintln(os.Stderr, "init:", err)
	}
	syscall.Sync()
	if err := syscall.Reboot(cmd); err != nil {
		fmt.Fprintln(os.Stderr, "init: reboot:", err)
	}
}

func unmountRoot() error {
	f, err := os.Open("/proc/self/mountinfo")
	if err != nil {
		return err
	}
	defer f.Close()
	var errs []error
	for _, m := range mountsBelow(f, "/newroot") {
		err := syscall.Unmount(m, 0)
		if errors.Is(err, syscall.EBUSY) {
			err = syscall.Unmount(m, syscall.MNT_DETACH)
		}
		if err != nil {
			errs = append(errs, fmt.Errorf("umount %s: %w", m, err))
		}
	}
	return errors.Join(errs...)
}

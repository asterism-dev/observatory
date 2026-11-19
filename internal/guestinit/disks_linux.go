package guestinit

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/asterism-dev/observatory/internal/proto"
)

// mke2fs is the initramfs's, reachable through the platform bind after the root switch; a var so tests can use the host's.
var mke2fs = platformDir + "/libexec/mke2fs"

// MountDisks formats blank attached disks, mounts each at its path in r and grows its filesystem.
func MountDisks(r Root, disks []proto.GuestDisk) error {
	var errs []error
	for _, d := range disks {
		if err := mountDisk(r, d); err != nil {
			errs = append(errs, fmt.Errorf("disk %s: %w", d.Device, err))
		}
	}
	return errors.Join(errs...)
}

func mountDisk(r Root, d proto.GuestDisk) error {
	if !waitBlockDevice(d.Device, 30*time.Second) {
		return errors.New("device did not appear")
	}
	blank, err := deviceBlank(d.Device)
	if err != nil {
		return err
	}
	if blank {
		// nodiscard: a discard on NBD punches holes into the backing file, a wipe if this ever misjudges.
		if out, err := exec.Command(mke2fs, "-t", "ext4", "-q", "-E", "nodiscard", d.Device).CombinedOutput(); err != nil {
			return fmt.Errorf("mke2fs: %w: %s", err, out)
		}
	}
	target := r.join(d.MountPath)
	if err := os.MkdirAll(target, 0o755); err != nil {
		return err
	}
	if !isMountpoint(target) {
		if err := mountFS(d.Device, target); err != nil {
			return fmt.Errorf("mount %s: %w", target, err)
		}
	}
	// Grows a disk that was resized while detached; a filesystem other than ext4 is left as is.
	growFS(d.Device, target)
	if blank {
		return chown(target, devID, devID)
	}
	return nil
}

func waitBlockDevice(dev string, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for {
		if fi, err := os.Stat(dev); err == nil && fi.Mode()&os.ModeDevice != 0 {
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(time.Second)
	}
}

func deviceBlank(dev string) (bool, error) {
	f, err := os.Open(dev)
	if err != nil {
		return false, err
	}
	defer f.Close()
	return IsBlank(f)
}

func isMountpoint(path string) bool {
	var st, parent syscall.Stat_t
	if syscall.Stat(path, &st) != nil || syscall.Stat(filepath.Dir(path), &parent) != nil {
		return false
	}
	return st.Dev != parent.Dev
}

// mountFS tries ext4 first, then every block filesystem the kernel knows, so a disk the user reformatted still mounts.
func mountFS(dev, target string) error {
	err := syscall.Mount(dev, target, "ext4", 0, "")
	if err == nil {
		return nil
	}
	data, _ := os.ReadFile("/proc/filesystems")
	for _, line := range strings.Split(string(data), "\n") {
		fstype := strings.TrimSpace(line)
		if fstype == "" || fstype == "ext4" || strings.HasPrefix(line, "nodev") {
			continue
		}
		if syscall.Mount(dev, target, fstype, 0, "") == nil {
			return nil
		}
	}
	return err
}

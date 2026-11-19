package guestinit

import (
	"errors"
	"fmt"
	"io"
	"os"
	"syscall"
	"unsafe"

	"golang.org/x/sys/unix"
)

func mountBase() error {
	for _, m := range []struct{ src, target, fstype string }{
		{"proc", "/proc", "proc"},
		{"sysfs", "/sys", "sysfs"},
		{"devtmpfs", "/dev", "devtmpfs"},
		{"devpts", "/dev/pts", "devpts"},
		{"tmpfs", "/dev/shm", "tmpfs"},
		{"cgroup2", "/sys/fs/cgroup", "cgroup2"},
	} {
		if err := os.MkdirAll(m.target, 0o755); err != nil {
			return err
		}
		if err := syscall.Mount(m.src, m.target, m.fstype, 0, ""); err != nil && !errors.Is(err, syscall.EBUSY) {
			return fmt.Errorf("mount %s: %w", m.target, err)
		}
	}
	return nil
}

// newRoot is where the image's disk is mounted until switchRoot makes it "/".
const newRoot = "/newroot"

func mountRoot(dev string) error {
	if err := os.MkdirAll(newRoot, 0o755); err != nil {
		return err
	}
	if err := syscall.Mount(dev, newRoot, "ext4", 0, ""); err != nil {
		return fmt.Errorf("mount %s: %w", dev, err)
	}
	return nil
}

type mountOp struct {
	source, target string
	flags          uintptr
}

// switchRootOps are the mounts that make root the real root of the mount namespace, as switch_root does.
func switchRootOps(root string) []mountOp {
	return []mountOp{
		{platformDir, root + platformDir, syscall.MS_BIND | syscall.MS_REC},
		{"", root + platformDir, syscall.MS_BIND | syscall.MS_REMOUNT | syscall.MS_RDONLY},
		{"/dev", root + "/dev", syscall.MS_MOVE},
		{"/proc", root + "/proc", syscall.MS_MOVE},
		{"/sys", root + "/sys", syscall.MS_MOVE},
		{root, "/", syscall.MS_MOVE},
	}
}

// switchRoot moves root onto "/" and enters it; the kernel refuses user namespaces to anything still chrooted below the initramfs.
func switchRoot(root string) error {
	if err := os.Chdir(root); err != nil {
		return err
	}
	for _, op := range switchRootOps(root) {
		if err := os.MkdirAll(op.target, 0o755); err != nil {
			return err
		}
		if err := syscall.Mount(op.source, op.target, "", op.flags, ""); err != nil {
			return fmt.Errorf("mount %s on %s: %w", op.source, op.target, err)
		}
	}
	if err := syscall.Chroot("."); err != nil {
		return fmt.Errorf("chroot: %w", err)
	}
	return os.Chdir("/")
}

// ext4IocResizeFS is EXT4_IOC_RESIZE_FS, _IOW('f', 16, __u64).
const ext4IocResizeFS = 0x40086610

// growFS grows the ext4 filesystem of dev mounted at dir to the device's size; the guest kernel refuses resize2fs's writable open of a mounted device.
func growFS(dev, dir string) error {
	d, err := os.Open(dev)
	if err != nil {
		return err
	}
	size, err := d.Seek(0, io.SeekEnd)
	d.Close()
	if err != nil {
		return err
	}
	var st unix.Statfs_t
	if err := unix.Statfs(dir, &st); err != nil {
		return err
	}
	m, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer m.Close()
	blocks := uint64(size) / uint64(st.Bsize)
	if _, _, errno := unix.Syscall(unix.SYS_IOCTL, m.Fd(), ext4IocResizeFS, uintptr(unsafe.Pointer(&blocks))); errno != 0 {
		return fmt.Errorf("grow %s to %d blocks: %w", dir, blocks, errno)
	}
	return nil
}

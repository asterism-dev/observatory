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

func mountRoot(dev string) (Root, error) {
	root := Root{Path: "/newroot"}
	if err := os.MkdirAll(root.Path, 0o755); err != nil {
		return root, err
	}
	if err := syscall.Mount(dev, root.Path, "ext4", 0, ""); err != nil {
		return root, fmt.Errorf("mount %s: %w", dev, err)
	}
	return root, nil
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

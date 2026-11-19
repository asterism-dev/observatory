package guestinit

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"syscall"
)

func mountBase() error {
	for _, m := range []struct{ src, target, fstype string }{
		{"proc", "/proc", "proc"},
		{"sysfs", "/sys", "sysfs"},
		{"devtmpfs", "/dev", "devtmpfs"},
		{"devpts", "/dev/pts", "devpts"},
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

func growRoot(dev string) error {
	if out, err := exec.Command(resize2fs, dev).CombinedOutput(); err != nil {
		return fmt.Errorf("resize2fs %s: %w: %s", dev, err, out)
	}
	return nil
}

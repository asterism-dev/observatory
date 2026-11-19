package guestinit

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/asterism-dev/observatory/internal/proto"
)

func TestMountDisksLoop(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("needs root")
	}
	bin, err := exec.LookPath("mke2fs")
	if err != nil {
		t.Skip(err)
	}
	mke2fs = bin
	img := filepath.Join(t.TempDir(), "disk.img")
	if err := os.WriteFile(img, make([]byte, 64<<20), 0o600); err != nil {
		t.Fatal(err)
	}
	out, err := exec.Command("losetup", "--find", "--show", img).Output()
	if err != nil {
		t.Fatal(err)
	}
	dev := strings.TrimSpace(string(out))
	t.Cleanup(func() { exec.Command("losetup", "-d", dev).Run() })

	r := Root{t.TempDir()}
	mnt := r.join("data")
	disks := []proto.GuestDisk{{Device: dev, MountPath: "/data"}}
	if err := MountDisks(r, disks); err != nil {
		t.Fatal(err)
	}
	var st syscall.Stat_t
	if err := syscall.Stat(mnt, &st); err != nil || st.Uid != devID || !isMountpoint(mnt) {
		t.Fatalf("fresh disk: uid %d mounted %v err %v", st.Uid, isMountpoint(mnt), err)
	}
	if err := os.WriteFile(filepath.Join(mnt, "keep"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := MountDisks(r, disks); err != nil {
		t.Fatal("remount while mounted:", err)
	}
	if err := os.Chown(mnt, 0, 0); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Unmount(mnt, 0); err != nil {
		t.Fatal(err)
	}
	if err := MountDisks(r, disks); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { syscall.Unmount(mnt, 0) })
	if _, err := os.Stat(filepath.Join(mnt, "keep")); err != nil {
		t.Fatal("disk reformatted:", err)
	}
	if err := syscall.Stat(mnt, &st); err != nil || st.Uid != 0 {
		t.Fatalf("existing disk re-chowned: uid %d", st.Uid)
	}
}

func TestGrowFSLoop(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("needs root")
	}
	img := filepath.Join(t.TempDir(), "disk.img")
	if out, err := exec.Command("mke2fs", "-q", "-t", "ext4", img, "32M").CombinedOutput(); err != nil {
		t.Skipf("mke2fs: %v: %s", err, out)
	}
	if err := os.Truncate(img, 128<<20); err != nil {
		t.Fatal(err)
	}
	out, err := exec.Command("losetup", "--find", "--show", img).Output()
	if err != nil {
		t.Fatal(err)
	}
	dev := strings.TrimSpace(string(out))
	t.Cleanup(func() { exec.Command("losetup", "-d", dev).Run() })
	mnt := t.TempDir()
	if err := syscall.Mount(dev, mnt, "ext4", 0, ""); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { syscall.Unmount(mnt, 0) })
	for range 2 {
		if err := growFS(dev, mnt); err != nil {
			t.Fatal(err)
		}
	}
	var st syscall.Statfs_t
	if err := syscall.Statfs(mnt, &st); err != nil || int64(st.Blocks)*st.Bsize < 100<<20 {
		t.Fatalf("size after grow = %d bytes, %v", int64(st.Blocks)*st.Bsize, err)
	}
}

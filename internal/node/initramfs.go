package node

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
)

// guestTools are the helper names the image runs from /.observatory/bin; the init dispatches on argv[0].
var guestTools = []string{"vmm-token", "git-credential-vmm", "gh", "agent-browser", "vmm-desktop", "gcloud"}

// BuildInitramfs writes the guest's initramfs (newc cpio) from the installed guestboot dir to out.
func BuildInitramfs(guestbootDir, out string) error {
	var b bytes.Buffer
	ino := uint32(0)
	add := func(name string, mode uint32, data []byte, rdev [2]uint32) {
		ino++
		nlink := uint32(1)
		if mode&syscall.S_IFMT == syscall.S_IFDIR {
			nlink = 2
		}
		fmt.Fprintf(&b, "070701%08X%08X%08X%08X%08X%08X%08X%08X%08X%08X%08X%08X%08X",
			ino, mode, 0, 0, nlink, 0, len(data), 0, 0, rdev[0], rdev[1], len(name)+1, 0)
		b.WriteString(name + "\x00")
		pad(&b)
		b.Write(data)
		pad(&b)
	}
	file := func(name, src string) error {
		data, err := os.ReadFile(filepath.Join(guestbootDir, src))
		if err == nil {
			add(name, syscall.S_IFREG|0o755, data, [2]uint32{})
		}
		return err
	}
	for _, d := range []string{"dev", ".observatory", ".observatory/bin", ".observatory/libexec"} {
		add(d, syscall.S_IFDIR|0o755, nil, [2]uint32{})
	}
	// The kernel opens the console for /init before anything mounts devtmpfs.
	add("dev/console", syscall.S_IFCHR|0o600, nil, [2]uint32{5, 1})
	if err := file(".observatory/bin/observatory-init", "observatory-init"); err != nil {
		return err
	}
	add("init", syscall.S_IFLNK|0o777, []byte(".observatory/bin/observatory-init"), [2]uint32{})
	for _, t := range guestTools {
		add(".observatory/bin/"+t, syscall.S_IFLNK|0o777, []byte("observatory-init"), [2]uint32{})
	}
	// Under /.observatory, the only initramfs path the init still reaches after switching into the image's root.
	for _, t := range []string{"mke2fs", "resize2fs"} {
		if err := file(".observatory/libexec/"+t, t); err != nil {
			return err
		}
	}
	add("TRAILER!!!", 0, nil, [2]uint32{})
	tmp := out + ".tmp"
	if err := os.WriteFile(tmp, b.Bytes(), 0o644); err != nil {
		return err
	}
	// Jails hard-link it as a shared file, which must be root's and 0644 whatever the umask.
	if err := os.Chmod(tmp, 0o644); err != nil {
		os.Remove(tmp)
		return err
	}
	return os.Rename(tmp, out)
}

func pad(b *bytes.Buffer) {
	for b.Len()%4 != 0 {
		b.WriteByte(0)
	}
}

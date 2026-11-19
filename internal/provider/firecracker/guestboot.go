package firecracker

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/asterism-dev/observatory/internal/node"
	"github.com/asterism-dev/observatory/internal/version"
)

// guestbootDir is where the installer unpacks guestboot.tar; tests point it elsewhere.
var guestbootDir = "/usr/local/lib/observatory/guestboot"

type guestboot struct{ kernel, initrd, id string } // id changes with kernel and initramfs

// loadGuestboot checks src matches this node and puts the kernel and a fresh initramfs into dir, where jails can hard-link them.
func loadGuestboot(src, dir string) (guestboot, error) {
	v, err := os.ReadFile(filepath.Join(src, "VERSION"))
	if err != nil {
		return guestboot{}, fmt.Errorf("guestboot: %w; rerun the installer", err)
	}
	if got := strings.TrimSpace(string(v)); got != version.Version {
		return guestboot{}, fmt.Errorf("guestboot %s does not match node %s; rerun the installer", got, version.Version)
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return guestboot{}, err
	}
	g := guestboot{kernel: filepath.Join(dir, "vmlinux"), initrd: filepath.Join(dir, "initramfs-"+version.Version+".cpio")}
	if err := copyShared(filepath.Join(src, "vmlinux"), g.kernel); err != nil {
		return guestboot{}, err
	}
	if err := node.BuildInitramfs(src, g.initrd); err != nil {
		return guestboot{}, fmt.Errorf("initramfs: %w", err)
	}
	old, _ := filepath.Glob(filepath.Join(dir, "initramfs-*.cpio"))
	for _, p := range old {
		if p != g.initrd {
			os.Remove(p)
		}
	}
	h := sha256.New()
	for _, p := range []string{g.kernel, g.initrd} {
		f, err := os.Open(p)
		if err != nil {
			return guestboot{}, err
		}
		_, err = io.Copy(h, f)
		f.Close()
		if err != nil {
			return guestboot{}, err
		}
	}
	g.id = hex.EncodeToString(h.Sum(nil))[:16]
	return g, nil
}

// copyShared copies src to dst as a 0644 file, replacing dst by rename so running jails keep their link to the old one.
func copyShared(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	tmp := dst + ".tmp"
	out, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
	if err == nil {
		_, err = io.Copy(out, in)
		err = errors.Join(err, out.Close())
	}
	if err == nil {
		err = os.Chmod(tmp, 0o644)
	}
	if err == nil {
		err = os.Rename(tmp, dst)
	}
	if err != nil {
		os.Remove(tmp)
	}
	return err
}

package guestinit

import (
	"bytes"
	"errors"
	"io"
)

const blankProbe = 1 << 20

// IsBlank reports whether dev's first MiB is readable and all zero; a short or failed read is never blank, so a disk is not formatted by mistake.
func IsBlank(dev io.ReaderAt) (bool, error) {
	// ponytail: only the first MiB is probed, so signatures blkid finds near the end of a disk (md 1.0, backup GPT) do not count; probe those if such disks show up.
	buf := make([]byte, blankProbe)
	n, err := dev.ReadAt(buf, 0)
	if n < blankProbe {
		if err == nil || errors.Is(err, io.EOF) {
			return false, nil
		}
		return false, err
	}
	// Every superblock blkid knows within the first MiB (ext4 at 0x438, xfs, btrfs, LVM, swap) is non-zero.
	return !bytes.ContainsFunc(buf, func(r rune) bool { return r != 0 }), nil
}

package guestinit

import (
	"os"
	"strings"

	"golang.org/x/sys/unix"
)

const authorizedKeysFile = devHome + "/.ssh/authorized_keys"

// openAuthorizedKeys resolves symlinks inside the root, so a link planted in the image cannot reach the initramfs.
func (r Root) openAuthorizedKeys() (*os.File, error) {
	dir, err := unix.Open(r.Path, unix.O_PATH|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	defer unix.Close(dir)
	fd, err := unix.Openat2(dir, strings.TrimPrefix(authorizedKeysFile, "/"), &unix.OpenHow{
		Flags:   unix.O_RDONLY | unix.O_NONBLOCK | unix.O_CLOEXEC,
		Resolve: unix.RESOLVE_IN_ROOT,
	})
	if err != nil {
		return nil, err
	}
	return os.NewFile(uintptr(fd), r.join(authorizedKeysFile)), nil
}

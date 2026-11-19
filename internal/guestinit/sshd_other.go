//go:build !linux

package guestinit

import (
	"os"
	"syscall"
)

func (r Root) openAuthorizedKeys() (*os.File, error) {
	return os.OpenFile(r.join(devHome+"/.ssh/authorized_keys"), os.O_RDONLY|syscall.O_NONBLOCK, 0)
}

//go:build !linux

package guestinit

import (
	"errors"
	"time"
)

func dialVsock(uint32, time.Duration) (tokenConn, error) {
	return nil, errors.New("vsock needs linux")
}

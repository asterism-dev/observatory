//go:build !linux

package guestinit

import (
	"errors"
	"time"
)

func dialVsock(time.Duration) (tokenConn, error) {
	return nil, errors.New("vsock needs linux")
}

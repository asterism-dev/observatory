package guestinit

import (
	"errors"
	"io"
	"os"
	"time"

	"golang.org/x/sys/unix"
)

// vsockConn is a blocking vsock stream whose deadline is applied as socket timeouts, as the Go runtime does not poll AF_VSOCK for us here.
type vsockConn struct {
	fd       int
	deadline time.Time
}

func dialVsock(timeout time.Duration) (tokenConn, error) {
	fd, err := unix.Socket(unix.AF_VSOCK, unix.SOCK_STREAM|unix.SOCK_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	c := &vsockConn{fd: fd}
	if err := c.SetDeadline(time.Now().Add(timeout)); err != nil {
		c.Close()
		return nil, err
	}
	if err := unix.Connect(fd, &unix.SockaddrVM{CID: unix.VMADDR_CID_HOST, Port: tokenPort}); err != nil {
		c.Close()
		return nil, err
	}
	return c, nil
}

func (c *vsockConn) SetDeadline(t time.Time) error { c.deadline = t; return c.applyTimeout() }

func (c *vsockConn) applyTimeout() error {
	left := time.Until(c.deadline)
	if left <= 0 {
		return os.ErrDeadlineExceeded
	}
	tv := unix.NsecToTimeval(left.Nanoseconds())
	if err := unix.SetsockoptTimeval(c.fd, unix.SOL_SOCKET, unix.SO_RCVTIMEO, &tv); err != nil {
		return err
	}
	return unix.SetsockoptTimeval(c.fd, unix.SOL_SOCKET, unix.SO_SNDTIMEO, &tv)
}

func (c *vsockConn) Read(p []byte) (int, error) {
	for {
		if err := c.applyTimeout(); err != nil {
			return 0, err
		}
		n, err := unix.Read(c.fd, p)
		switch {
		case errors.Is(err, unix.EINTR):
			continue
		case err != nil:
			return 0, err
		case n == 0:
			return 0, io.EOF
		}
		return n, nil
	}
}

func (c *vsockConn) Write(p []byte) (int, error) {
	done := 0
	for done < len(p) {
		if err := c.applyTimeout(); err != nil {
			return done, err
		}
		n, err := unix.Write(c.fd, p[done:])
		if err != nil && !errors.Is(err, unix.EINTR) {
			return done, err
		}
		done += max(n, 0)
	}
	return done, nil
}

func (c *vsockConn) Close() error { return unix.Close(c.fd) }

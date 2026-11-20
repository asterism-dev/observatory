package guestinit

import (
	"context"
	"net"
	"os"

	"golang.org/x/sys/unix"
)

const telemetryPort = 1052

// dialTelemetry connects to the node's telemetry listener; nonblocking, the os.File is pollable, so the HTTP client's idle reads hold no socket timeout.
func dialTelemetry(context.Context) (net.Conn, error) {
	c, err := dialVsock(telemetryPort, tokenTimeout)
	if err != nil {
		return nil, err
	}
	fd := c.(*vsockConn).fd
	if err := unix.SetNonblock(fd, true); err != nil {
		c.Close()
		return nil, err
	}
	return vsockFile{os.NewFile(uintptr(fd), "vsock")}, nil
}

type vsockFile struct{ *os.File }

func (vsockFile) LocalAddr() net.Addr  { return vsockAddr{} }
func (vsockFile) RemoteAddr() net.Addr { return vsockAddr{} }

type vsockAddr struct{}

func (vsockAddr) Network() string { return "vsock" }
func (vsockAddr) String() string  { return "host" }

//go:build linux

package linuxvm

import (
	"context"
	"fmt"
	"net"
	"os"
	"time"

	"golang.org/x/sys/unix"

	"github.com/asterism-dev/observatory/internal/node"
)

// activityBackoff keeps a busy VM from costing a read per frame; it delays a suspend by at most this much.
var activityBackoff = 30 * time.Second

// WaitIdle returns nil once no activity frame crossed any of taps for idle.
func WaitIdle(ctx context.Context, taps []string, idle time.Duration) error {
	for {
		files, err := openAll(taps, packetSocket)
		if err != nil {
			return err
		}
		active, err := waitActivity(ctx, files, idle)
		if err != nil || !active {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(activityBackoff):
		}
	}
}

// WaitWake recreates f's taps and reads them until an activity frame arrives; the taps stay for Restore, which needs them closed here first.
func WaitWake(ctx context.Context, f node.VMFile, owner int) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := SetupTaps(f, owner); err != nil {
		RemoveTaps(f)
		return fmt.Errorf("taps: %w", err)
	}
	var taps []string
	for i := 0; i <= len(f.Spec.NICs); i++ {
		taps = append(taps, TapName(f.Spec.ID, i))
	}
	files, err := openAll(taps, openTap)
	if err == nil {
		_, err = waitActivity(ctx, files, -1)
	}
	if err != nil {
		RemoveTaps(f)
	}
	return err
}

// waitActivity reads files until an activity frame (true), idle passing (false; never for idle < 0) or ctx ending; it closes files.
func waitActivity(ctx context.Context, files []*os.File, idle time.Duration) (bool, error) {
	defer closeAll(files)
	active := make(chan struct{}, 1)
	for _, f := range files {
		go func() {
			buf := make([]byte, 65536)
			for {
				n, err := f.Read(buf)
				if err != nil {
					return
				}
				if node.IsActivity(buf[:n]) {
					select {
					case active <- struct{}{}:
					default:
					}
					return
				}
			}
		}()
	}
	var timeout <-chan time.Time
	if idle >= 0 {
		t := time.NewTimer(idle)
		defer t.Stop()
		timeout = t.C
	}
	select {
	case <-ctx.Done():
		return false, ctx.Err()
	case <-timeout:
		return false, nil
	case <-active:
		return true, nil
	}
}

func openAll(names []string, open func(string) (*os.File, error)) ([]*os.File, error) {
	var files []*os.File
	for _, n := range names {
		f, err := open(n)
		if err != nil {
			closeAll(files)
			return nil, fmt.Errorf("%s: %w", n, err)
		}
		files = append(files, f)
	}
	return files, nil
}

// closeAll returns once the fds are closed: Close of a pollable file waits for blocked reads to let go.
func closeAll(files []*os.File) {
	for _, f := range files {
		f.Close()
	}
}

func packetSocket(name string) (*os.File, error) {
	ifi, err := net.InterfaceByName(name)
	if err != nil {
		return nil, err
	}
	// Protocol 0 receives nothing until bind, so no other interface's frames get queued.
	fd, err := unix.Socket(unix.AF_PACKET, unix.SOCK_RAW|unix.SOCK_NONBLOCK|unix.SOCK_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	all := uint16(unix.ETH_P_ALL)<<8 | uint16(unix.ETH_P_ALL)>>8
	if err := unix.Bind(fd, &unix.SockaddrLinklayer{Protocol: all, Ifindex: ifi.Index}); err != nil {
		unix.Close(fd)
		return nil, err
	}
	return os.NewFile(uintptr(fd), "packet:"+name), nil
}

// openTap attaches to tap name's single queue, as Firecracker does; only one fd may hold it.
func openTap(name string) (*os.File, error) {
	fd, err := unix.Open("/dev/net/tun", unix.O_RDWR|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	ifr, err := unix.NewIfreq(name)
	if err == nil {
		ifr.SetUint16(unix.IFF_TAP | unix.IFF_NO_PI)
		err = unix.IoctlIfreq(fd, unix.TUNSETIFF, ifr)
	}
	if err != nil {
		unix.Close(fd)
		return nil, err
	}
	return os.NewFile(uintptr(fd), "tap:"+name), nil
}

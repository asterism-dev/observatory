package guestinit

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/vishvananda/netlink"
	"golang.org/x/sys/unix"

	"github.com/asterism-dev/observatory/internal/proto"
)

const rndReseedCRNG = 0x5207

var virtioRE = regexp.MustCompile(`^virtio(\d+)$`)

func newAgent(root Root) *Agent {
	return &Agent{
		Root:    root,
		Seed:    readCurrentSeed,
		SetMACs: setMACs,
		SetClock: func(ns int64) error {
			return unix.ClockSettime(unix.CLOCK_REALTIME, &unix.Timespec{Sec: ns / 1e9, Nsec: ns % 1e9})
		},
		ReseedRNG:     reseedRNG,
		SetHostname:   func(name string) error { return unix.Sethostname([]byte(name)) },
		ConfigureNICs: ConfigureNICs,
		GrowRoot:      func() error { return growFS(rootDevice, root.Path) },
		MountDisks:    MountDisks,
	}
}

// readCurrentSeed drops the block cache first: the node rewrites the seed ISO while the guest may still hold the old blocks.
func readCurrentSeed() (proto.GuestSeed, error) {
	dev, err := FindSeedDevice("/sys/block")
	if err != nil {
		return proto.GuestSeed{}, err
	}
	f, err := os.Open(dev)
	if err != nil {
		return proto.GuestSeed{}, err
	}
	err = unix.IoctlSetInt(int(f.Fd()), unix.BLKFLSBUF, 0)
	f.Close()
	if err != nil {
		return proto.GuestSeed{}, fmt.Errorf("flush %s: %w", dev, err)
	}
	return ReadSeed(dev)
}

// virtioNICs lists NIC names in virtio index order, which follows the order the node attached them in.
func virtioNICs() ([]string, error) {
	entries, err := os.ReadDir("/sys/class/net")
	if err != nil {
		return nil, err
	}
	type nic struct {
		index int
		name  string
	}
	var found []nic
	for _, e := range entries {
		dev, err := filepath.EvalSymlinks("/sys/class/net/" + e.Name() + "/device")
		if err != nil {
			continue
		}
		if m := virtioRE.FindStringSubmatch(filepath.Base(dev)); m != nil {
			i, _ := strconv.Atoi(m[1])
			found = append(found, nic{i, e.Name()})
		}
	}
	slices.SortFunc(found, func(a, b nic) int { return a.index - b.index })
	names := make([]string, len(found))
	for i, n := range found {
		names[i] = n.name
	}
	return names, nil
}

func setMACs(macs []string) error {
	names, err := virtioNICs()
	if err != nil {
		return err
	}
	if len(macs) > 0 && len(macs) != len(names) {
		return fmt.Errorf("got %d macs for %d nics", len(macs), len(names))
	}
	for i, mac := range macs {
		hw, err := parseMAC(mac)
		if err != nil {
			return err
		}
		link, err := netlink.LinkByName(names[i])
		if err != nil {
			return err
		}
		if err := netlink.LinkSetDown(link); err != nil {
			return err
		}
		if err := netlink.LinkSetHardwareAddr(link, hw); err != nil {
			return fmt.Errorf("%s: %w", names[i], err)
		}
		if err := netlink.LinkSetUp(link); err != nil {
			return err
		}
	}
	return nil
}

// reseedRNG mixes host entropy into the CRNG and makes the kernel rekey, as a clone starts with its template's state.
func reseedRNG(entropy []byte) error {
	f, err := os.OpenFile("/dev/urandom", os.O_WRONLY, 0)
	if err != nil {
		return err
	}
	defer f.Close()
	if _, err := f.Write(entropy); err != nil {
		return err
	}
	return unix.IoctlSetInt(int(f.Fd()), rndReseedCRNG, 0)
}

func parseMAC(s string) ([]byte, error) {
	var hw []byte
	for _, p := range strings.Split(s, ":") {
		b, err := strconv.ParseUint(p, 16, 8)
		if err != nil {
			return nil, fmt.Errorf("bad mac %q", s)
		}
		hw = append(hw, byte(b))
	}
	return hw, nil
}

// listen serves the agent on vsock until the listener fails.
func (a *Agent) listen() error {
	fd, err := unix.Socket(unix.AF_VSOCK, unix.SOCK_STREAM|unix.SOCK_CLOEXEC, 0)
	if err != nil {
		return err
	}
	defer unix.Close(fd)
	if err := unix.Bind(fd, &unix.SockaddrVM{CID: unix.VMADDR_CID_ANY, Port: agentPort}); err != nil {
		return err
	}
	if err := unix.Listen(fd, 8); err != nil {
		return err
	}
	return a.ServeAgent(func() (agentConn, bool, error) {
		for {
			nfd, sa, err := unix.Accept4(fd, unix.SOCK_NONBLOCK|unix.SOCK_CLOEXEC)
			if err == unix.EINTR || err == unix.ECONNABORTED {
				continue
			}
			if err != nil {
				return nil, false, err
			}
			vm, _ := sa.(*unix.SockaddrVM)
			// Nonblocking, so the os.File is pollable and supports read deadlines.
			return os.NewFile(uintptr(nfd), "vsock"), vm != nil && vm.CID == unix.VMADDR_CID_HOST, nil
		}
	})
}

//go:build linux

package linuxvm

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/asterism-dev/observatory/internal/chassisconf"
)

// nbdMu keeps two VMs starting at once from picking the same free device.
var nbdMu sync.Mutex

func NBDConnected(dev string) bool {
	b, err := os.ReadFile(filepath.Join("/sys/block", filepath.Base(dev), "size"))
	return err == nil && strings.TrimSpace(string(b)) != "0"
}

func NBDRecord(vmDir, diskID string) string { return filepath.Join(vmDir, "nbd-"+diskID) }

// AttachNBD connects the export to a free /dev/nbdN, reusing the device recorded for this disk if it is still connected to the same URL (after a node crash).
func AttachNBD(vmDir, diskID, rawURL, psk string) (dev string, err error) {
	host, port, export, err := parseNBDURL(rawURL)
	if err != nil {
		return "", err
	}
	if export != diskID {
		return "", fmt.Errorf("disk %s: NBD URL names export %s, but the key identity is the disk ID", diskID, export)
	}
	// Safe outside nbdMu: a running qemu-nbd loaded its key at start and never rereads the file.
	tlsDir, err := writeNBDKey(vmDir, diskID, psk)
	if err != nil {
		return "", err
	}
	defer func() {
		if err != nil {
			os.RemoveAll(tlsDir)
		}
	}()
	nbdMu.Lock()
	defer nbdMu.Unlock()
	if b, err := os.ReadFile(NBDRecord(vmDir, diskID)); err == nil {
		dev, url := parseNBDRecord(b)
		if NBDConnected(dev) {
			if url == rawURL {
				return dev, nil
			}
			// Connected to an export of an earlier attach, whose port and key are gone.
			if err := stopNBD(dev); err != nil {
				return "", err
			}
		}
	} else if !errors.Is(err, fs.ErrNotExist) {
		return "", err
	}
	devs, err := filepath.Glob("/sys/block/nbd*")
	if err != nil {
		return "", err
	}
	for _, sys := range devs {
		dev := "/dev/" + filepath.Base(sys)
		if NBDConnected(dev) {
			continue
		}
		// Not nbd-client: its -persist does not reconnect, and writes after a storage restart were lost without an error.
		if err := chassisconf.Run("qemu-nbd", qemuNBDArgs(dev, host, port, export, tlsDir)...); err != nil {
			return "", fmt.Errorf("disk %s: %w", diskID, err)
		}
		// qemu-nbd returns once its daemon runs, which is before the kernel has the device.
		for i := 0; !NBDConnected(dev); i++ {
			if i == 50 {
				chassisconf.Run("qemu-nbd", "--disconnect", dev)
				return "", fmt.Errorf("disk %s: %s not connected 5s after qemu-nbd started", diskID, dev)
			}
			time.Sleep(100 * time.Millisecond)
		}
		return dev, os.WriteFile(NBDRecord(vmDir, diskID), []byte(dev+"\n"+rawURL+"\n"), 0o644)
	}
	return "", errors.New("no free /dev/nbd device (is the nbd module loaded?)")
}

func DetachNBD(vmDir, diskID string) error {
	b, err := os.ReadFile(NBDRecord(vmDir, diskID))
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	var stopErr error
	if dev, _ := parseNBDRecord(b); NBDConnected(dev) {
		stopErr = stopNBD(dev)
	}
	// A running qemu-nbd never rereads its key, so the key goes even if stopping failed.
	if err := errors.Join(stopErr, os.RemoveAll(nbdKeyDir(vmDir, diskID))); err != nil {
		return err
	}
	// A forced detach from Kill may race the VMM's own exit path.
	if err := os.Remove(NBDRecord(vmDir, diskID)); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return nil
}

// nbdStopWait bounds the wait after each signal to qemu-nbd.
var nbdStopWait = 10 * time.Second

// stopNBD ends the qemu-nbd serving dev. Not qemu-nbd --disconnect: with the server gone it can hang in D state for the reconnect delay.
func stopNBD(dev string) error {
	pid, err := nbdClientPID(dev)
	if errors.Is(err, fs.ErrNotExist) && !NBDConnected(dev) {
		return nil // disconnected since the caller looked
	}
	if err != nil {
		return fmt.Errorf("%s: client pid: %w", dev, err)
	}
	// SIGTERM closes the export cleanly; a qemu-nbd stuck reconnecting drops the device but keeps running, and after SIGKILL the kernel fails pending I/O.
	for _, sig := range []syscall.Signal{syscall.SIGTERM, syscall.SIGKILL} {
		if err := syscall.Kill(pid, sig); err != nil && !errors.Is(err, syscall.ESRCH) {
			return err
		}
		for deadline := time.Now().Add(nbdStopWait); time.Now().Before(deadline); time.Sleep(100 * time.Millisecond) {
			if !NBDConnected(dev) && errors.Is(syscall.Kill(pid, 0), syscall.ESRCH) {
				return nil
			}
		}
	}
	return fmt.Errorf("%s: qemu-nbd %d still running after SIGKILL", dev, pid)
}

// nbdClientPID returns the process of the thread that holds dev (sysfs names the thread).
func nbdClientPID(dev string) (int, error) {
	b, err := os.ReadFile(filepath.Join("/sys/block", filepath.Base(dev), "pid"))
	if err != nil {
		return 0, err
	}
	status, err := os.ReadFile(filepath.Join("/proc", strings.TrimSpace(string(b)), "status"))
	if err != nil {
		return 0, err
	}
	for line := range strings.Lines(string(status)) {
		if v, ok := strings.CutPrefix(line, "Tgid:"); ok {
			return strconv.Atoi(strings.TrimSpace(v))
		}
	}
	return 0, errors.New("no Tgid")
}

// NBDWatch mirrors lost NBD connections into Err.
type NBDWatch struct {
	errs sync.Map // vmID/diskID -> message of a lost NBD connection
}

// Watch records a lost NBD connection until done closes.
func (w *NBDWatch) Watch(vmID, dir, diskID string, done <-chan struct{}) {
	key := vmID + "/" + diskID
	defer w.errs.Delete(key)
	t := time.NewTicker(2 * time.Second)
	defer t.Stop()
	for {
		b, _ := os.ReadFile(NBDRecord(dir, diskID))
		if dev, _ := parseNBDRecord(b); NBDConnected(dev) {
			w.errs.Delete(key)
		} else {
			w.errs.Store(key, "NBD connection lost")
		}
		select {
		case <-done:
			return
		case <-t.C:
		}
	}
}

func (w *NBDWatch) Err(vmID, diskID string) string {
	e, _ := w.errs.Load(vmID + "/" + diskID)
	msg, _ := e.(string)
	return msg
}

package storage

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"

	"github.com/asterism-dev/observatory/internal/proto"
)

var validPSK = regexp.MustCompile(`^[0-9a-f]{64}$`)

// NBDKitArgs serves one file over TLS-PSK. No --exit-with-parent: its graceful shutdown fails in-flight writes, so exports are only ever SIGKILLed.
func NBDKitArgs(addr string, port int, keyFile, file string) []string {
	return []string{"--foreground", "--ipaddr", addr, "--port", strconv.Itoa(port),
		"--tls=require", "--tls-psk=" + keyFile, "file", "file=" + file}
}

// Exports runs one nbdkit per export; not safe for concurrent use.
type Exports struct {
	Addr, KeyDir string
	Dir          Dir
	// Spawn starts nbdkit; stop must SIGKILL it and return once it has exited, done receives its exit.
	Spawn func(args []string) (stop func(), done <-chan error, err error)

	running map[string]*export
}

type export struct {
	want proto.StorageExport
	stop func()
	done <-chan error
}

func (e *Exports) keyFile(diskID string) string { return filepath.Join(e.KeyDir, diskID+".psk") }

// Apply starts missing exports, kills removed or changed ones and restarts exited ones.
func (e *Exports) Apply(want []proto.StorageExport) []proto.StorageExportStatus {
	if e.running == nil {
		e.running = map[string]*export{}
	}
	wanted := map[string]proto.StorageExport{}
	for _, w := range want {
		wanted[w.DiskID] = w
	}
	exitErr := map[string]error{}
	// Kill first so a port moving between exports is free again.
	for id, r := range e.running {
		exited := false
		select {
		case err := <-r.done:
			exited = true
			if err == nil {
				err = errors.New("exited")
			}
			exitErr[id] = fmt.Errorf("nbdkit: %w", err)
		default:
		}
		if w, ok := wanted[id]; ok && w == r.want && !exited {
			continue
		}
		if !exited {
			r.stop()
		}
		os.Remove(e.keyFile(id))
		delete(e.running, id)
	}

	out := make([]proto.StorageExportStatus, 0, len(want))
	for _, w := range want {
		st := proto.StorageExportStatus{DiskID: w.DiskID, Port: w.Port}
		if _, ok := e.running[w.DiskID]; !ok {
			if err := e.start(w); err != nil {
				st.Error = err.Error()
			} else if err := exitErr[w.DiskID]; err != nil {
				st.Error = err.Error() + " (restarted)"
			}
		}
		out = append(out, st)
	}
	return out
}

func (e *Exports) start(w proto.StorageExport) error {
	if !plainName(w.DiskID) {
		return errors.New("invalid disk id")
	}
	if !validPSK.MatchString(w.PSK) {
		return errors.New("invalid key")
	}
	file := e.Dir.File(w.DiskID)
	if _, err := os.Stat(file); os.IsNotExist(err) {
		return errors.New("file missing")
	} else if err != nil {
		return err
	}
	if err := os.MkdirAll(e.KeyDir, 0o700); err != nil {
		return err
	}
	if err := os.Chmod(e.KeyDir, 0o700); err != nil {
		return err
	}
	key := e.keyFile(w.DiskID)
	os.Remove(key) // a leftover from a crashed run may have other permissions
	if err := os.WriteFile(key, []byte(w.DiskID+":"+w.PSK+"\n"), 0o600); err != nil {
		return err
	}
	stop, done, err := e.Spawn(NBDKitArgs(e.Addr, w.Port, key, file))
	if err != nil {
		os.Remove(key)
		return err
	}
	e.running[w.DiskID] = &export{want: w, stop: stop, done: done}
	return nil
}

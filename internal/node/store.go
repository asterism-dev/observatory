// Package node runs on each VM host: it persists VMs, talks to the hub and drives a Provider.
package node

import (
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"sort"

	"github.com/asterism-dev/observatory/internal/proto"
)

type VMFile struct {
	Spec       proto.VMSpec   `json:"spec"`
	MAC        string         `json:"mac"`
	Power      proto.Power    `json:"power,omitempty"` // empty means running
	StopMode   proto.StopMode `json:"stopMode,omitempty"`
	Generation uint64         `json:"generation,omitempty"`
	// NeedsPrepare is cleared once Prepare succeeds; inverted so vm.json files predating it count as prepared.
	NeedsPrepare bool `json:"needsPrepare,omitempty"`
	// Hibernated is set only once StateDir holds a complete, fsynced memory state.
	Hibernated bool `json:"hibernated,omitempty"`
	// Boot is never written to disk; nil means the hub has not pushed it since the node started.
	Boot *proto.Boot `json:"-"`
}

const (
	StateDir  = "state"
	StateFile = "vmstate"
	MemFile   = "mem"
)

func VMDir(vmsDir, id string) string { return filepath.Join(vmsDir, id) }

func SaveVM(vmsDir string, f VMFile) error {
	dir := VMDir(vmsDir, f.Spec.ID)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	b, err := json.MarshalIndent(f, "", "  ")
	if err != nil {
		return err
	}
	tmp := filepath.Join(dir, "vm.json.tmp")
	// WriteFile keeps the mode of an existing file, and vm.json carries export keys.
	if err := os.Remove(tmp); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, filepath.Join(dir, "vm.json"))
}

func LoadVMs(vmsDir string) ([]VMFile, error) {
	entries, err := os.ReadDir(vmsDir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []VMFile
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		b, err := os.ReadFile(filepath.Join(vmsDir, e.Name(), "vm.json"))
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, err
		}
		var f VMFile
		if err := json.Unmarshal(b, &f); err != nil {
			return nil, err
		}
		out = append(out, f)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Spec.ID < out[j].Spec.ID })
	return out, nil
}

func syncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}

func DeleteVMDir(vmsDir, id string) error { return os.RemoveAll(VMDir(vmsDir, id)) }

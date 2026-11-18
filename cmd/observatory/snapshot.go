package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"time"
)

type snapshotJSON struct {
	ID               string    `json:"id"`
	Name             string    `json:"name"`
	Memory           bool      `json:"memory"`
	State            string    `json:"state"`
	Error            string    `json:"error"`
	SizeBytes        int64     `json:"sizeBytes"`
	CreatedAt        time.Time `json:"createdAt"`
	CanRestoreMemory bool      `json:"canRestoreMemory"`
}

func snapshotCommands() []command {
	return []command{
		{"snapshot list", "<vm>", "list the snapshots of a VM", cmdSnapshotList},
		{"snapshot create", "<vm> <name> [--memory] [--wait]", "snapshot a VM (--memory: include RAM)", cmdSnapshotCreate},
		{"snapshot restore", "<vm> <snap> [--memory] [--restart] [--wait]", "restore a VM to a snapshot", cmdSnapshotRestore},
		{"snapshot delete", "<vm> <snap>", "delete a snapshot", cmdSnapshotDelete},
		{"snapshot fork", "<vm> <snap> <new-vm> [--memory] [--wait]", "create a new VM from a snapshot", cmdSnapshotFork},
	}
}

func (c *cli) snapshotList(vm vmJSON) ([]snapshotJSON, json.RawMessage, error) {
	path, err := c.vmPath(vm.ID, "/snapshots")
	if err != nil {
		return nil, nil, err
	}
	var raw json.RawMessage
	if err := c.call(http.MethodGet, path, nil, &raw); err != nil {
		return nil, nil, err
	}
	var l []snapshotJSON
	return l, raw, json.Unmarshal(raw, &l)
}

func (c *cli) findSnapshot(vm vmJSON, key string) (snapshotJSON, error) {
	l, _, err := c.snapshotList(vm)
	if err != nil {
		return snapshotJSON{}, err
	}
	refs := make([]ref, len(l))
	for i, s := range l {
		refs[i] = ref{s.ID, s.Name}
	}
	r, err := pick("snapshot on VM "+vm.Name, key, refs)
	if err != nil {
		return snapshotJSON{}, err
	}
	return l[slices.IndexFunc(l, func(s snapshotJSON) bool { return s.ID == r.ID })], nil
}

func humanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	f, suffix := float64(n), "KMGT"
	i := -1
	for f >= unit && i < len(suffix)-1 {
		f /= unit
		i++
	}
	return fmt.Sprintf("%.1f %ciB", f, suffix[i])
}

func cmdSnapshotList(c *cli, args []string) error {
	pos, err := c.parse(c.flags("snapshot list"), args, 1, 1)
	if err != nil {
		return err
	}
	vm, err := c.findVM(pos[0])
	if err != nil {
		return err
	}
	l, raw, err := c.snapshotList(vm)
	if err != nil {
		return err
	}
	rows := make([][]string, len(l))
	for i, s := range l {
		mem := "no"
		if s.Memory {
			mem = "yes"
		}
		rows[i] = []string{s.Name, mem, s.State, humanBytes(s.SizeBytes), ago(c.now(), s.CreatedAt)}
	}
	return c.emit(raw, []string{"name", "memory", "state", "size", "created"}, rows)
}

func cmdSnapshotCreate(c *cli, args []string) error {
	fs := c.flags("snapshot create")
	memory := fs.Bool("memory", false, "include the VM's memory")
	wait := fs.Bool("wait", false, "wait until the snapshot is ready")
	pos, err := c.parse(fs, args, 2, 2)
	if err != nil {
		return err
	}
	vm, err := c.findVM(pos[0])
	if err != nil {
		return err
	}
	path, err := c.vmPath(vm.ID, "/snapshots")
	if err != nil {
		return err
	}
	var raw json.RawMessage
	err = c.call(http.MethodPost, path, struct {
		Name   string `json:"name"`
		Memory bool   `json:"memory"`
	}{pos[1], *memory}, &raw)
	if err != nil {
		return err
	}
	var sn snapshotJSON
	if err := json.Unmarshal(raw, &sn); err != nil {
		return err
	}
	if !*wait {
		return c.done(raw, "Snapshot %s of VM %s requested.", sn.Name, vm.Name)
	}
	_, err = c.waitFor("snapshot "+sn.Name, path, func(list json.RawMessage, _ int) (bool, error) {
		var l []snapshotJSON
		if err := json.Unmarshal(list, &l); err != nil {
			return false, err
		}
		i := slices.IndexFunc(l, func(s snapshotJSON) bool { return s.ID == sn.ID })
		if i < 0 {
			return false, &exitError{1, fmt.Sprintf("snapshot %s disappeared", sn.Name)}
		}
		c.progress(l[i].State)
		switch l[i].State {
		case "ready":
			return true, nil
		case "error":
			return false, &exitError{1, fmt.Sprintf("snapshot %s failed: %s", sn.Name, l[i].Error)}
		}
		return false, nil
	})
	if err != nil {
		return err
	}
	return c.done(raw, "Snapshot %s of VM %s is ready.", sn.Name, vm.Name)
}

func cmdSnapshotRestore(c *cli, args []string) error {
	fs := c.flags("snapshot restore")
	memory := fs.Bool("memory", false, "restore the memory too")
	restart := fs.Bool("restart", false, "allow restarting the VM")
	wait := fs.Bool("wait", false, "wait until the VM is back")
	pos, err := c.parse(fs, args, 2, 2)
	if err != nil {
		return err
	}
	vm, err := c.findVM(pos[0])
	if err != nil {
		return err
	}
	sn, err := c.findSnapshot(vm, pos[1])
	if err != nil {
		return err
	}
	if err := c.confirm(fmt.Sprintf("Restore VM %s to snapshot %s? Changes since then are lost.", vm.Name, sn.Name)); err != nil {
		return err
	}
	path, err := c.vmPath(vm.ID, "/snapshots/"+sn.ID+"/restore")
	if err != nil {
		return err
	}
	var raw json.RawMessage
	err = c.call(http.MethodPost, path, struct {
		Memory         bool `json:"memory"`
		RestartAllowed bool `json:"restartAllowed"`
	}{*memory, *restart}, &raw)
	if err != nil {
		return err
	}
	if !*wait {
		return c.done(raw, "Restore of VM %s to snapshot %s requested.", vm.Name, sn.Name)
	}
	if raw, err = c.waitVMState(vm.ID, vm.State); err != nil {
		return err
	}
	return c.done(raw, "VM %s restored to snapshot %s.", vm.Name, sn.Name)
}

func cmdSnapshotDelete(c *cli, args []string) error {
	pos, err := c.parse(c.flags("snapshot delete"), args, 2, 2)
	if err != nil {
		return err
	}
	vm, err := c.findVM(pos[0])
	if err != nil {
		return err
	}
	sn, err := c.findSnapshot(vm, pos[1])
	if err != nil {
		return err
	}
	if err := c.confirm(fmt.Sprintf("Delete snapshot %s of VM %s?", sn.Name, vm.Name)); err != nil {
		return err
	}
	path, err := c.vmPath(vm.ID, "/snapshots/"+sn.ID)
	if err != nil {
		return err
	}
	if err := c.call(http.MethodDelete, path, nil, nil); err != nil {
		return err
	}
	return c.done(nil, "Snapshot %s deleted.", sn.Name)
}

func cmdSnapshotFork(c *cli, args []string) error {
	fs := c.flags("snapshot fork")
	memory := fs.Bool("memory", false, "start from the snapshot's memory")
	wait := fs.Bool("wait", false, "wait until the new VM is running")
	pos, err := c.parse(fs, args, 3, 3)
	if err != nil {
		return err
	}
	vm, err := c.findVM(pos[0])
	if err != nil {
		return err
	}
	sn, err := c.findSnapshot(vm, pos[1])
	if err != nil {
		return err
	}
	path, err := c.vmPath(vm.ID, "/snapshots/"+sn.ID+"/fork")
	if err != nil {
		return err
	}
	var raw json.RawMessage
	err = c.call(http.MethodPost, path, struct {
		Name   string `json:"name"`
		Memory bool   `json:"memory"`
	}{pos[2], *memory}, &raw)
	if err != nil {
		return err
	}
	var nv vmJSON
	if err := json.Unmarshal(raw, &nv); err != nil {
		return err
	}
	if !*wait {
		return c.done(raw, "VM %s created from snapshot %s (%s).", nv.Name, sn.Name, nv.State)
	}
	if raw, err = c.waitVMState(nv.ID, "running"); err != nil {
		return err
	}
	return c.done(raw, "VM %s is running.", nv.Name)
}

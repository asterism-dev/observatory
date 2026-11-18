package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"time"
)

// Disk states from internal/disks.State; the others are transient or broken.
const (
	diskReady   = "ready"
	diskMissing = "missing"
)

type diskJSON struct {
	ID    string `json:"id"`
	Name  string `json:"name"`
	Owner struct {
		Kind string `json:"kind"`
		ID   string `json:"id"`
		Name string `json:"name"`
	} `json:"owner"`
	SizeGB         int    `json:"sizeGB"`
	AllocatedBytes int64  `json:"allocatedBytes"`
	State          string `json:"state"`
	ExportError    string `json:"exportError"`
	Attachment     *struct {
		VM        ref    `json:"vm"`
		MountPath string `json:"mountPath"`
	} `json:"attachment"`
	CreatedAt time.Time `json:"createdAt"`
}

type diskList struct {
	Disks         []diskJSON `json:"disks"`
	StorageOnline bool       `json:"storageOnline"`
}

type vmDiskBody struct {
	DiskID    string `json:"diskId"`
	MountPath string `json:"mountPath"`
}

func diskCommands() []command {
	return []command{
		{"disk list", "", "list disks", cmdDiskList},
		{"disk show", "<disk>", "show a disk", cmdDiskShow},
		{"disk create", "<name> --size GB [--team T] [--wait]", "create a disk", cmdDiskCreate},
		{"disk copy", "<disk> <new> [--team T] [--wait]", "copy a disk", cmdDiskCopy},
		{"disk grow", "<disk> --size GB [--restart]", "grow a disk", cmdDiskGrow},
		{"disk release", "<disk>", "force-release a disk from its VM", cmdDiskRelease},
		{"disk delete", "<disk>", "delete a disk", cmdDiskDelete},
		{"disk attach", "<disk> <vm> [--mount PATH] [--restart]", "attach a disk to a VM", cmdDiskAttach},
		{"disk detach", "<disk> <vm> [--restart]", "detach a disk from a VM", cmdDiskDetach},
	}
}

func (c *cli) diskList() (diskList, json.RawMessage, error) {
	var l diskList
	var raw json.RawMessage
	path, err := c.orgPath("/disks")
	if err != nil {
		return l, nil, err
	}
	if err := c.call(http.MethodGet, path, nil, &raw); err != nil {
		return l, nil, err
	}
	return l, raw, json.Unmarshal(raw, &l)
}

func (c *cli) findDisk(key string) (diskJSON, error) {
	l, _, err := c.diskList()
	if err != nil {
		return diskJSON{}, err
	}
	refs := make([]ref, len(l.Disks))
	for i, d := range l.Disks {
		refs[i] = ref{d.ID, d.Name}
	}
	r, err := pick("disk", key, refs)
	if err != nil {
		return diskJSON{}, err
	}
	return l.Disks[slices.IndexFunc(l.Disks, func(d diskJSON) bool { return d.ID == r.ID })], nil
}

func (d diskJSON) ownerLabel() string { return scopeLabel(d.Owner.Kind, d.Owner.Name) }

func (d diskJSON) attached() string {
	if d.Attachment == nil {
		return "–"
	}
	return d.Attachment.VM.Name + ":" + d.Attachment.MountPath
}

func (c *cli) diskPath(id, rest string) (string, error) { return c.orgPath("/disks/" + id + rest) }

func cmdDiskList(c *cli, args []string) error {
	if _, err := c.parse(c.flags("disk list"), args, 0, 0); err != nil {
		return err
	}
	l, raw, err := c.diskList()
	if err != nil {
		return err
	}
	rows := make([][]string, len(l.Disks))
	for i, d := range l.Disks {
		rows[i] = []string{d.Name, fmt.Sprintf("%d GB", d.SizeGB), humanBytes(d.AllocatedBytes), d.ownerLabel(), d.attached(), d.State}
	}
	if err := c.emit(raw, []string{"name", "size", "used", "owner", "attached", "state"}, rows); err != nil {
		return err
	}
	if !l.StorageOnline {
		fmt.Fprintln(c.stderr, "storage offline")
	}
	return nil
}

func cmdDiskShow(c *cli, args []string) error {
	pos, err := c.parse(c.flags("disk show"), args, 1, 1)
	if err != nil {
		return err
	}
	d, err := c.findDisk(pos[0])
	if err != nil {
		return err
	}
	path, err := c.diskPath(d.ID, "")
	if err != nil {
		return err
	}
	var raw json.RawMessage
	if err := c.call(http.MethodGet, path, nil, &raw); err != nil {
		return err
	}
	if err := json.Unmarshal(raw, &d); err != nil {
		return err
	}
	state := d.State
	if d.ExportError != "" {
		state += " (" + d.ExportError + ")"
	}
	return c.emitKV(raw, [][2]string{
		{"ID", d.ID}, {"Name", d.Name}, {"Size", fmt.Sprintf("%d GB", d.SizeGB)}, {"Used", humanBytes(d.AllocatedBytes)},
		{"Owner", d.ownerLabel()}, {"State", state}, {"Attached", d.attached()}, {"Created", ago(c.now(), d.CreatedAt)},
	})
}

func (c *cli) waitDiskReady(id, name string) (json.RawMessage, error) {
	path, err := c.diskPath(id, "")
	if err != nil {
		return nil, err
	}
	return c.waitFor("disk "+name, path, func(raw json.RawMessage, status int) (bool, error) {
		if status == http.StatusNotFound {
			return false, &exitError{1, fmt.Sprintf("disk %s disappeared", name)}
		}
		var d diskJSON
		if err := json.Unmarshal(raw, &d); err != nil {
			return false, err
		}
		c.progress(d.State)
		switch d.State {
		case diskReady:
			return true, nil
		case diskMissing:
			return false, &exitError{1, fmt.Sprintf("disk %s is missing from storage", name)}
		}
		return false, nil
	})
}

type diskOwner struct {
	Kind   string  `json:"kind"`
	TeamID *string `json:"teamId,omitempty"`
}

func (c *cli) diskOwner(team string) (diskOwner, error) {
	_, teamID, err := c.resolveScope(scopeFlags{"private", team})
	if err != nil {
		return diskOwner{}, err
	}
	if teamID != nil {
		return diskOwner{"team", teamID}, nil
	}
	return diskOwner{"user", nil}, nil
}

func (c *cli) diskCreated(raw json.RawMessage, wait bool) error {
	var d diskJSON
	if err := json.Unmarshal(raw, &d); err != nil {
		return err
	}
	if !wait {
		return c.done(raw, "Disk %s created (%s).", d.Name, d.State)
	}
	raw, err := c.waitDiskReady(d.ID, d.Name)
	if err != nil {
		return err
	}
	return c.done(raw, "Disk %s is ready.", d.Name)
}

func cmdDiskCreate(c *cli, args []string) error {
	fs := c.flags("disk create")
	size := fs.Int("size", 0, "size in GB")
	team := fs.String("team", "", "team `NAME|ID` to own the disk (default: you)")
	wait := fs.Bool("wait", false, "wait until the disk is ready")
	pos, err := c.parse(fs, args, 1, 1)
	if err != nil {
		return err
	}
	if *size < 1 {
		return usagef("--size GB is required")
	}
	owner, err := c.diskOwner(*team)
	if err != nil {
		return err
	}
	path, err := c.orgPath("/disks")
	if err != nil {
		return err
	}
	var raw json.RawMessage
	err = c.call(http.MethodPost, path, struct {
		Name   string    `json:"name"`
		Owner  diskOwner `json:"owner"`
		SizeGB int       `json:"sizeGB"`
	}{pos[0], owner, *size}, &raw)
	if err != nil {
		return err
	}
	return c.diskCreated(raw, *wait)
}

func cmdDiskCopy(c *cli, args []string) error {
	fs := c.flags("disk copy")
	team := fs.String("team", "", "team `NAME|ID` to own the copy (default: you)")
	wait := fs.Bool("wait", false, "wait until the copy is ready")
	pos, err := c.parse(fs, args, 2, 2)
	if err != nil {
		return err
	}
	src, err := c.findDisk(pos[0])
	if err != nil {
		return err
	}
	owner, err := c.diskOwner(*team)
	if err != nil {
		return err
	}
	path, err := c.diskPath(src.ID, "/copy")
	if err != nil {
		return err
	}
	var raw json.RawMessage
	err = c.call(http.MethodPost, path, struct {
		Name  string    `json:"name"`
		Owner diskOwner `json:"owner"`
	}{pos[1], owner}, &raw)
	if err != nil {
		return err
	}
	var w struct {
		Warning string `json:"warning"`
	}
	if json.Unmarshal(raw, &w) == nil && w.Warning != "" {
		fmt.Fprintln(c.stderr, "observatory: warning: "+w.Warning)
	}
	return c.diskCreated(raw, *wait)
}

func cmdDiskGrow(c *cli, args []string) error {
	fs := c.flags("disk grow")
	size := fs.Int("size", 0, "new size in GB")
	restart := fs.Bool("restart", false, "allow restarting the attached VM")
	pos, err := c.parse(fs, args, 1, 1)
	if err != nil {
		return err
	}
	if *size < 1 {
		return usagef("--size GB is required")
	}
	d, err := c.findDisk(pos[0])
	if err != nil {
		return err
	}
	path, err := c.diskPath(d.ID, "/grow")
	if err != nil {
		return err
	}
	var raw json.RawMessage
	err = c.call(http.MethodPost, path, struct {
		SizeGB         int  `json:"sizeGB"`
		RestartAllowed bool `json:"restartAllowed"`
	}{*size, *restart}, &raw)
	if err != nil {
		return err
	}
	return c.done(raw, "Disk %s growing to %d GB.", d.Name, *size)
}

func cmdDiskRelease(c *cli, args []string) error {
	pos, err := c.parse(c.flags("disk release"), args, 1, 1)
	if err != nil {
		return err
	}
	d, err := c.findDisk(pos[0])
	if err != nil {
		return err
	}
	if err := c.confirm(fmt.Sprintf("Force-release disk %s from its VM? Unflushed data may be lost.", d.Name)); err != nil {
		return err
	}
	path, err := c.diskPath(d.ID, "/release")
	if err != nil {
		return err
	}
	if err := c.call(http.MethodPost, path, nil, nil); err != nil {
		return err
	}
	return c.done(nil, "Disk %s released.", d.Name)
}

func cmdDiskDelete(c *cli, args []string) error {
	pos, err := c.parse(c.flags("disk delete"), args, 1, 1)
	if err != nil {
		return err
	}
	d, err := c.findDisk(pos[0])
	if err != nil {
		return err
	}
	if err := c.confirm(fmt.Sprintf("Delete disk %s and its data?", d.Name)); err != nil {
		return err
	}
	path, err := c.diskPath(d.ID, "")
	if err != nil {
		return err
	}
	if err := c.call(http.MethodDelete, path, nil, nil); err != nil {
		return err
	}
	return c.done(nil, "Disk %s deleted.", d.Name)
}

func currentVMDisks(vm vmJSON) []vmDiskBody {
	out := make([]vmDiskBody, len(vm.Disks))
	for i, d := range vm.Disks {
		out[i] = vmDiskBody{d.ID, d.MountPath}
	}
	return out
}

func (c *cli) putVMDisks(vm vmJSON, disks []vmDiskBody, restart bool) error {
	path, err := c.vmPath(vm.ID, "/disks")
	if err != nil {
		return err
	}
	return c.call(http.MethodPut, path, struct {
		Disks          []vmDiskBody `json:"disks"`
		RestartAllowed bool         `json:"restartAllowed"`
	}{disks, restart}, nil)
}

func cmdDiskAttach(c *cli, args []string) error {
	fs := c.flags("disk attach")
	mount := fs.String("mount", "", "mount path (default /mnt/<disk>)")
	restart := fs.Bool("restart", false, "allow restarting the VM")
	pos, err := c.parse(fs, args, 2, 2)
	if err != nil {
		return err
	}
	d, err := c.findDisk(pos[0])
	if err != nil {
		return err
	}
	vm, err := c.findVM(pos[1])
	if err != nil {
		return err
	}
	cur := currentVMDisks(vm)
	if slices.ContainsFunc(cur, func(b vmDiskBody) bool { return b.DiskID == d.ID }) {
		return usagef("disk %s is already attached to VM %s", d.Name, vm.Name)
	}
	if *mount == "" {
		*mount = "/mnt/" + d.Name
	}
	if err := c.putVMDisks(vm, append(cur, vmDiskBody{d.ID, *mount}), *restart); err != nil {
		return err
	}
	return c.done(nil, "Disk %s attached to VM %s at %s.", d.Name, vm.Name, *mount)
}

func cmdDiskDetach(c *cli, args []string) error {
	fs := c.flags("disk detach")
	restart := fs.Bool("restart", false, "allow restarting the VM")
	pos, err := c.parse(fs, args, 2, 2)
	if err != nil {
		return err
	}
	d, err := c.findDisk(pos[0])
	if err != nil {
		return err
	}
	vm, err := c.findVM(pos[1])
	if err != nil {
		return err
	}
	cur := currentVMDisks(vm)
	rest := slices.DeleteFunc(slices.Clone(cur), func(b vmDiskBody) bool { return b.DiskID == d.ID })
	if len(rest) == len(cur) {
		return notFoundf("disk %s is not attached to VM %s", d.Name, vm.Name)
	}
	if err := c.putVMDisks(vm, rest, *restart); err != nil {
		return err
	}
	return c.done(nil, "Disk %s detached from VM %s.", d.Name, vm.Name)
}

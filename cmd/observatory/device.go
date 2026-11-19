package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"time"
)

type deviceJSON struct {
	ID            string     `json:"id"`
	Name          string     `json:"name"`
	IP            string     `json:"ip"`
	Owner         ref        `json:"owner"`
	CreatedAt     time.Time  `json:"createdAt"`
	LastHandshake *time.Time `json:"lastHandshake"`
}

func deviceCommands() []command {
	return []command{
		{"device list", "", "list WireGuard devices", cmdDeviceList},
		{"device create", "<name> [--out FILE|-]", "create a device and write its WireGuard config", cmdDeviceCreate},
		{"device delete", "<device>", "delete a device", cmdDeviceDelete},
	}
}

func writeOnce(path string, data []byte) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if errors.Is(err, fs.ErrExist) {
		return fmt.Errorf("%s exists; choose another --out", path)
	}
	if err != nil {
		return err
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

func (c *cli) deviceList() ([]deviceJSON, json.RawMessage, error) {
	path, err := c.orgPath("/devices")
	if err != nil {
		return nil, nil, err
	}
	var raw json.RawMessage
	if err := c.call(http.MethodGet, path, nil, &raw); err != nil {
		return nil, nil, err
	}
	var ds []deviceJSON
	if err := json.Unmarshal(raw, &ds); err != nil {
		return nil, nil, err
	}
	return ds, raw, nil
}

func cmdDeviceList(c *cli, args []string) error {
	if _, err := c.parse(c.flags("device list"), args, 0, 0); err != nil {
		return err
	}
	ds, raw, err := c.deviceList()
	if err != nil {
		return err
	}
	rows := make([][]string, len(ds))
	for i, d := range ds {
		hs := "never"
		if d.LastHandshake != nil {
			hs = ago(c.now(), *d.LastHandshake)
		}
		rows[i] = []string{d.Name, d.IP, d.Owner.Name, hs}
	}
	return c.emit(raw, []string{"name", "ip", "owner", "last handshake"}, rows)
}

func cmdDeviceCreate(c *cli, args []string) error {
	fs := c.flags("device create")
	out := fs.String("out", "", "write the config to `FILE` (default ./<name>.conf), or - for stdout")
	pos, err := c.parse(fs, args, 1, 1)
	if err != nil {
		return err
	}
	name := pos[0]
	path := *out
	if path == "" {
		path = name + ".conf"
	}
	if path != "-" {
		if path, err = filepath.Abs(path); err != nil {
			return err
		}
		if _, err := os.Lstat(path); err == nil {
			return fmt.Errorf("%s exists; choose another --out", path)
		}
	}
	apiPath, err := c.orgPath("/devices")
	if err != nil {
		return err
	}
	var raw json.RawMessage
	if err := c.call(http.MethodPost, apiPath, map[string]string{"name": name}, &raw); err != nil {
		return err
	}
	var resp struct {
		Config string `json:"config"`
	}
	if err := json.Unmarshal(raw, &resp); err != nil {
		return err
	}
	if path == "-" {
		if c.g.json {
			return c.printJSON(raw)
		}
		_, err := fmt.Fprint(c.stdout, resp.Config)
		return err
	}
	if err := writeOnce(path, []byte(resp.Config)); err != nil {
		return fmt.Errorf("device %s was created but its config could not be saved (delete it and retry): %w", name, err)
	}
	var body map[string]json.RawMessage
	if err := json.Unmarshal(raw, &body); err != nil {
		return err
	}
	delete(body, "config")
	stripped, err := json.Marshal(body)
	if err != nil {
		return err
	}
	return c.done(stripped, "Wrote %s (keep it private). Import it with: sudo wg-quick up %s, or in the WireGuard app.", path, path)
}

func cmdDeviceDelete(c *cli, args []string) error {
	pos, err := c.parse(c.flags("device delete"), args, 1, 1)
	if err != nil {
		return err
	}
	ds, _, err := c.deviceList()
	if err != nil {
		return err
	}
	refs := make([]ref, len(ds))
	for i, d := range ds {
		refs[i] = ref{d.ID, d.Name}
	}
	d, err := pick("device", pos[0], refs)
	if err != nil {
		return err
	}
	if err := c.confirm(fmt.Sprintf("Delete device %s?", d.Name)); err != nil {
		return err
	}
	path, err := c.orgPath("/devices/" + d.ID)
	if err != nil {
		return err
	}
	if err := c.call(http.MethodDelete, path, nil, nil); err != nil {
		return err
	}
	return c.done(nil, "Device %s deleted.", d.Name)
}

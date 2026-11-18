package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
)

type networkJSON struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	Subnet  string `json:"subnet"`
	VMCount int    `json:"vmCount"`
}

func networkCommands() []command {
	return []command{
		{"network list", "", "list networks", cmdNetworkList},
		{"network create", "<name> [--subnet CIDR]", "create a network", cmdNetworkCreate},
		{"network delete", "<net>", "delete a network", cmdNetworkDelete},
		{"network attach", "<net> <vm> [--restart]", "attach a VM to a network", cmdNetworkAttach},
		{"network detach", "<net> <vm> [--restart]", "detach a VM from a network", cmdNetworkDetach},
	}
}

func (c *cli) networkList() ([]networkJSON, json.RawMessage, error) {
	var l []networkJSON
	var raw json.RawMessage
	path, err := c.orgPath("/networks")
	if err != nil {
		return nil, nil, err
	}
	if err := c.call(http.MethodGet, path, nil, &raw); err != nil {
		return nil, nil, err
	}
	if err := json.Unmarshal(raw, &l); err != nil {
		return nil, nil, err
	}
	return l, raw, nil
}

func (c *cli) findNetwork(key string) (networkJSON, error) {
	l, _, err := c.networkList()
	if err != nil {
		return networkJSON{}, err
	}
	refs := make([]ref, len(l))
	for i, n := range l {
		refs[i] = ref{n.ID, n.Name}
	}
	r, err := pick("network", key, refs)
	if err != nil {
		return networkJSON{}, err
	}
	return l[slices.IndexFunc(l, func(n networkJSON) bool { return n.ID == r.ID })], nil
}

func cmdNetworkList(c *cli, args []string) error {
	if _, err := c.parse(c.flags("network list"), args, 0, 0); err != nil {
		return err
	}
	l, raw, err := c.networkList()
	if err != nil {
		return err
	}
	rows := make([][]string, len(l))
	for i, n := range l {
		rows[i] = []string{n.Name, n.Subnet, fmt.Sprint(n.VMCount)}
	}
	return c.emit(raw, []string{"name", "subnet", "vms"}, rows)
}

func cmdNetworkCreate(c *cli, args []string) error {
	fs := c.flags("network create")
	subnet := fs.String("subnet", "", "subnet `CIDR` (default: automatic)")
	pos, err := c.parse(fs, args, 1, 1)
	if err != nil {
		return err
	}
	path, err := c.orgPath("/networks")
	if err != nil {
		return err
	}
	var raw json.RawMessage
	err = c.call(http.MethodPost, path, struct {
		Name   string `json:"name"`
		Subnet string `json:"subnet"`
	}{pos[0], *subnet}, &raw)
	if err != nil {
		return err
	}
	var n networkJSON
	if err := json.Unmarshal(raw, &n); err != nil {
		return err
	}
	return c.done(raw, "Network %s created (%s).", n.Name, n.Subnet)
}

func cmdNetworkDelete(c *cli, args []string) error {
	pos, err := c.parse(c.flags("network delete"), args, 1, 1)
	if err != nil {
		return err
	}
	n, err := c.findNetwork(pos[0])
	if err != nil {
		return err
	}
	if err := c.confirm(fmt.Sprintf("Delete network %s?", n.Name)); err != nil {
		return err
	}
	path, err := c.orgPath("/networks/" + n.ID)
	if err != nil {
		return err
	}
	if err := c.call(http.MethodDelete, path, nil, nil); err != nil {
		return err
	}
	return c.done(nil, "Network %s deleted.", n.Name)
}

func currentVMNetworks(vm vmJSON) []string {
	out := make([]string, len(vm.Networks))
	for i, n := range vm.Networks {
		out[i] = n.ID
	}
	return out
}

func (c *cli) putVMNetworks(vm vmJSON, ids []string, restart bool) (json.RawMessage, error) {
	path, err := c.vmPath(vm.ID, "/networks")
	if err != nil {
		return nil, err
	}
	var raw json.RawMessage
	err = c.call(http.MethodPut, path, struct {
		NetworkIDs     []string `json:"networkIds"`
		RestartAllowed bool     `json:"restartAllowed"`
	}{ids, restart}, &raw)
	return raw, err
}

func networkVMArgs(c *cli, path string, args []string) (networkJSON, vmJSON, bool, error) {
	fs := c.flags(path)
	restart := fs.Bool("restart", false, "allow restarting the VM")
	pos, err := c.parse(fs, args, 2, 2)
	if err != nil {
		return networkJSON{}, vmJSON{}, false, err
	}
	n, err := c.findNetwork(pos[0])
	if err != nil {
		return n, vmJSON{}, false, err
	}
	vm, err := c.findVM(pos[1])
	return n, vm, *restart, err
}

func cmdNetworkAttach(c *cli, args []string) error {
	n, vm, restart, err := networkVMArgs(c, "network attach", args)
	if err != nil {
		return err
	}
	cur := currentVMNetworks(vm)
	if slices.Contains(cur, n.ID) {
		return usagef("VM %s is already on network %s", vm.Name, n.Name)
	}
	raw, err := c.putVMNetworks(vm, append(cur, n.ID), restart)
	if err != nil {
		return err
	}
	return c.done(raw, "VM %s attached to network %s.", vm.Name, n.Name)
}

func cmdNetworkDetach(c *cli, args []string) error {
	n, vm, restart, err := networkVMArgs(c, "network detach", args)
	if err != nil {
		return err
	}
	cur := currentVMNetworks(vm)
	rest := slices.DeleteFunc(slices.Clone(cur), func(id string) bool { return id == n.ID })
	if len(rest) == len(cur) {
		return notFoundf("VM %s is not on network %s", vm.Name, n.Name)
	}
	raw, err := c.putVMNetworks(vm, rest, restart)
	if err != nil {
		return err
	}
	return c.done(raw, "VM %s detached from network %s.", vm.Name, n.Name)
}

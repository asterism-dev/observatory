package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"
)

type vmJSON struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	State    string `json:"state"`
	Power    string `json:"power"`
	StopMode string `json:"stopMode"`
	Error    string `json:"error"`
	IP       string `json:"ip"`
	MgmtIP   string `json:"mgmtIP"`
	DNSName  string `json:"dnsName"`
	Image    string `json:"image"`
	Owner    struct {
		Kind, ID, Name string
	} `json:"owner"`
	Node     ref `json:"node"`
	Networks []struct {
		ID      string `json:"id"`
		Name    string `json:"name"`
		IP      string `json:"ip"`
		DNSName string `json:"dnsName"`
	} `json:"networks"`
	CPUs               int       `json:"cpus"`
	MemoryMB           int       `json:"memoryMB"`
	DiskGB             int       `json:"diskGB"`
	AutoSuspendMinutes int       `json:"autoSuspendMinutes"`
	StockImage         bool      `json:"stockImage"`
	CreatedAt          time.Time `json:"createdAt"`
	Disks              []struct {
		ID        string `json:"id"`
		Name      string `json:"name"`
		MountPath string `json:"mountPath"`
		SizeGB    int    `json:"sizeGB"`
		State     string `json:"state"`
		Error     string `json:"error"`
	} `json:"disks"`
}

type vmList struct {
	VMs   []vmJSON `json:"vms"`
	Nodes []ref    `json:"nodes"`
}

func vmCommands() []command {
	power := func(path, summary, target string) command {
		return command{path, "<vm> [--wait]", summary, cmdVMPower(path, target)}
	}
	return []command{
		{"vm list", "", "list VMs", cmdVMListCmd},
		{"vm show", "<vm>", "show a VM", cmdVMShow},
		{"vm create", "<name> [flags]", "create a VM", cmdVMCreate},
		power("vm start", "start a stopped VM", "running"),
		power("vm resume", "resume a paused or hibernated VM", "running"),
		power("vm pause", "pause a running VM", "paused"),
		power("vm hibernate", "hibernate a running VM", "hibernated"),
		{"vm stop", "<vm> [--force] [--wait]", "shut a VM down (--force: power off)", cmdVMStop},
		{"vm restart", "<vm> [--force] [--wait]", "stop, then start a VM", cmdVMRestart},
		{"vm delete", "<vm> [--wait]", "delete a VM", cmdVMDelete},
		{"vm auto-suspend", "<vm> <minutes|off>", "suspend an idle VM after N minutes (5-1440)", cmdVMAutoSuspend},
		{"vm metrics", "<vm>", "show recent CPU and memory samples", cmdVMMetrics},
		{"vm egress", "<vm>", "show egress proxy hosts and CA of a VM", cmdVMEgress},
		{"vm integrations", "<vm>", "show the integrations a VM gets", cmdVMIntegrations},
	}
}

func (c *cli) vmList() (vmList, json.RawMessage, error) {
	var l vmList
	var raw json.RawMessage
	path, err := c.orgPath("/vms")
	if err != nil {
		return l, nil, err
	}
	if err := c.call(http.MethodGet, path, nil, &raw); err != nil {
		return l, nil, err
	}
	if err := json.Unmarshal(raw, &l); err != nil {
		return l, nil, err
	}
	return l, raw, nil
}

func (c *cli) findVM(key string) (vmJSON, error) {
	l, _, err := c.vmList()
	if err != nil {
		return vmJSON{}, err
	}
	refs := make([]ref, len(l.VMs))
	for i, v := range l.VMs {
		refs[i] = ref{v.ID, v.Name}
	}
	r, err := pick("vm", key, refs)
	if err != nil {
		return vmJSON{}, err
	}
	i := slices.IndexFunc(l.VMs, func(v vmJSON) bool { return v.ID == r.ID })
	return l.VMs[i], nil
}

func (c *cli) vmPath(id, rest string) (string, error) { return c.orgPath("/vms/" + id + rest) }

func (c *cli) waitVMState(id string, want ...string) (json.RawMessage, error) {
	path, err := c.vmPath(id, "")
	if err != nil {
		return nil, err
	}
	return c.waitFor("vm "+id, path, func(raw json.RawMessage, status int) (bool, error) {
		var v vmJSON
		if err := json.Unmarshal(raw, &v); err != nil {
			return false, err
		}
		c.progress(v.State)
		if slices.Contains(want, v.State) {
			return true, nil
		}
		if v.State == "error" {
			return false, &exitError{1, fmt.Sprintf("vm %s failed: %s", v.Name, v.Error)}
		}
		return false, nil
	})
}

func parseMemory(s string) (int, error) {
	t := strings.ToUpper(strings.TrimSpace(s))
	mult := 1
	for _, u := range []struct {
		suffix string
		mult   int
	}{{"GB", 1024}, {"G", 1024}, {"MB", 1}, {"M", 1}} {
		if strings.HasSuffix(t, u.suffix) {
			t, mult = strings.TrimSuffix(t, u.suffix), u.mult
			break
		}
	}
	n, err := strconv.Atoi(t)
	if err != nil || n <= 0 {
		return 0, usagef("invalid memory %q: use e.g. 4G, 512M", s)
	}
	return n * mult, nil
}

func (v vmJSON) size() string {
	return fmt.Sprintf("%d vCPU · %s · %d GB", v.CPUs, gb(v.MemoryMB), v.DiskGB)
}

func cmdVMListCmd(c *cli, args []string) error {
	if _, err := c.parse(c.flags("vm list"), args, 0, 0); err != nil {
		return err
	}
	l, raw, err := c.vmList()
	if err != nil {
		return err
	}
	rows := make([][]string, len(l.VMs))
	for i, v := range l.VMs {
		rows[i] = []string{v.Name, v.State, c.ownerLabel(v.Owner.Kind, v.Owner.ID, v.Owner.Name), v.Node.Name, v.size(), orDash(v.IP)}
	}
	return c.emit(raw, []string{"name", "state", "owner", "node", "size", "ip"}, rows)
}

func orDash(s string) string {
	if s == "" {
		return "–"
	}
	return s
}

func cmdVMShow(c *cli, args []string) error {
	pos, err := c.parse(c.flags("vm show"), args, 1, 1)
	if err != nil {
		return err
	}
	v, err := c.findVM(pos[0])
	if err != nil {
		return err
	}
	path, err := c.vmPath(v.ID, "")
	if err != nil {
		return err
	}
	var raw json.RawMessage
	if err := c.call(http.MethodGet, path, nil, &raw); err != nil {
		return err
	}
	if err := json.Unmarshal(raw, &v); err != nil {
		return err
	}
	state := v.State
	if v.Error != "" {
		state += " (" + v.Error + ")"
	}
	var nets, disks []string
	for _, n := range v.Networks {
		nets = append(nets, n.Name+" "+n.IP)
	}
	for _, d := range v.Disks {
		disks = append(disks, fmt.Sprintf("%s → %s (%d GB)", d.Name, d.MountPath, d.SizeGB))
	}
	suspend, image := "off", "–"
	if v.AutoSuspendMinutes > 0 {
		suspend = fmt.Sprintf("%d min", v.AutoSuspendMinutes)
	}
	if v.StockImage {
		image = "stock"
	} else if v.Image != "" {
		image = v.Image
	}
	return c.emitKV(raw, [][2]string{
		{"name", v.Name}, {"state", state}, {"owner", c.ownerLabel(v.Owner.Kind, v.Owner.ID, v.Owner.Name)}, {"node", v.Node.Name},
		{"size", v.size()}, {"dns", v.DNSName}, {"ip", orDash(v.IP)},
		{"networks", orDash(strings.Join(nets, ", "))}, {"disks", orDash(strings.Join(disks, ", "))},
		{"auto-suspend", suspend}, {"image", image}, {"created", ago(c.now(), v.CreatedAt)},
	})
}

func cmdVMCreate(c *cli, args []string) error {
	fs := c.flags("vm create")
	node := fs.String("node", "", "node `NAME|ID` (default: the only node)")
	team := fs.String("team", "", "team `NAME|ID` to own the VM (default: you)")
	cpus := fs.Int("cpus", 2, "vCPUs")
	memory := fs.String("memory", "4G", "memory, e.g. 4G or 512M")
	disk := fs.Int("disk", 20, "disk size in GB")
	wait := fs.Bool("wait", false, "wait until the VM is running")
	pos, err := c.parse(fs, args, 1, 1)
	if err != nil {
		return err
	}
	memMB, err := parseMemory(*memory)
	if err != nil {
		return err
	}
	_, teamID, err := c.resolveScope(scopeFlags{"private", *team})
	if err != nil {
		return err
	}
	l, _, err := c.vmList()
	if err != nil {
		return err
	}
	var nodeID string
	switch {
	case *node != "":
		n, err := pick("node", *node, l.Nodes)
		if err != nil {
			return err
		}
		nodeID = n.ID
	case len(l.Nodes) == 1:
		nodeID = l.Nodes[0].ID
	default:
		names := make([]string, len(l.Nodes))
		for i, n := range l.Nodes {
			names[i] = n.Name
		}
		return usagef("several nodes; pass --node: %s", strings.Join(names, ", "))
	}
	type owner struct {
		Kind   string  `json:"kind"`
		TeamID *string `json:"teamId,omitempty"`
	}
	o := owner{"user", nil}
	if teamID != nil {
		o = owner{"team", teamID}
	}
	path, err := c.orgPath("/vms")
	if err != nil {
		return err
	}
	var raw json.RawMessage
	err = c.call(http.MethodPost, path, struct {
		Name     string `json:"name"`
		Owner    owner  `json:"owner"`
		NodeID   string `json:"nodeId"`
		CPUs     int    `json:"cpus"`
		MemoryMB int    `json:"memoryMB"`
		DiskGB   int    `json:"diskGB"`
	}{pos[0], o, nodeID, *cpus, memMB, *disk}, &raw)
	if err != nil {
		return err
	}
	var v vmJSON
	if err := json.Unmarshal(raw, &v); err != nil {
		return err
	}
	if !*wait {
		return c.done(raw, "VM %s created (%s).", v.Name, v.State)
	}
	if raw, err = c.waitVMState(v.ID, "running"); err != nil {
		return err
	}
	return c.done(raw, "VM %s is running.", v.Name)
}

func (c *cli) vmPost(v vmJSON, action string) (json.RawMessage, error) {
	path, err := c.vmPath(v.ID, "/"+action)
	if err != nil {
		return nil, err
	}
	var raw json.RawMessage
	if err := c.call(http.MethodPost, path, nil, &raw); err != nil {
		return nil, err
	}
	return raw, nil
}

func (c *cli) powered(raw json.RawMessage, v vmJSON, verb, target string, wait bool) error {
	if !wait {
		return c.done(raw, "VM %s: %s requested.", v.Name, verb)
	}
	raw, err := c.waitVMState(v.ID, target)
	if err != nil {
		return err
	}
	return c.done(raw, "VM %s is %s.", v.Name, target)
}

func cmdVMPower(path, target string) func(*cli, []string) error {
	verb := strings.TrimPrefix(path, "vm ")
	return func(c *cli, args []string) error {
		fs := c.flags(path)
		wait := fs.Bool("wait", false, "wait until the VM is "+target)
		pos, err := c.parse(fs, args, 1, 1)
		if err != nil {
			return err
		}
		v, err := c.findVM(pos[0])
		if err != nil {
			return err
		}
		raw, err := c.vmPost(v, verb)
		if err != nil {
			return err
		}
		return c.powered(raw, v, verb, target, *wait)
	}
}

func cmdVMStop(c *cli, args []string) error {
	fs := c.flags("vm stop")
	force := fs.Bool("force", false, "power off instead of a graceful shutdown")
	wait := fs.Bool("wait", false, "wait until the VM is stopped")
	pos, err := c.parse(fs, args, 1, 1)
	if err != nil {
		return err
	}
	v, err := c.findVM(pos[0])
	if err != nil {
		return err
	}
	raw, err := c.vmPost(v, stopAction(*force))
	if err != nil {
		return err
	}
	return c.powered(raw, v, "stop", "stopped", *wait)
}

func stopAction(force bool) string {
	if force {
		return "stop"
	}
	return "shutdown"
}

func cmdVMRestart(c *cli, args []string) error {
	fs := c.flags("vm restart")
	force := fs.Bool("force", false, "power off instead of a graceful shutdown")
	wait := fs.Bool("wait", false, "wait until the VM is running again")
	pos, err := c.parse(fs, args, 1, 1)
	if err != nil {
		return err
	}
	v, err := c.findVM(pos[0])
	if err != nil {
		return err
	}
	if v.State != "stopped" {
		if _, err := c.vmPost(v, stopAction(*force)); err != nil {
			return err
		}
		if _, err := c.waitVMState(v.ID, "stopped"); err != nil {
			return err
		}
	}
	raw, err := c.vmPost(v, "start")
	if err != nil {
		return err
	}
	return c.powered(raw, v, "restart", "running", *wait)
}

func cmdVMDelete(c *cli, args []string) error {
	fs := c.flags("vm delete")
	wait := fs.Bool("wait", false, "wait until the VM is gone")
	pos, err := c.parse(fs, args, 1, 1)
	if err != nil {
		return err
	}
	v, err := c.findVM(pos[0])
	if err != nil {
		return err
	}
	if err := c.confirm(fmt.Sprintf("Delete VM %s and its data?", v.Name)); err != nil {
		return err
	}
	path, err := c.vmPath(v.ID, "")
	if err != nil {
		return err
	}
	if err := c.call(http.MethodDelete, path, nil, nil); err != nil {
		return err
	}
	if *wait {
		_, err := c.waitFor("vm "+v.ID, path, func(_ json.RawMessage, status int) (bool, error) {
			return status == http.StatusNotFound, nil
		})
		if err != nil {
			return err
		}
	}
	return c.done(nil, "VM %s deleted.", v.Name)
}

func cmdVMAutoSuspend(c *cli, args []string) error {
	pos, err := c.parse(c.flags("vm auto-suspend"), args, 2, 2)
	if err != nil {
		return err
	}
	minutes := 0
	if pos[1] != "off" {
		minutes, err = strconv.Atoi(pos[1])
		if err != nil || minutes < 5 || minutes > 1440 {
			return usagef("minutes must be off or between 5 and 1440")
		}
	}
	v, err := c.findVM(pos[0])
	if err != nil {
		return err
	}
	path, err := c.vmPath(v.ID, "/auto-suspend")
	if err != nil {
		return err
	}
	var raw json.RawMessage
	if err := c.call(http.MethodPut, path, map[string]int{"minutes": minutes}, &raw); err != nil {
		return err
	}
	if minutes == 0 {
		return c.done(raw, "Auto-suspend of VM %s is off.", v.Name)
	}
	return c.done(raw, "VM %s suspends after %d min idle.", v.Name, minutes)
}

func (c *cli) vmGet(name, rest string, into any) (vmJSON, json.RawMessage, error) {
	v, err := c.findVM(name)
	if err != nil {
		return v, nil, err
	}
	path, err := c.vmPath(v.ID, rest)
	if err != nil {
		return v, nil, err
	}
	var raw json.RawMessage
	if err := c.call(http.MethodGet, path, nil, &raw); err != nil {
		return v, nil, err
	}
	return v, raw, json.Unmarshal(raw, into)
}

func cmdVMMetrics(c *cli, args []string) error {
	pos, err := c.parse(c.flags("vm metrics"), args, 1, 1)
	if err != nil {
		return err
	}
	var m struct {
		Samples []struct {
			At         time.Time `json:"at"`
			CPUPercent float64   `json:"cpuPercent"`
			MemTotalMB int       `json:"memTotalMB"`
			MemUsedMB  int       `json:"memUsedMB"`
		} `json:"samples"`
	}
	_, raw, err := c.vmGet(pos[0], "/metrics", &m)
	if err != nil {
		return err
	}
	rows := make([][]string, len(m.Samples))
	for i, s := range m.Samples {
		rows[i] = []string{s.At.In(c.now().Location()).Format("15:04:05"), fmt.Sprintf("%.0f%%", s.CPUPercent), gb(s.MemUsedMB) + " / " + gb(s.MemTotalMB)}
	}
	return c.emit(raw, []string{"time", "cpu", "memory"}, rows)
}

func cmdVMEgress(c *cli, args []string) error {
	pos, err := c.parse(c.flags("vm egress"), args, 1, 1)
	if err != nil {
		return err
	}
	var e struct {
		Secrets      int        `json:"secrets"`
		Hosts        []string   `json:"hosts"`
		CANotAfter   *time.Time `json:"caNotAfter"`
		TrustChanged bool       `json:"trustChanged"`
	}
	_, raw, err := c.vmGet(pos[0], "/egress", &e)
	if err != nil {
		return err
	}
	ca := "–"
	if e.CANotAfter != nil {
		ca = e.CANotAfter.Format("2006-01-02 15:04")
	}
	trust := "no"
	if e.TrustChanged {
		trust = "yes (restart the VM's processes to pick up the new hosts)"
	}
	return c.emitKV(raw, [][2]string{
		{"secrets", strconv.Itoa(e.Secrets)}, {"hosts", orDash(strings.Join(e.Hosts, ", "))},
		{"ca expires", ca}, {"trust changed", trust},
	})
}

func cmdVMIntegrations(c *cli, args []string) error {
	pos, err := c.parse(c.flags("vm integrations"), args, 1, 1)
	if err != nil {
		return err
	}
	var l struct {
		Integrations []struct {
			Kind     string `json:"kind"`
			Scope    string `json:"scope"`
			TeamName string `json:"teamName"`
			Mode     string `json:"mode"`
		} `json:"integrations"`
	}
	_, raw, err := c.vmGet(pos[0], "/integrations", &l)
	if err != nil {
		return err
	}
	rows := make([][]string, len(l.Integrations))
	for i, g := range l.Integrations {
		scope := "–"
		if g.Scope != "" {
			scope = scopeLabel(g.Scope, g.TeamName)
		}
		rows[i] = []string{g.Kind, scope, orDash(g.Mode)}
	}
	return c.emit(raw, []string{"kind", "scope", "mode"}, rows)
}

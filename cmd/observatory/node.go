package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"time"
)

type nodeJSON struct {
	ID           string     `json:"id"`
	Name         string     `json:"name"`
	State        string     `json:"state"`
	Version      string     `json:"version"`
	CPUs         int        `json:"cpus"`
	MemoryMB     int        `json:"memoryMB"`
	VMCount      int        `json:"vmCount"`
	UsedCPUs     int        `json:"usedCPUs"`
	UsedMemoryMB int        `json:"usedMemoryMB"`
	LastSeen     *time.Time `json:"lastSeen"`
	Quarantined  bool       `json:"quarantined"`
	Image        *struct {
		Version string     `json:"version"`
		State   string     `json:"state"`
		Error   string     `json:"error"`
		RetryAt *time.Time `json:"retryAt"`
	} `json:"image"`
}

func nodeCommands() []command {
	return []command{
		{"node list", "", "list nodes", cmdNodeList},
		{"node show", "<node>", "show a node", cmdNodeShow},
		{"node add", "<name>", "register a node and print its install command", cmdNodeAdd},
		{"node token", "<node>", "issue a new install command (revokes the current credential)", cmdNodeToken},
		{"node rename", "<node> <name>", "rename a node", cmdNodeRename},
		{"node delete", "<node>", "delete a node", cmdNodeDelete},
	}
}

func (c *cli) findNode(key string) (nodeJSON, error) {
	l, _, err := getList[nodeJSON](c, "/nodes")
	if err != nil {
		return nodeJSON{}, err
	}
	return findBy("node", key, l, func(n nodeJSON) ref { return ref{n.ID, n.Name} })
}

func (c *cli) nodeImage(n nodeJSON) string {
	if n.Image == nil {
		return "-"
	}
	s := n.Image.State
	if n.Image.Version != "" {
		s += " " + n.Image.Version
	}
	if n.Image.RetryAt != nil {
		s += ", retry " + n.Image.RetryAt.In(c.now().Location()).Format("15:04")
	}
	return s
}

func (c *cli) lastSeen(n nodeJSON) string {
	if n.LastSeen == nil {
		return "-"
	}
	return ago(c.now(), *n.LastSeen)
}

func nodeState(n nodeJSON) string {
	if n.Quarantined {
		return n.State + " (quarantined)"
	}
	return n.State
}

func cmdNodeList(c *cli, args []string) error {
	if _, err := c.parse(c.flags("node list"), args, 0, 0); err != nil {
		return err
	}
	l, raw, err := getList[nodeJSON](c, "/nodes")
	if err != nil {
		return err
	}
	rows := make([][]string, len(l))
	for i, n := range l {
		rows[i] = []string{n.Name, nodeState(n), fmt.Sprint(n.VMCount),
			fmt.Sprintf("%d/%d", n.UsedCPUs, n.CPUs), gb(n.UsedMemoryMB) + "/" + gb(n.MemoryMB), c.nodeImage(n), c.lastSeen(n)}
	}
	return c.emit(raw, []string{"name", "state", "vms", "cpu", "ram", "image", "last seen"}, rows)
}

func cmdNodeShow(c *cli, args []string) error {
	pos, err := c.parse(c.flags("node show"), args, 1, 1)
	if err != nil {
		return err
	}
	n, err := c.findNode(pos[0])
	if err != nil {
		return err
	}
	path, err := c.orgPath("/nodes/" + n.ID)
	if err != nil {
		return err
	}
	var raw json.RawMessage
	if err := c.call(http.MethodGet, path, nil, &raw); err != nil {
		return err
	}
	if err := json.Unmarshal(raw, &n); err != nil {
		return err
	}
	pairs := [][2]string{{"name", n.Name}, {"id", n.ID}, {"state", nodeState(n)}, {"version", n.Version},
		{"cpu", fmt.Sprintf("%d/%d used", n.UsedCPUs, n.CPUs)}, {"ram", gb(n.UsedMemoryMB) + "/" + gb(n.MemoryMB) + " used"},
		{"vms", fmt.Sprint(n.VMCount)}, {"last seen", c.lastSeen(n)}, {"image", c.nodeImage(n)}}
	if n.Image != nil && n.Image.Error != "" {
		pairs = append(pairs, [2]string{"image error", n.Image.Error})
	}
	return c.emitKV(raw, pairs)
}

func cmdNodeAdd(c *cli, args []string) error {
	pos, err := c.parse(c.flags("node add"), args, 1, 1)
	if err != nil {
		return err
	}
	path, err := c.orgPath("/nodes")
	if err != nil {
		return err
	}
	var raw json.RawMessage
	if err := c.call(http.MethodPost, path, map[string]string{"name": pos[0]}, &raw); err != nil {
		return err
	}
	var res struct {
		InstallCommand string `json:"installCommand"`
	}
	if err := json.Unmarshal(raw, &res); err != nil {
		return err
	}
	return c.done(raw, "%s", res.InstallCommand)
}

func cmdNodeToken(c *cli, args []string) error {
	pos, err := c.parse(c.flags("node token"), args, 1, 1)
	if err != nil {
		return err
	}
	n, err := c.findNode(pos[0])
	if err != nil {
		return err
	}
	if n.State != "pending" {
		if err := c.confirm(fmt.Sprintf("Issue a new credential for node %s? The node disconnects until reinstalled.", n.Name)); err != nil {
			return err
		}
	}
	path, err := c.orgPath("/nodes/" + n.ID + "/credential")
	if err != nil {
		return err
	}
	var raw json.RawMessage
	if err := c.call(http.MethodPost, path, nil, &raw); err != nil {
		return err
	}
	var res struct {
		InstallCommand string `json:"installCommand"`
	}
	if err := json.Unmarshal(raw, &res); err != nil {
		return err
	}
	return c.done(raw, "%s", res.InstallCommand)
}

func cmdNodeRename(c *cli, args []string) error {
	pos, err := c.parse(c.flags("node rename"), args, 2, 2)
	if err != nil {
		return err
	}
	n, err := c.findNode(pos[0])
	if err != nil {
		return err
	}
	path, err := c.orgPath("/nodes/" + n.ID)
	if err != nil {
		return err
	}
	var raw json.RawMessage
	if err := c.call(http.MethodPatch, path, map[string]string{"name": pos[1]}, &raw); err != nil {
		return err
	}
	return c.done(raw, "Node %s renamed to %s.", n.Name, pos[1])
}

func cmdNodeDelete(c *cli, args []string) error {
	pos, err := c.parse(c.flags("node delete"), args, 1, 1)
	if err != nil {
		return err
	}
	n, err := c.findNode(pos[0])
	if err != nil {
		return err
	}
	if err := c.confirm(fmt.Sprintf("Delete node %s?", n.Name)); err != nil {
		return err
	}
	path, err := c.orgPath("/nodes/" + n.ID)
	if err != nil {
		return err
	}
	if err := c.call(http.MethodDelete, path, nil, nil); err != nil {
		return err
	}
	return c.done(nil, "Node %s deleted.", n.Name)
}

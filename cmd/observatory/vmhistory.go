package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"time"
)

type vmHistoryJSON struct {
	Name  string `json:"name"`
	Owner struct {
		Kind, ID, Name string
	} `json:"owner"`
	CPUs      int       `json:"cpus"`
	MemoryMB  int       `json:"memoryMB"`
	DiskGB    int       `json:"diskGB"`
	CreatedAt time.Time `json:"createdAt"`
	CreatedBy *ref      `json:"createdBy"`
	DeletedAt time.Time `json:"deletedAt"`
	DeletedBy *ref      `json:"deletedBy"`
	Reason    string    `json:"reason"`
}

func (c *cli) vmListDeleted() error {
	path, err := c.orgPath("/vms/history")
	if err != nil {
		return err
	}
	var raw json.RawMessage
	if err := c.call(http.MethodGet, path, nil, &raw); err != nil {
		return err
	}
	var h struct {
		VMs []vmHistoryJSON `json:"vms"`
	}
	if err := json.Unmarshal(raw, &h); err != nil {
		return err
	}
	when := func(t time.Time) string { return t.In(c.now().Location()).Format("2006-01-02 15:04") }
	rows := make([][]string, len(h.VMs))
	for i, v := range h.VMs {
		by := "expired"
		if v.Reason != "expired" {
			by = userName(v.DeletedBy)
		}
		size := vmJSON{CPUs: v.CPUs, MemoryMB: v.MemoryMB, DiskGB: v.DiskGB}.size()
		rows[i] = []string{v.Name, c.ownerLabel(v.Owner.Kind, v.Owner.ID, v.Owner.Name), size,
			when(v.CreatedAt), userName(v.CreatedBy), when(v.DeletedAt), by, lifetime(v.DeletedAt.Sub(v.CreatedAt))}
	}
	return c.emit(raw, []string{"name", "owner", "size", "created", "by", "deleted", "by", "lifetime"}, rows)
}

func userName(r *ref) string {
	if r == nil || r.Name == "" {
		return "unknown"
	}
	return r.Name
}

// lifetime shows the two largest units: 1d 0h, 2h 37m, 12m.
func lifetime(d time.Duration) string {
	m := int(d / time.Minute)
	switch {
	case m >= 24*60:
		return fmt.Sprintf("%dd %dh", m/(24*60), m%(24*60)/60)
	case m >= 60:
		return fmt.Sprintf("%dh %dm", m/60, m%60)
	}
	return fmt.Sprintf("%dm", m)
}

func (c *cli) deleteAtLabel(at *time.Time) string {
	if at == nil {
		return "–"
	}
	return fmt.Sprintf("%s (in %s)", at.In(c.now().Location()).Format("2006-01-02 15:04"), lifetime(at.Sub(c.now())))
}

func cmdVMDeleteAfter(c *cli, args []string) error {
	pos, err := c.parse(c.flags("vm delete-after"), args, 2, 2)
	if err != nil {
		return err
	}
	v, err := c.findVM(pos[0])
	if err != nil {
		return err
	}
	path, err := c.vmPath(v.ID, "/delete-after")
	if err != nil {
		return err
	}
	var body struct {
		DeleteAfter *string `json:"deleteAfter"`
	}
	if pos[1] != "off" {
		body.DeleteAfter = &pos[1]
	}
	var raw json.RawMessage
	if err := c.call(http.MethodPut, path, body, &raw); err != nil {
		return err
	}
	if err := json.Unmarshal(raw, &v); err != nil {
		return err
	}
	if v.DeleteAt == nil {
		return c.done(raw, "VM %s is no longer deleted automatically.", v.Name)
	}
	return c.done(raw, "VM %s is deleted at %s.", v.Name, c.deleteAtLabel(v.DeleteAt))
}

package api

import (
	"errors"
	"fmt"
	"net/http"
	"path"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/asterism-dev/observatory/internal/auth"
	"github.com/asterism-dev/observatory/internal/authz"
	"github.com/asterism-dev/observatory/internal/proto"
	"github.com/asterism-dev/observatory/internal/store"
)

var vmNamePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,62}$`)

type refJSON struct {
	ID   uuid.UUID `json:"id"`
	Name string    `json:"name"`
}

type ownerJSON struct {
	Kind string    `json:"kind"`
	ID   uuid.UUID `json:"id"`
	Name string    `json:"name"`
}

type vmJSON struct {
	ID         uuid.UUID      `json:"id"`
	Name       string         `json:"name"`
	Owner      ownerJSON      `json:"owner"`
	Node       refJSON        `json:"node"`
	State      proto.VMState  `json:"state"`
	Power      proto.Power    `json:"power"`
	StopMode   proto.StopMode `json:"stopMode,omitempty"`
	Error      string         `json:"error,omitempty"`
	IP         string         `json:"ip,omitempty"`
	MgmtIP     string         `json:"mgmtIP,omitempty"`
	DNSName    string         `json:"dnsName"`
	Networks   []vmNICJSON    `json:"networks"`
	CPUs       int            `json:"cpus"`
	MemoryMB   int            `json:"memoryMB"`
	DiskGB     int            `json:"diskGB"`
	Image      string         `json:"image"`
	StockImage bool           `json:"stockImage"` // ran without a guest image on a node that has guest images
	CreatedAt  time.Time      `json:"createdAt"`
	CanPower   bool           `json:"canPower"`
	CanDelete  bool           `json:"canDelete"`
	CanNetwork bool           `json:"canNetwork"`
	Disks      []vmDiskJSON   `json:"disks"`
	CanDisks   bool           `json:"canDisks"`

	AutoSuspendMinutes int  `json:"autoSuspendMinutes"`
	CanSnapshot        bool `json:"canSnapshot"`
	CanRestore         bool `json:"canRestore"`
	CanFork            bool `json:"canFork"`
	Restoring          bool `json:"restoring"`

	Usage *metricJSON `json:"usage,omitempty"` // latest sample, so lists need no request per VM
}

type vmDiskJSON struct {
	ID        uuid.UUID `json:"id"`
	Name      string    `json:"name"`
	MountPath string    `json:"mountPath"`
	SizeGB    int       `json:"sizeGB"`
	State     string    `json:"state"`
	Error     string    `json:"error,omitempty"`
}

type vmNICJSON struct {
	ID      uuid.UUID `json:"id"`
	Name    string    `json:"name"`
	IP      string    `json:"ip"`
	DNSName string    `json:"dnsName"`
}

// vmNames holds the org's display names for VM owners and nodes.
type vmNames struct {
	users, teams, nodes, networks map[uuid.UUID]string
	nics                          map[uuid.UUID][]store.VMNIC
	vmDisks                       map[uuid.UUID][]store.VMDisk
	disks                         map[uuid.UUID]store.Disk
	teamList                      []store.Team
	nodeList                      []store.Node
}

func (h *api) vmNames(r *http.Request, org uuid.UUID) (vmNames, error) {
	ctx := r.Context()
	n := vmNames{users: map[uuid.UUID]string{}, teams: map[uuid.UUID]string{}, nodes: map[uuid.UUID]string{}, networks: map[uuid.UUID]string{}, nics: map[uuid.UUID][]store.VMNIC{}, disks: map[uuid.UUID]store.Disk{}}
	members, err := h.store.Members(ctx, org)
	if err != nil {
		return n, err
	}
	for _, m := range members {
		n.users[m.User.ID] = m.User.Name
	}
	if n.teamList, err = h.store.Teams(ctx, org); err != nil {
		return n, err
	}
	for _, t := range n.teamList {
		n.teams[t.ID] = t.Name
	}
	if n.nodeList, err = h.store.Nodes(ctx, org); err != nil {
		return n, err
	}
	for _, nd := range n.nodeList {
		n.nodes[nd.ID] = nd.Name
	}
	nets, err := h.store.Networks(ctx, org)
	if err != nil {
		return n, err
	}
	for _, nw := range nets {
		n.networks[nw.ID] = nw.Name
	}
	nics, err := h.store.OrgVMNetworks(ctx, org)
	if err != nil {
		return n, err
	}
	for _, nic := range nics {
		n.nics[nic.VMID] = append(n.nics[nic.VMID], nic)
	}
	if n.vmDisks, err = h.store.OrgVMDisks(ctx, org); err != nil {
		return n, err
	}
	ds, err := h.store.Disks(ctx, org)
	if err != nil {
		return n, err
	}
	for _, d := range ds {
		n.disks[d.ID] = d
	}
	return n, nil
}

func (h *api) vmJSON(r *http.Request, v store.VM, names vmNames) vmJSON {
	p, _ := auth.PrincipalFrom(r.Context())
	e, sub, obj := h.z.Enforcer(), authz.UserSubject(p.User.ID), authz.VMObject(v.ID)
	j := vmJSON{ID: v.ID, Name: v.Name, Node: refJSON{v.NodeID, names.nodes[v.NodeID]}, State: v.State, Power: v.Power,
		StopMode: v.StopMode, AutoSuspendMinutes: v.AutoSuspendMinutes, Error: v.Error, IP: v.IP, CPUs: v.CPUs, MemoryMB: v.MemoryMB, DiskGB: v.DiskGB, Image: v.Image, CreatedAt: v.CreatedAt,
		CanPower:  e.Allowed(sub, v.OrgID, obj, store.ActionVMPower),
		CanDelete: e.Allowed(sub, v.OrgID, obj, store.ActionVMDelete), CanNetwork: e.Allowed(sub, v.OrgID, obj, store.ActionVMNetwork),
		CanDisks: e.Allowed(sub, v.OrgID, obj, store.ActionVMDisks), CanSnapshot: e.Allowed(sub, v.OrgID, obj, store.ActionVMSnapshot),
		CanRestore: e.Allowed(sub, v.OrgID, obj, store.ActionVMRestore), CanFork: e.Allowed(sub, v.OrgID, obj, store.ActionVMFork),
		Restoring: v.RestoreState == "node" || v.RestoreState == "storage",
		DNSName:   v.Name + ".internal", Networks: []vmNICJSON{}, Disks: []vmDiskJSON{}}
	if s := h.fleet.Metrics(v.ID); len(s) > 0 {
		u := metricJSON(s[len(s)-1])
		j.Usage = &u
	}
	if v.MgmtIP.IsValid() {
		j.MgmtIP = v.MgmtIP.String()
	}
	if v.Image == "" && v.Provisioned {
		j.StockImage = !slices.ContainsFunc(names.nodeList, func(n store.Node) bool {
			return n.ID == v.NodeID && n.Image != nil && n.Image.State == proto.ImageCustom
		})
	}
	for _, nic := range names.nics[v.ID] {
		nn := names.networks[nic.NetworkID]
		j.Networks = append(j.Networks, vmNICJSON{nic.NetworkID, nn, nic.IP.String(), v.Name + "." + nn + ".internal"})
	}
	if attached := names.vmDisks[v.ID]; len(attached) > 0 {
		errs := h.diskErrors(v.ID)
		for _, a := range attached {
			d := names.disks[a.DiskID]
			j.Disks = append(j.Disks, vmDiskJSON{a.DiskID, d.Name, a.MountPath, d.SizeGB, h.disks.Info(d).State, errs[a.DiskID.String()]})
		}
	}
	if v.UserID != nil {
		j.Owner = ownerJSON{"user", *v.UserID, names.users[*v.UserID]}
	} else if v.TeamID != nil {
		j.Owner = ownerJSON{"team", *v.TeamID, names.teams[*v.TeamID]}
	}
	return j
}

// writeVM renders a single VM; names are loaded per call since only list pages render many.
func (h *api) writeVM(w http.ResponseWriter, r *http.Request, status int, v store.VM) {
	names, err := h.vmNames(r, v.OrgID)
	if err != nil {
		serverError(w, "vm names", err)
		return
	}
	writeJSON(w, status, h.vmJSON(r, v, names))
}

func vmOwnerSubject(v store.VM) string {
	if v.UserID != nil {
		return authz.UserSubject(*v.UserID)
	}
	return authz.TeamSubject(*v.TeamID)
}

func vmAudit(r *http.Request, action string, v store.VM) store.AuditEntry {
	e := auditEntry(r, &v.OrgID, action, authz.VMObject(v.ID), store.ResultAllowed)
	e.Details = map[string]any{"name": v.Name}
	return e
}

// vmFromPath loads {id} within the caller's org and authorizes act on it.
func (h *api) vmFromPath(w http.ResponseWriter, r *http.Request, act string) (store.VM, bool) {
	m, ok := h.orgFromPath(w, r)
	if !ok {
		return store.VM{}, false
	}
	id, ok := pathID(w, r, "id")
	if !ok {
		return store.VM{}, false
	}
	v, err := h.store.VM(r.Context(), m.Org.ID, id)
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "not found")
		return store.VM{}, false
	}
	if err != nil {
		serverError(w, "vm", err)
		return store.VM{}, false
	}
	return v, h.authorize(w, r, v.OrgID, authz.VMObject(v.ID), act)
}

func (h *api) listVMs(w http.ResponseWriter, r *http.Request) {
	m, ok := h.orgFromPath(w, r)
	if !ok {
		return
	}
	vms, err := h.store.VMs(r.Context(), m.Org.ID)
	if err != nil {
		serverError(w, "vms", err)
		return
	}
	names, err := h.vmNames(r, m.Org.ID)
	if err != nil {
		serverError(w, "vm names", err)
		return
	}
	p, _ := auth.PrincipalFrom(r.Context())
	e, sub := h.z.Enforcer(), authz.UserSubject(p.User.ID)
	out := []vmJSON{}
	for _, v := range vms {
		if e.Allowed(sub, m.Org.ID, authz.VMObject(v.ID), "vm:read") {
			out = append(out, h.vmJSON(r, v, names))
		}
	}
	owners := []ownerJSON{}
	if e.Allowed(sub, m.Org.ID, authz.ObjVMs, store.ActionVMCreate) {
		owners = append(owners, ownerJSON{"user", p.User.ID, p.User.Name})
	}
	for _, t := range names.teamList {
		if e.Allowed(sub, m.Org.ID, authz.TeamVMsObject(t.ID), store.ActionVMCreate) {
			owners = append(owners, ownerJSON{"team", t.ID, t.Name})
		}
	}
	type nodeRef struct {
		ID     uuid.UUID `json:"id"`
		Name   string    `json:"name"`
		Online bool      `json:"online"`
	}
	nodes := []nodeRef{}
	for _, n := range names.nodeList {
		if n.Enrolled && e.Allowed(sub, m.Org.ID, authz.NodeObject(n.ID), "node:use") {
			nodes = append(nodes, nodeRef{n.ID, n.Name, h.fleet.Online(n.ID)})
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"vms": out, "creatableOwners": owners, "nodes": nodes})
}

func (h *api) createVM(w http.ResponseWriter, r *http.Request) {
	m, ok := h.orgFromPath(w, r)
	if !ok {
		return
	}
	var body struct {
		Name  string
		Owner struct {
			Kind   string
			TeamID *uuid.UUID
		}
		NodeID                 uuid.UUID
		CPUs, MemoryMB, DiskGB int
	}
	if !readJSON(w, r, &body) {
		return
	}
	p, _ := auth.PrincipalFrom(r.Context())
	v := store.VM{OrgID: m.Org.ID, NodeID: body.NodeID, Name: body.Name, CPUs: body.CPUs, MemoryMB: body.MemoryMB, DiskGB: body.DiskGB, CreatedBy: p.User.ID}
	var obj string
	switch {
	case !vmNamePattern.MatchString(body.Name):
		writeError(w, http.StatusBadRequest, "invalid name")
		return
	case body.CPUs < 1 || body.CPUs > 32 || body.MemoryMB < 512 || body.MemoryMB > 131072 || body.DiskGB < 10 || body.DiskGB > 2048:
		writeError(w, http.StatusBadRequest, "invalid size")
		return
	case body.Owner.Kind == "user":
		v.UserID, obj = &p.User.ID, authz.ObjVMs
	case body.Owner.Kind == "team" && body.Owner.TeamID != nil:
		v.TeamID, obj = body.Owner.TeamID, authz.TeamVMsObject(*body.Owner.TeamID)
	default:
		writeError(w, http.StatusBadRequest, "invalid owner")
		return
	}
	if !h.authorize(w, r, m.Org.ID, obj, store.ActionVMCreate) || !h.authorize(w, r, m.Org.ID, authz.NodeObject(v.NodeID), "node:use") {
		return
	}
	// commit reloads the enforcer, so the generated owner rules apply before the node is told.
	if !h.commit(w, r, func(tx *store.Store) error {
		var err error
		if v, err = tx.CreateVM(r.Context(), v); err != nil {
			return err
		}
		e := vmAudit(r, store.ActionVMCreate, v)
		e.Details["owner"] = vmOwnerSubject(v)
		e.Details["node"] = v.NodeID
		e.Details["cpus"], e.Details["memoryMB"], e.Details["diskGB"] = v.CPUs, v.MemoryMB, v.DiskGB
		return tx.Audit(r.Context(), e)
	}) {
		return
	}
	h.fleet.NotifyOrg(v.OrgID)
	h.writeVM(w, r, http.StatusCreated, v)
}

func (h *api) getVM(w http.ResponseWriter, r *http.Request) {
	v, ok := h.vmFromPath(w, r, "vm:read")
	if !ok {
		return
	}
	h.writeVM(w, r, http.StatusOK, v)
}

func (h *api) powerVM(action string, power proto.Power, mode proto.StopMode) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		v, ok := h.vmFromPath(w, r, store.ActionVMPower)
		if !ok {
			return
		}
		err := h.store.InTx(r.Context(), func(tx *store.Store) error {
			var err error
			if v, err = tx.SetVMPower(r.Context(), v.OrgID, v.ID, power, mode); err != nil {
				return err
			}
			e := vmAudit(r, store.ActionVMPower, v)
			e.Details["action"], e.Details["power"], e.Details["stopMode"] = action, power, mode
			return tx.Audit(r.Context(), e)
		})
		if errors.Is(err, store.ErrNotFound) {
			writeError(w, http.StatusNotFound, "not found")
			return
		}
		if errors.Is(err, store.ErrNotRunning) {
			writeError(w, http.StatusConflict, err.Error())
			return
		}
		if err != nil {
			serverError(w, "vm power", err)
			return
		}
		h.fleet.NotifyOrg(v.OrgID)
		h.writeVM(w, r, http.StatusOK, v)
	}
}

func (h *api) setAutoSuspend(w http.ResponseWriter, r *http.Request) {
	v, ok := h.vmFromPath(w, r, store.ActionVMPower)
	if !ok {
		return
	}
	var body struct {
		Minutes int `json:"minutes"`
	}
	if !readJSON(w, r, &body) {
		return
	}
	if body.Minutes != 0 && (body.Minutes < 5 || body.Minutes > 1440) {
		writeError(w, http.StatusBadRequest, "minutes must be 0 or between 5 and 1440")
		return
	}
	if !h.commit(w, r, func(tx *store.Store) error {
		var err error
		if v, err = tx.SetAutoSuspend(r.Context(), v.OrgID, v.ID, body.Minutes); err != nil {
			return err
		}
		e := vmAudit(r, store.ActionVMPower, v)
		e.Details["autoSuspendMinutes"] = body.Minutes
		return tx.Audit(r.Context(), e)
	}) {
		return
	}
	h.fleet.NotifyOrg(v.OrgID)
	h.writeVM(w, r, http.StatusOK, v)
}

func (h *api) deleteVM(w http.ResponseWriter, r *http.Request) {
	v, ok := h.vmFromPath(w, r, store.ActionVMDelete)
	if !ok {
		return
	}
	if !h.commit(w, r, func(tx *store.Store) error {
		if err := tx.DeleteVM(r.Context(), v.OrgID, v.ID); errors.Is(err, store.ErrConflict) {
			return errForkPending
		} else if err != nil {
			return err
		}
		return tx.Audit(r.Context(), vmAudit(r, store.ActionVMDelete, v))
	}) {
		return
	}
	h.fleet.DropMetrics(v.ID)
	h.fleet.NotifyOrg(v.OrgID)
	w.WriteHeader(http.StatusNoContent)
}

func (h *api) vmMetrics(w http.ResponseWriter, r *http.Request) {
	v, ok := h.vmFromPath(w, r, "vm:read")
	if !ok {
		return
	}
	samples := h.fleet.Metrics(v.ID)
	out := make([]metricJSON, len(samples))
	for i, s := range samples {
		out[i] = metricJSON(s)
	}
	writeJSON(w, http.StatusOK, map[string]any{"samples": out})
}

type metricJSON struct {
	At         time.Time `json:"at"`
	CPUPercent float64   `json:"cpuPercent"`
	MemTotalMB int       `json:"memTotalMB"`
	MemUsedMB  int       `json:"memUsedMB"`
	DiskUsedMB int       `json:"diskUsedMB"`
}

func (h *api) setVMNetworks(w http.ResponseWriter, r *http.Request) {
	v, ok := h.vmFromPath(w, r, store.ActionVMNetwork)
	if !ok {
		return
	}
	var body struct {
		NetworkIDs     []uuid.UUID
		RestartAllowed bool
	}
	if !readJSON(w, r, &body) {
		return
	}
	current, err := h.store.VMNetworks(r.Context(), v.ID)
	if err != nil {
		serverError(w, "vm networks", err)
		return
	}
	attached := map[uuid.UUID]bool{}
	for _, n := range current {
		attached[n.NetworkID] = true
	}
	for _, id := range body.NetworkIDs {
		if !attached[id] && !h.authorize(w, r, v.OrgID, authz.NetworkObject(id), "network:use") {
			return
		}
	}
	if !h.commit(w, r, func(tx *store.Store) error {
		updated, err := tx.SetVMNetworks(r.Context(), v.OrgID, v.ID, body.NetworkIDs, body.RestartAllowed)
		if err != nil {
			return err
		}
		if updated.Generation == v.Generation {
			v = updated
			return nil
		}
		v = updated
		nets, err := tx.Networks(r.Context(), v.OrgID)
		if err != nil {
			return err
		}
		nics, err := tx.VMNetworks(r.Context(), v.ID)
		if err != nil {
			return err
		}
		names := []string{}
		for _, nic := range nics {
			for _, n := range nets {
				if n.ID == nic.NetworkID {
					names = append(names, n.Name)
				}
			}
		}
		e := vmAudit(r, store.ActionVMNetwork, v)
		e.Details["networks"] = names
		return tx.Audit(r.Context(), e)
	}) {
		return
	}
	h.fleet.NotifyOrg(v.OrgID)
	h.writeVM(w, r, http.StatusOK, v)
}

const maxVMDisks = 8

var (
	mountPathPattern   = regexp.MustCompile(`^(/[A-Za-z0-9._-]+)+$`)
	reservedMountTrees = []string{"/bin", "/boot", "/dev", "/etc", "/lib", "/proc", "/run", "/sbin", "/sys", "/usr", "/var/lib/cloud", "/home/ubuntu/.ssh"}
	reservedMountPaths = []string{"/home", "/home/ubuntu", "/root", "/var", "/var/lib"}
	errDiskMissing     = fmt.Errorf("disk is missing from storage: %w", store.ErrConflict)
	errForkPending     = errors.New("a fork from this VM's snapshot is still being created or failed; delete the fork first")
)

func validateVMDisks(disks []store.VMDisk) error {
	if len(disks) > maxVMDisks {
		return fmt.Errorf("at most %d disks", maxVMDisks)
	}
	ids, paths := map[uuid.UUID]bool{}, map[string]bool{}
	for i, d := range disks {
		p := d.MountPath
		if !mountPathPattern.MatchString(p) || path.Clean(p) != p {
			return fmt.Errorf("invalid mount path %q", p)
		}
		if slices.Contains(reservedMountPaths, p) || slices.ContainsFunc(reservedMountTrees, func(t string) bool { return p == t || strings.HasPrefix(p, t+"/") }) {
			return fmt.Errorf("reserved mount path %q", p)
		}
		if ids[d.DiskID] || paths[p] {
			return errors.New("duplicate disk or mount path")
		}
		// A disk mounted inside another would be hidden or shadowed depending on mount order.
		if slices.ContainsFunc(disks[:i], func(o store.VMDisk) bool {
			return strings.HasPrefix(p, o.MountPath+"/") || strings.HasPrefix(o.MountPath, p+"/")
		}) {
			return fmt.Errorf("mount path %q is nested in another", p)
		}
		ids[d.DiskID], paths[p] = true, true
	}
	return nil
}

func (h *api) setVMDisks(w http.ResponseWriter, r *http.Request) {
	v, ok := h.vmFromPath(w, r, store.ActionVMDisks)
	if !ok {
		return
	}
	var body struct {
		Disks          []store.VMDisk
		RestartAllowed bool
	}
	if !readJSON(w, r, &body) {
		return
	}
	if err := validateVMDisks(body.Disks); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	var denied uuid.UUID
	if !h.commit(w, r, func(tx *store.Store) error {
		// The org lock serializes with SetVMDisks, so the attached set read here is the one it replaces.
		if err := tx.LockOrg(r.Context(), v.OrgID); err != nil {
			return err
		}
		current, err := tx.VMDisks(r.Context(), v.ID)
		if err != nil {
			return err
		}
		p, _ := auth.PrincipalFrom(r.Context())
		e, sub := h.z.Enforcer(), authz.UserSubject(p.User.ID)
		for _, d := range body.Disks {
			if slices.ContainsFunc(current, func(c store.VMDisk) bool { return c.DiskID == d.DiskID }) {
				continue
			}
			disk, err := tx.Disk(r.Context(), v.OrgID, d.DiskID)
			if err != nil {
				return err
			}
			if !e.Allowed(sub, v.OrgID, authz.DiskObject(disk.ID), "disk:use") {
				denied = disk.ID
				return errForbidden
			}
			if h.disks.Info(disk).State == "missing" {
				return errDiskMissing
			}
		}
		updated, changed, err := tx.SetVMDisks(r.Context(), v.OrgID, v.ID, body.Disks, body.RestartAllowed)
		if err != nil {
			return err
		}
		v = updated
		if !changed {
			return nil
		}
		all, err := tx.Disks(r.Context(), v.OrgID)
		if err != nil {
			return err
		}
		names := make(map[uuid.UUID]string, len(all))
		for _, disk := range all {
			names[disk.ID] = disk.Name
		}
		entries := []string{}
		for _, d := range body.Disks {
			entries = append(entries, names[d.DiskID]+":"+d.MountPath)
		}
		ae := vmAudit(r, store.ActionVMDisks, v)
		ae.Details["disks"] = entries
		return tx.Audit(r.Context(), ae)
	}) {
		if denied != uuid.Nil {
			h.auditDenied(r, v.OrgID, authz.DiskObject(denied), "disk:use")
		}
		return
	}
	h.fleet.NotifyOrg(v.OrgID)
	h.writeVM(w, r, http.StatusOK, v)
}

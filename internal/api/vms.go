package api

import (
	"errors"
	"net/http"
	"regexp"
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
	ID        uuid.UUID      `json:"id"`
	Name      string         `json:"name"`
	Owner     ownerJSON      `json:"owner"`
	Node      refJSON        `json:"node"`
	State     proto.VMState  `json:"state"`
	Power     proto.Power    `json:"power"`
	StopMode  proto.StopMode `json:"stopMode,omitempty"`
	Error     string         `json:"error,omitempty"`
	IP        string         `json:"ip,omitempty"`
	CPUs      int            `json:"cpus"`
	MemoryMB  int            `json:"memoryMB"`
	DiskGB    int            `json:"diskGB"`
	CreatedAt time.Time      `json:"createdAt"`
	CanPower  bool           `json:"canPower"`
	CanDelete bool           `json:"canDelete"`
}

// vmNames holds the org's display names for VM owners and nodes.
type vmNames struct {
	users, teams, nodes map[uuid.UUID]string
	teamList            []store.Team
	nodeList            []store.Node
}

func (h *api) vmNames(r *http.Request, org uuid.UUID) (vmNames, error) {
	ctx := r.Context()
	n := vmNames{users: map[uuid.UUID]string{}, teams: map[uuid.UUID]string{}, nodes: map[uuid.UUID]string{}}
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
	return n, nil
}

func (h *api) vmJSON(r *http.Request, v store.VM, names vmNames) vmJSON {
	p, _ := auth.PrincipalFrom(r.Context())
	e, sub, obj := h.z.Enforcer(), authz.UserSubject(p.User.ID), authz.VMObject(v.ID)
	j := vmJSON{ID: v.ID, Name: v.Name, Node: refJSON{v.NodeID, names.nodes[v.NodeID]}, State: v.State, Power: v.Power,
		StopMode: v.StopMode, Error: v.Error, IP: v.IP, CPUs: v.CPUs, MemoryMB: v.MemoryMB, DiskGB: v.DiskGB, CreatedAt: v.CreatedAt,
		CanPower:  e.Allowed(sub, v.OrgID, obj, store.ActionVMPower),
		CanDelete: e.Allowed(sub, v.OrgID, obj, store.ActionVMDelete)}
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
		if e.Allowed(sub, m.Org.ID, authz.NodeObject(n.ID), "node:use") {
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
	case body.CPUs < 1 || body.CPUs > 32 || body.MemoryMB < 512 || body.MemoryMB > 131072 || body.DiskGB < 5 || body.DiskGB > 2048:
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

func (h *api) powerVM(power proto.Power, mode proto.StopMode) http.HandlerFunc {
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
			e.Details["power"], e.Details["stopMode"] = power, mode
			return tx.Audit(r.Context(), e)
		})
		if errors.Is(err, store.ErrNotFound) {
			writeError(w, http.StatusNotFound, "not found")
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

func (h *api) deleteVM(w http.ResponseWriter, r *http.Request) {
	v, ok := h.vmFromPath(w, r, store.ActionVMDelete)
	if !ok {
		return
	}
	if !h.commit(w, r, func(tx *store.Store) error {
		if err := tx.DeleteVM(r.Context(), v.OrgID, v.ID); err != nil {
			return err
		}
		return tx.Audit(r.Context(), vmAudit(r, store.ActionVMDelete, v))
	}) {
		return
	}
	h.fleet.NotifyOrg(v.OrgID)
	w.WriteHeader(http.StatusNoContent)
}

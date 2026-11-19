package api

import (
	"errors"
	"net/http"
	"regexp"
	"time"

	"github.com/google/uuid"

	"github.com/asterism-dev/observatory/internal/auth"
	"github.com/asterism-dev/observatory/internal/authz"
	"github.com/asterism-dev/observatory/internal/store"
)

var (
	snapshotNamePattern = regexp.MustCompile(`^[a-z][a-z0-9-]{0,30}$`)
	errSnapshotBusy     = errors.New("snapshot or VM busy: a snapshot, restore or fork is in progress")
)

type snapshotDiskJSON struct {
	Name      string `json:"name"`
	MountPath string `json:"mountPath"`
	SizeGB    int    `json:"sizeGB"`
	ReadyGB   int    `json:"readyGB"`
}

type snapshotJSON struct {
	ID               uuid.UUID          `json:"id"`
	Name             string             `json:"name"`
	Memory           bool               `json:"memory"`
	State            string             `json:"state"`
	Error            string             `json:"error,omitempty"`
	SizeBytes        int64              `json:"sizeBytes"`
	Disks            []snapshotDiskJSON `json:"disks"`
	CreatedAt        time.Time          `json:"createdAt"`
	CanRestoreMemory bool               `json:"canRestoreMemory"`
}

func snapshotView(sn store.Snapshot, canRestoreMemory bool) snapshotJSON {
	j := snapshotJSON{ID: sn.ID, Name: sn.Name, Memory: sn.Memory, State: sn.State, Error: sn.Error, SizeBytes: sn.SizeBytes,
		Disks: []snapshotDiskJSON{}, CreatedAt: sn.CreatedAt, CanRestoreMemory: canRestoreMemory}
	for _, d := range sn.Disks {
		j.Disks = append(j.Disks, snapshotDiskJSON{d.DiskName, d.MountPath, d.SizeGB, d.ReadyGB})
	}
	return j
}

func snapshotAudit(r *http.Request, action string, v store.VM, sn store.Snapshot) store.AuditEntry {
	e := auditEntry(r, &v.OrgID, action, authz.VMObject(v.ID), store.ResultAllowed)
	e.Details = map[string]any{"name": sn.Name}
	return e
}

// snapshotFromPath loads {sid} of the VM in the path after authorizing act on that VM.
func (h *api) snapshotFromPath(w http.ResponseWriter, r *http.Request, act string) (store.VM, store.Snapshot, bool) {
	v, ok := h.vmFromPath(w, r, act)
	if !ok {
		return v, store.Snapshot{}, false
	}
	sid, ok := pathID(w, r, "sid")
	if !ok {
		return v, store.Snapshot{}, false
	}
	sn, err := h.store.Snapshot(r.Context(), v.OrgID, v.ID, sid)
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "not found")
		return v, sn, false
	}
	if err != nil {
		serverError(w, "snapshot", err)
		return v, sn, false
	}
	return v, sn, true
}

func (h *api) listSnapshots(w http.ResponseWriter, r *http.Request) {
	v, ok := h.vmFromPath(w, r, "vm:read")
	if !ok {
		return
	}
	sns, err := h.store.Snapshots(r.Context(), v.OrgID, v.ID)
	if err != nil {
		serverError(w, "snapshots", err)
		return
	}
	out := make([]snapshotJSON, 0, len(sns))
	for _, sn := range sns {
		restorable, err := h.store.MemoryRestorable(r.Context(), v, sn)
		if err != nil {
			serverError(w, "snapshot shape", err)
			return
		}
		out = append(out, snapshotView(sn, restorable))
	}
	writeJSON(w, http.StatusOK, out)
}

func (h *api) createSnapshot(w http.ResponseWriter, r *http.Request) {
	v, ok := h.vmFromPath(w, r, store.ActionVMSnapshot)
	if !ok {
		return
	}
	var body struct {
		Name   string
		Memory bool
	}
	if !readJSON(w, r, &body) {
		return
	}
	if !snapshotNamePattern.MatchString(body.Name) {
		writeError(w, http.StatusBadRequest, "invalid name")
		return
	}
	p, _ := auth.PrincipalFrom(r.Context())
	var sn store.Snapshot
	if !h.commit(w, r, func(tx *store.Store) error {
		var err error
		if sn, err = tx.CreateSnapshot(r.Context(), v.OrgID, v.ID, body.Name, body.Memory, p.User.ID); err != nil {
			return err
		}
		attached, err := tx.VMDisks(r.Context(), v.ID)
		if err != nil {
			return err
		}
		disks := []string{}
		for _, a := range attached {
			d, err := tx.Disk(r.Context(), v.OrgID, a.DiskID)
			if err != nil {
				return err
			}
			disks = append(disks, d.Name)
		}
		e := snapshotAudit(r, store.ActionVMSnapshot, v, sn)
		e.Details["memory"], e.Details["disks"] = sn.Memory, disks
		return tx.Audit(r.Context(), e)
	}) {
		return
	}
	h.fleet.NotifyOrg(v.OrgID)
	writeJSON(w, http.StatusCreated, snapshotView(sn, false))
}

func (h *api) deleteSnapshot(w http.ResponseWriter, r *http.Request) {
	v, sn, ok := h.snapshotFromPath(w, r, store.ActionVMSnapshot)
	if !ok {
		return
	}
	if !h.commit(w, r, func(tx *store.Store) error {
		if err := tx.DeleteSnapshot(r.Context(), v.OrgID, v.ID, sn.ID); errors.Is(err, store.ErrConflict) {
			return errSnapshotBusy
		} else if err != nil {
			return err
		}
		return tx.Audit(r.Context(), snapshotAudit(r, store.ActionVMSnapshotDelete, v, sn))
	}) {
		return
	}
	h.fleet.NotifyOrg(v.OrgID)
	w.WriteHeader(http.StatusNoContent)
}

func (h *api) restoreSnapshot(w http.ResponseWriter, r *http.Request) {
	v, sn, ok := h.snapshotFromPath(w, r, store.ActionVMRestore)
	if !ok {
		return
	}
	var body struct{ Memory, RestartAllowed bool }
	if !readJSON(w, r, &body) {
		return
	}
	if !h.commit(w, r, func(tx *store.Store) error {
		var err error
		if v, err = tx.RequestRestore(r.Context(), v.OrgID, v.ID, sn.ID, body.Memory, body.RestartAllowed); errors.Is(err, store.ErrConflict) {
			return errSnapshotBusy
		} else if err != nil {
			return err
		}
		e := vmAudit(r, store.ActionVMRestore, v)
		e.Details["snapshot"], e.Details["memory"] = sn.Name, body.Memory && sn.Memory
		return tx.Audit(r.Context(), e)
	}) {
		return
	}
	h.fleet.NotifyOrg(v.OrgID)
	h.writeVM(w, r, http.StatusOK, v)
}

func (h *api) forkSnapshot(w http.ResponseWriter, r *http.Request) {
	src, sn, ok := h.snapshotFromPath(w, r, store.ActionVMFork)
	if !ok {
		return
	}
	var body struct {
		Name   string
		Memory bool
	}
	if !readJSON(w, r, &body) {
		return
	}
	if !vmNamePattern.MatchString(body.Name) {
		writeError(w, http.StatusBadRequest, "invalid name")
		return
	}
	// The fork keeps the source's owner, so the caller must be allowed to create VMs for that owner on that node.
	scope := authz.ObjVMs
	if src.TeamID != nil {
		scope = authz.TeamVMsObject(*src.TeamID)
	}
	if !h.authorize(w, r, src.OrgID, scope, store.ActionVMCreate) || !h.authorize(w, r, src.OrgID, authz.NodeObject(src.NodeID), "node:use") {
		return
	}
	p, _ := auth.PrincipalFrom(r.Context())
	var v store.VM
	if !h.commit(w, r, func(tx *store.Store) error {
		var err error
		if v, err = tx.ForkVM(r.Context(), src.OrgID, sn, body.Name, body.Memory, p.User.ID); err != nil {
			return err
		}
		fe := vmAudit(r, store.ActionVMFork, src)
		fe.Details["snapshot"], fe.Details["memory"], fe.Details["vm"], fe.Details["vmName"] = sn.Name, body.Memory && sn.Memory, v.ID, v.Name
		if err := tx.Audit(r.Context(), fe); err != nil {
			return err
		}
		ce := vmAudit(r, store.ActionVMCreate, v)
		ce.Details["owner"] = vmOwnerSubject(v)
		ce.Details["node"] = v.NodeID
		ce.Details["cpus"], ce.Details["memoryMB"], ce.Details["diskGB"] = v.CPUs, v.MemoryMB, v.DiskGB
		ce.Details["forkOf"], ce.Details["snapshot"] = src.ID, sn.Name
		return tx.Audit(r.Context(), ce)
	}) {
		return
	}
	h.fleet.NotifyOrg(v.OrgID)
	h.writeVM(w, r, http.StatusCreated, v)
}

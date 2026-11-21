package api

import (
	"errors"
	"net/http"
	"time"

	"github.com/google/uuid"

	"github.com/asterism-dev/observatory/internal/auth"
	"github.com/asterism-dev/observatory/internal/authz"
	"github.com/asterism-dev/observatory/internal/store"
)

const maxDiskGB = 4096

type diskJSON struct {
	ID             uuid.UUID       `json:"id"`
	Name           string          `json:"name"`
	Owner          ownerJSON       `json:"owner"`
	SizeGB         int             `json:"sizeGB"`
	AllocatedBytes int64           `json:"allocatedBytes"`
	State          string          `json:"state"`
	ExportError    string          `json:"exportError,omitempty"`
	Attachment     *diskAttachJSON `json:"attachment,omitempty"`
	CreatedAt      time.Time       `json:"createdAt"`
	CanUpdate      bool            `json:"canUpdate"`
	CanDelete      bool            `json:"canDelete"`
	CanUse         bool            `json:"canUse"`
}

type diskAttachJSON struct {
	VM        refJSON `json:"vm"`
	MountPath string  `json:"mountPath"`
}

type ownerNames struct{ users, teams map[uuid.UUID]string }

func (h *api) ownerNames(r *http.Request, org uuid.UUID) (ownerNames, error) {
	n := ownerNames{users: map[uuid.UUID]string{}, teams: map[uuid.UUID]string{}}
	members, err := h.store.Members(r.Context(), org)
	if err != nil {
		return n, err
	}
	for _, m := range members {
		n.users[m.User.ID] = m.User.Name
	}
	teams, err := h.store.Teams(r.Context(), org)
	if err != nil {
		return n, err
	}
	for _, t := range teams {
		n.teams[t.ID] = t.Name
	}
	return n, nil
}

func (h *api) diskJSON(r *http.Request, d store.Disk, names ownerNames) diskJSON {
	p, _ := auth.PrincipalFrom(r.Context())
	e, sub, obj := h.z.Enforcer(), authz.UserSubject(p.User.ID), authz.DiskObject(d.ID)
	info := h.disks.Info(d)
	j := diskJSON{ID: d.ID, Name: d.Name, SizeGB: d.SizeGB, AllocatedBytes: info.AllocatedBytes, State: info.State,
		ExportError: info.ExportError, CreatedAt: d.CreatedAt,
		CanUpdate: e.Allowed(sub, d.OrgID, obj, "compute.disks.update"), CanDelete: e.Allowed(sub, d.OrgID, obj, "compute.disks.delete"),
		CanUse: e.Allowed(sub, d.OrgID, obj, "compute.disks.use")}
	if d.UserID != nil {
		j.Owner = ownerJSON{"user", *d.UserID, names.users[*d.UserID]}
	} else if d.TeamID != nil {
		j.Owner = ownerJSON{"team", *d.TeamID, names.teams[*d.TeamID]}
	}
	if d.Attachment != nil {
		j.Attachment = &diskAttachJSON{refJSON{d.Attachment.VMID, d.Attachment.VMName}, d.Attachment.MountPath}
	}
	return j
}

func (h *api) writeDisk(w http.ResponseWriter, r *http.Request, status int, d store.Disk, warning string) {
	names, err := h.ownerNames(r, d.OrgID)
	if err != nil {
		serverError(w, "disk owners", err)
		return
	}
	j := h.diskJSON(r, d, names)
	if warning == "" {
		writeJSON(w, status, j)
		return
	}
	writeJSON(w, status, struct {
		diskJSON
		Warning string `json:"warning"`
	}{j, warning})
}

func diskAudit(r *http.Request, action string, d store.Disk) store.AuditEntry {
	e := auditEntry(r, &d.OrgID, action, authz.DiskObject(d.ID), store.ResultAllowed)
	e.Details = map[string]any{"name": d.Name, "sizeGB": d.SizeGB}
	return e
}

func validDisk(w http.ResponseWriter, name string, sizeGB int) bool {
	if !vmNamePattern.MatchString(name) {
		writeError(w, http.StatusBadRequest, "invalid name")
		return false
	}
	return validDiskSize(w, sizeGB)
}

func validDiskSize(w http.ResponseWriter, sizeGB int) bool {
	if sizeGB < 1 || sizeGB > maxDiskGB {
		writeError(w, http.StatusBadRequest, "invalid size")
		return false
	}
	return true
}

type diskOwnerBody struct {
	Kind   string
	TeamID *uuid.UUID
}

// diskOwner authorizes compute.disks.create for the owner scope and fills the disk's owner.
func (h *api) diskOwner(w http.ResponseWriter, r *http.Request, org uuid.UUID, o diskOwnerBody, d *store.Disk) bool {
	p, _ := auth.PrincipalFrom(r.Context())
	var obj string
	switch {
	case o.Kind == "user":
		d.UserID, obj = &p.User.ID, authz.ObjDisks
	case o.Kind == "team" && o.TeamID != nil:
		d.TeamID, obj = o.TeamID, authz.TeamDisksObject(*o.TeamID)
	default:
		writeError(w, http.StatusBadRequest, "invalid owner")
		return false
	}
	if !h.authorize(w, r, org, obj, "compute.disks.create") {
		return false
	}
	if d.TeamID != nil && !h.z.Enforcer().HasTeam(org, *d.TeamID) {
		writeError(w, http.StatusNotFound, "not found")
		return false
	}
	return true
}

// diskFromPath loads {id} within the caller's org and authorizes act on it.
func (h *api) diskFromPath(w http.ResponseWriter, r *http.Request, act string) (store.Disk, bool) {
	m, ok := h.orgFromPath(w, r)
	if !ok {
		return store.Disk{}, false
	}
	id, ok := pathID(w, r, "id")
	if !ok {
		return store.Disk{}, false
	}
	d, err := h.store.Disk(r.Context(), m.OrgID, id)
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "not found")
		return store.Disk{}, false
	}
	if err != nil {
		serverError(w, "disk", err)
		return store.Disk{}, false
	}
	return d, h.authorize(w, r, d.OrgID, authz.DiskObject(d.ID), act)
}

func (h *api) listDisks(w http.ResponseWriter, r *http.Request) {
	m, ok := h.orgFromPath(w, r)
	if !ok {
		return
	}
	ds, err := h.store.Disks(r.Context(), m.OrgID)
	if err != nil {
		serverError(w, "disks", err)
		return
	}
	names, err := h.ownerNames(r, m.OrgID)
	if err != nil {
		serverError(w, "disk owners", err)
		return
	}
	p, _ := auth.PrincipalFrom(r.Context())
	e, sub := h.z.Enforcer(), authz.UserSubject(p.User.ID)
	out := []diskJSON{}
	for _, d := range ds {
		if e.Allowed(sub, m.OrgID, authz.DiskObject(d.ID), "compute.disks.get") {
			out = append(out, h.diskJSON(r, d, names))
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"disks": out, "storageOnline": h.disks.Online()})
}

func (h *api) getDisk(w http.ResponseWriter, r *http.Request) {
	d, ok := h.diskFromPath(w, r, "compute.disks.get")
	if !ok {
		return
	}
	h.writeDisk(w, r, http.StatusOK, d, "")
}

func (h *api) createDisk(w http.ResponseWriter, r *http.Request) {
	m, ok := h.orgFromPath(w, r)
	if !ok {
		return
	}
	var body struct {
		Name   string
		Owner  diskOwnerBody
		SizeGB int
	}
	if !readJSON(w, r, &body) || !validDisk(w, body.Name, body.SizeGB) {
		return
	}
	p, _ := auth.PrincipalFrom(r.Context())
	d := store.Disk{OrgID: m.OrgID, Name: body.Name, SizeGB: body.SizeGB, CreatedBy: p.User.ID}
	if !h.diskOwner(w, r, m.OrgID, body.Owner, &d) {
		return
	}
	if !h.commit(w, r, func(tx *store.Store) error {
		var err error
		if d, err = tx.CreateDisk(r.Context(), d); err != nil {
			return err
		}
		return tx.Audit(r.Context(), diskAudit(r, store.ActionDiskCreate, d))
	}) {
		return
	}
	h.writeDisk(w, r, http.StatusCreated, d, "")
}

func (h *api) copyDisk(w http.ResponseWriter, r *http.Request) {
	src, ok := h.diskFromPath(w, r, "compute.disks.use")
	if !ok {
		return
	}
	var body struct {
		Name  string
		Owner diskOwnerBody
	}
	if !readJSON(w, r, &body) {
		return
	}
	if !vmNamePattern.MatchString(body.Name) {
		writeError(w, http.StatusBadRequest, "invalid name")
		return
	}
	p, _ := auth.PrincipalFrom(r.Context())
	d := store.Disk{OrgID: src.OrgID, Name: body.Name, CopyFrom: &src.ID, CreatedBy: p.User.ID}
	if !h.diskOwner(w, r, src.OrgID, body.Owner, &d) {
		return
	}
	if h.disks.Info(src).State == "missing" {
		writeError(w, http.StatusConflict, "source disk is missing from storage")
		return
	}
	if !h.commit(w, r, func(tx *store.Store) error {
		var err error
		if d, err = tx.CreateDisk(r.Context(), d); err != nil {
			return err
		}
		e := diskAudit(r, store.ActionDiskCopy, d)
		e.Details["source"] = src.ID
		return tx.Audit(r.Context(), e)
	}) {
		return
	}
	var warning string
	if st := h.disks.Info(src).State; src.Attachment != nil || st == "releasing" || st == "creating" || st == "growing" {
		warning = "source disk is in use or not settled; the copy may not be consistent"
	}
	h.writeDisk(w, r, http.StatusCreated, d, warning)
}

func (h *api) growDisk(w http.ResponseWriter, r *http.Request) {
	d, ok := h.diskFromPath(w, r, "compute.disks.update")
	if !ok {
		return
	}
	var body struct {
		SizeGB         int
		RestartAllowed bool
	}
	if !readJSON(w, r, &body) || !validDiskSize(w, body.SizeGB) {
		return
	}
	if !h.commit(w, r, func(tx *store.Store) error {
		var err error
		if d, err = tx.GrowDisk(r.Context(), d.OrgID, d.ID, body.SizeGB, body.RestartAllowed); err != nil {
			return err
		}
		return tx.Audit(r.Context(), diskAudit(r, store.ActionDiskGrow, d))
	}) {
		return
	}
	h.fleet.NotifyOrg(d.OrgID)
	h.writeDisk(w, r, http.StatusOK, d, "")
}

func (h *api) releaseDisk(w http.ResponseWriter, r *http.Request) {
	d, ok := h.diskFromPath(w, r, "compute.disks.update")
	if !ok {
		return
	}
	if !h.commit(w, r, func(tx *store.Store) error {
		if err := tx.ForceRelease(r.Context(), d.OrgID, d.ID); err != nil {
			return err
		}
		return tx.Audit(r.Context(), diskAudit(r, store.ActionDiskRelease, d))
	}) {
		return
	}
	h.fleet.NotifyOrg(d.OrgID)
	w.WriteHeader(http.StatusNoContent)
}

func (h *api) deleteDisk(w http.ResponseWriter, r *http.Request) {
	d, ok := h.diskFromPath(w, r, "compute.disks.delete")
	if !ok {
		return
	}
	if !h.commit(w, r, func(tx *store.Store) error {
		if err := tx.DeleteDisk(r.Context(), d.OrgID, d.ID); err != nil {
			return err
		}
		return tx.Audit(r.Context(), diskAudit(r, store.ActionDiskDelete, d))
	}) {
		return
	}
	// Deleting a disk can finish a pending restore, which bumps the VM's generation.
	h.fleet.NotifyOrg(d.OrgID)
	w.WriteHeader(http.StatusNoContent)
}

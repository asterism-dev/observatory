package api

import (
	"net/http"
	"slices"
	"time"

	"github.com/google/uuid"

	"github.com/asterism-dev/observatory/internal/auth"
	"github.com/asterism-dev/observatory/internal/authz"
	"github.com/asterism-dev/observatory/internal/store"
)

type vmHistoryJSON struct {
	ID        uuid.UUID `json:"id"`
	Name      string    `json:"name"`
	Owner     ownerJSON `json:"owner"`
	CPUs      int       `json:"cpus"`
	MemoryMB  int       `json:"memoryMB"`
	DiskGB    int       `json:"diskGB"`
	Image     string    `json:"image"`
	CreatedAt time.Time `json:"createdAt"`
	CreatedBy *refJSON  `json:"createdBy"`
	DeletedAt time.Time `json:"deletedAt"`
	DeletedBy *refJSON  `json:"deletedBy"`
	Reason    string    `json:"reason"`
}

// setDeleteAfter needs compute.instances.delete even to extend or clear: an expiry is a delayed delete.
func (h *api) setDeleteAfter(w http.ResponseWriter, r *http.Request) {
	v, ok := h.vmFromPath(w, r, "compute.instances.delete")
	if !ok {
		return
	}
	var body struct {
		DeleteAfter *string `json:"deleteAfter"`
	}
	if !readJSON(w, r, &body) {
		return
	}
	var at *time.Time
	if body.DeleteAfter != nil {
		t, err := parseDeleteAfter(*body.DeleteAfter, time.Now())
		if err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		at = &t
	}
	if !h.commit(w, r, func(tx *store.Store) error {
		var err error
		if v, err = tx.SetDeleteAt(r.Context(), v.OrgID, v.ID, at); err != nil {
			return err
		}
		e := vmAudit(r, store.ActionVMDeleteAfter, v)
		e.Details["deleteAt"] = at
		return tx.Audit(r.Context(), e)
	}) {
		return
	}
	h.writeVM(w, r, http.StatusOK, v)
}

// vmHistory checks team membership against the row, since the per-VM grants went away with the VM.
func (h *api) vmHistory(w http.ResponseWriter, r *http.Request) {
	m, ok := h.orgFromPath(w, r)
	if !ok {
		return
	}
	rows, err := h.store.VMHistory(r.Context(), m.OrgID)
	if err != nil {
		serverError(w, "vm history", err)
		return
	}
	p, _ := auth.PrincipalFrom(r.Context())
	users, err := h.memberNames(r.Context(), m.OrgID)
	if err != nil {
		serverError(w, "members", err)
		return
	}
	teams, err := h.teamNames(r.Context(), m.OrgID)
	if err != nil {
		serverError(w, "teams", err)
		return
	}
	e, sub := h.z.Enforcer(), authz.UserSubject(p.User.ID)
	user := func(id *uuid.UUID) *refJSON {
		if id == nil {
			return nil
		}
		return &refJSON{*id, users[*id]}
	}
	out := []vmHistoryJSON{}
	for _, row := range rows {
		visible := row.UserID != nil && *row.UserID == p.User.ID || row.TeamID != nil && slices.Contains(e.TeamMembers(m.OrgID, *row.TeamID), p.User.ID) ||
			e.Allowed(sub, m.OrgID, authz.VMObject(row.ID), "compute.instances.get")
		if !visible {
			continue
		}
		j := vmHistoryJSON{ID: row.ID, Name: row.Name, CPUs: row.CPUs, MemoryMB: row.MemoryMB, DiskGB: row.DiskGB, Image: row.ImageRef,
			CreatedAt: row.CreatedAt, CreatedBy: user(row.CreatedBy), DeletedAt: row.DeletedAt, DeletedBy: user(row.DeletedBy), Reason: row.Reason}
		if row.UserID != nil {
			j.Owner = ownerJSON{"user", *row.UserID, users[*row.UserID]}
		} else if row.TeamID != nil {
			j.Owner = ownerJSON{"team", *row.TeamID, teams[*row.TeamID]}
		}
		out = append(out, j)
	}
	writeJSON(w, http.StatusOK, map[string]any{"vms": out})
}

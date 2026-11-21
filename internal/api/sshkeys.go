package api

import (
	"net/http"
	"time"

	"github.com/google/uuid"

	"github.com/asterism-dev/observatory/internal/auth"
	"github.com/asterism-dev/observatory/internal/authz"
	"github.com/asterism-dev/observatory/internal/store"
)

// sshKeyJSON omits the public key itself; the fingerprint identifies it.
type sshKeyJSON struct {
	ID          uuid.UUID `json:"id"`
	Name        string    `json:"name"`
	Fingerprint string    `json:"fingerprint"`
	CreatedAt   time.Time `json:"createdAt"`
}

func toSSHKeyJSON(k store.SSHKey) sshKeyJSON {
	return sshKeyJSON{k.ID, k.Name, k.Fingerprint, k.CreatedAt}
}

func sshKeyAudit(r *http.Request, action string, k store.SSHKey) store.AuditEntry {
	e := auditEntry(r, nil, action, authz.UserSubject(k.UserID)+"/ssh-keys/"+k.ID.String(), store.ResultAllowed)
	e.Details = map[string]any{"name": k.Name, "fingerprint": k.Fingerprint}
	return e
}

// notifyUserOrgs re-pushes every org of user, since its keys reach the VMs it owns or shares through teams.
func (h *api) notifyUserOrgs(r *http.Request, user uuid.UUID) {
	for _, org := range h.z.Enforcer().OrgsOf(user) {
		h.fleet.NotifyOrg(org)
	}
}

func (h *api) listSSHKeys(w http.ResponseWriter, r *http.Request) {
	p, _ := auth.PrincipalFrom(r.Context())
	ks, err := h.store.SSHKeys(r.Context(), p.User.ID)
	if err != nil {
		serverError(w, "ssh keys", err)
		return
	}
	out := make([]sshKeyJSON, 0, len(ks))
	for _, k := range ks {
		out = append(out, toSSHKeyJSON(k))
	}
	writeJSON(w, http.StatusOK, out)
}

func (h *api) addSSHKey(w http.ResponseWriter, r *http.Request) {
	var body struct{ Name, PublicKey string }
	if !readJSON(w, r, &body) {
		return
	}
	name, ok := validName(body.Name)
	if !ok {
		writeError(w, http.StatusBadRequest, "invalid name")
		return
	}
	p, _ := auth.PrincipalFrom(r.Context())
	k := store.SSHKey{UserID: p.User.ID, Name: name, PublicKey: body.PublicKey}
	if !h.commit(w, r, func(tx *store.Store) error {
		var err error
		if k, err = tx.AddSSHKey(r.Context(), k); err != nil {
			return err
		}
		return tx.Audit(r.Context(), sshKeyAudit(r, store.ActionSSHKeyAdd, k))
	}) {
		return
	}
	h.notifyUserOrgs(r, p.User.ID)
	writeJSON(w, http.StatusCreated, toSSHKeyJSON(k))
}

func (h *api) deleteSSHKey(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r, "id")
	if !ok {
		return
	}
	p, _ := auth.PrincipalFrom(r.Context())
	if !h.commit(w, r, func(tx *store.Store) error {
		ks, err := tx.SSHKeys(r.Context(), p.User.ID)
		if err != nil {
			return err
		}
		for _, k := range ks {
			if k.ID == id {
				if err := tx.DeleteSSHKey(r.Context(), p.User.ID, id); err != nil {
					return err
				}
				return tx.Audit(r.Context(), sshKeyAudit(r, store.ActionSSHKeyDelete, k))
			}
		}
		return store.ErrNotFound
	}) {
		return
	}
	h.notifyUserOrgs(r, p.User.ID)
	w.WriteHeader(http.StatusNoContent)
}

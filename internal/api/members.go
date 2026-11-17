package api

import (
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/google/uuid"

	"github.com/asterism-dev/observatory/internal/auth"
	"github.com/asterism-dev/observatory/internal/authz"
	"github.com/asterism-dev/observatory/internal/store"
)

const inviteTTL = 7 * 24 * time.Hour

func pathID(w http.ResponseWriter, r *http.Request, name string) (uuid.UUID, bool) {
	raw := r.PathValue(name)
	id, err := uuid.Parse(raw)
	if err != nil || id.String() != raw {
		writeError(w, http.StatusNotFound, "not found")
		return uuid.Nil, false
	}
	return id, true
}

func readRole(w http.ResponseWriter, r *http.Request) (store.Role, bool) {
	var body struct{ Role store.Role }
	if !readJSON(w, r, &body) {
		return "", false
	}
	switch body.Role {
	case store.RoleOwner, store.RoleAdmin, store.RoleMember:
		return body.Role, true
	}
	writeError(w, http.StatusBadRequest, "invalid role")
	return "", false
}

// privileged roles can only be granted or taken away by holders of member:set-role (owners).
func privileged(role store.Role) bool { return role == store.RoleOwner || role == store.RoleAdmin }

func (h *api) listMembers(w http.ResponseWriter, r *http.Request) {
	m, ok := h.orgFromPath(w, r)
	if !ok || !h.authorize(w, r, m.Org.ID, authz.ObjMembers, "member:read") {
		return
	}
	ms, err := h.store.Members(r.Context(), m.Org.ID)
	if err != nil {
		serverError(w, "members", err)
		return
	}
	type memberJSON struct {
		ID       uuid.UUID  `json:"id"`
		Email    string     `json:"email"`
		Name     string     `json:"name"`
		Role     store.Role `json:"role"`
		JoinedAt time.Time  `json:"joinedAt"`
	}
	out := make([]memberJSON, 0, len(ms))
	for _, mem := range ms {
		out = append(out, memberJSON{mem.User.ID, mem.User.Email, mem.User.Name, mem.Role, mem.JoinedAt})
	}
	writeJSON(w, http.StatusOK, out)
}

func (h *api) setMemberRole(w http.ResponseWriter, r *http.Request) {
	m, ok := h.orgFromPath(w, r)
	if !ok || !h.authorize(w, r, m.Org.ID, authz.ObjMembers, store.ActionMemberSetRole) {
		return
	}
	target, ok := pathID(w, r, "user")
	if !ok {
		return
	}
	role, ok := readRole(w, r)
	if !ok {
		return
	}
	if !h.commit(w, r, func(tx *store.Store) error {
		if err := tx.SetMemberRole(r.Context(), m.Org.ID, target, role); err != nil {
			return err
		}
		e := auditEntry(r, &m.Org.ID, store.ActionMemberSetRole, "org/members/"+target.String(), store.ResultAllowed)
		e.Details = map[string]any{"role": role}
		return tx.Audit(r.Context(), e)
	}) {
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// removeMember removes {user}; removing yourself is leaving and needs no permission.
func (h *api) removeMember(w http.ResponseWriter, r *http.Request) {
	m, ok := h.orgFromPath(w, r)
	if !ok {
		return
	}
	target, ok := pathID(w, r, "user")
	if !ok {
		return
	}
	p, _ := auth.PrincipalFrom(r.Context())
	action := store.ActionMemberLeave
	if target != p.User.ID {
		action = store.ActionMemberRemove
		if !h.authorize(w, r, m.Org.ID, authz.ObjMembers, store.ActionMemberRemove) {
			return
		}
		tm, err := h.store.Membership(r.Context(), m.Org.ID, target)
		if errors.Is(err, store.ErrNotFound) {
			writeError(w, http.StatusNotFound, "not found")
			return
		}
		if err != nil {
			serverError(w, "membership", err)
			return
		}
		if privileged(tm.Role) && !h.authorize(w, r, m.Org.ID, authz.ObjMembers, store.ActionMemberSetRole) {
			return
		}
	}
	if !h.commit(w, r, func(tx *store.Store) error {
		if err := tx.RemoveMember(r.Context(), m.Org.ID, target); err != nil {
			return err
		}
		return tx.Audit(r.Context(), auditEntry(r, &m.Org.ID, action, "org/members/"+target.String(), store.ResultAllowed))
	}) {
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *api) listInvites(w http.ResponseWriter, r *http.Request) {
	m, ok := h.orgFromPath(w, r)
	if !ok || !h.authorize(w, r, m.Org.ID, authz.ObjMembers, store.ActionMemberInvite) {
		return
	}
	invs, err := h.store.Invites(r.Context(), m.Org.ID)
	if err != nil {
		serverError(w, "invites", err)
		return
	}
	type inviteJSON struct {
		ID        uuid.UUID  `json:"id"`
		Role      store.Role `json:"role"`
		CreatedBy uuid.UUID  `json:"createdBy"`
		CreatedAt time.Time  `json:"createdAt"`
		ExpiresAt time.Time  `json:"expiresAt"`
	}
	out := make([]inviteJSON, 0, len(invs))
	for _, i := range invs {
		out = append(out, inviteJSON{i.ID, i.Role, i.CreatedBy, i.CreatedAt, i.ExpiresAt})
	}
	writeJSON(w, http.StatusOK, out)
}

func (h *api) createInvite(w http.ResponseWriter, r *http.Request) {
	m, ok := h.orgFromPath(w, r)
	if !ok || !h.authorize(w, r, m.Org.ID, authz.ObjMembers, store.ActionMemberInvite) {
		return
	}
	role, ok := readRole(w, r)
	if !ok {
		return
	}
	if privileged(role) && !h.authorize(w, r, m.Org.ID, authz.ObjMembers, store.ActionMemberSetRole) {
		return
	}
	p, _ := auth.PrincipalFrom(r.Context())
	var (
		token string
		inv   store.Invite
	)
	if !h.commit(w, r, func(tx *store.Store) error {
		var err error
		if token, inv, err = tx.CreateInvite(r.Context(), m.Org.ID, role, p.User.ID, inviteTTL); err != nil {
			return err
		}
		e := auditEntry(r, &m.Org.ID, store.ActionMemberInvite, "org/invites/"+inv.ID.String(), store.ResultAllowed)
		e.Details = map[string]any{"role": role}
		return tx.Audit(r.Context(), e)
	}) {
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{
		"id": inv.ID, "token": token, "url": h.publicURL.JoinPath("invite", token).String(), "expiresAt": inv.ExpiresAt,
	})
}

func (h *api) revokeInvite(w http.ResponseWriter, r *http.Request) {
	m, ok := h.orgFromPath(w, r)
	if !ok || !h.authorize(w, r, m.Org.ID, authz.ObjMembers, store.ActionMemberInvite) {
		return
	}
	id, ok := pathID(w, r, "id")
	if !ok {
		return
	}
	if !h.commit(w, r, func(tx *store.Store) error {
		if err := tx.RevokeInvite(r.Context(), m.Org.ID, id); err != nil {
			return err
		}
		return tx.Audit(r.Context(), auditEntry(r, &m.Org.ID, store.ActionInviteRevoke, "org/invites/"+id.String(), store.ResultAllowed))
	}) {
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *api) acceptInvite(w http.ResponseWriter, r *http.Request) {
	var body struct{ Token string }
	if !readJSON(w, r, &body) {
		return
	}
	p, _ := auth.PrincipalFrom(r.Context())
	var (
		org       store.Org
		role      store.Role
		acceptErr error
	)
	if !h.commit(w, r, func(tx *store.Store) error {
		if org, role, acceptErr = tx.AcceptInvite(r.Context(), body.Token, p.User.ID); acceptErr != nil {
			return acceptErr
		}
		e := auditEntry(r, &org.ID, store.ActionInviteAccept, authz.ObjMembers, store.ResultAllowed)
		e.Details = map[string]any{"role": role}
		return tx.Audit(r.Context(), e)
	}) {
		// The rejected attempt is audited outside the rolled-back transaction; the token never goes into the log.
		if errors.Is(acceptErr, store.ErrInviteInvalid) || errors.Is(acceptErr, store.ErrAlreadyMember) {
			e := auditEntry(r, nil, store.ActionInviteAccept, authz.ObjMembers, store.ResultFailed)
			e.Details = map[string]any{"reason": acceptErr.Error()}
			if err := h.store.Audit(r.Context(), e); err != nil {
				slog.Error("audit failed invite accept", "user", p.User.ID, "err", err)
			}
		}
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"orgId": org.ID, "role": role})
}

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

type teamJSON struct {
	ID          uuid.UUID `json:"id"`
	Name        string    `json:"name"`
	MemberCount int       `json:"memberCount"`
	CreatedAt   time.Time `json:"createdAt"`
}

func toTeamJSON(t store.Team) teamJSON { return teamJSON{t.ID, t.Name, t.MemberCount, t.CreatedAt} }

func (h *api) listTeams(w http.ResponseWriter, r *http.Request) {
	m, ok := h.orgFromPath(w, r)
	if !ok || !h.authorize(w, r, m.Org.ID, authz.ObjTeams, "team:read") {
		return
	}
	ts, err := h.store.Teams(r.Context(), m.Org.ID)
	if err != nil {
		serverError(w, "teams", err)
		return
	}
	out := make([]teamJSON, 0, len(ts))
	for _, t := range ts {
		out = append(out, toTeamJSON(t))
	}
	writeJSON(w, http.StatusOK, out)
}

func (h *api) createTeam(w http.ResponseWriter, r *http.Request) {
	m, ok := h.orgFromPath(w, r)
	if !ok || !h.authorize(w, r, m.Org.ID, authz.ObjTeams, store.ActionTeamCreate) {
		return
	}
	name, ok := readName(w, r)
	if !ok {
		return
	}
	var team store.Team
	if !h.commit(w, r, func(tx *store.Store) error {
		var err error
		if team, err = tx.CreateTeam(r.Context(), m.Org.ID, name); err != nil {
			return err
		}
		e := auditEntry(r, &m.Org.ID, store.ActionTeamCreate, authz.TeamObject(team.ID), store.ResultAllowed)
		e.Details = map[string]any{"name": name}
		return tx.Audit(r.Context(), e)
	}) {
		return
	}
	writeJSON(w, http.StatusCreated, toTeamJSON(team))
}

func (h *api) getTeam(w http.ResponseWriter, r *http.Request) {
	m, ok := h.orgFromPath(w, r)
	if !ok || !h.authorize(w, r, m.Org.ID, authz.ObjTeams, "team:read") {
		return
	}
	id, ok := pathID(w, r, "team")
	if !ok {
		return
	}
	team, err := h.store.Team(r.Context(), m.Org.ID, id)
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "not found")
		return
	} else if err != nil {
		serverError(w, "team", err)
		return
	}
	members, err := h.store.TeamMembers(r.Context(), m.Org.ID, id)
	if err != nil {
		serverError(w, "team members", err)
		return
	}
	type memberJSON struct {
		ID    uuid.UUID `json:"id"`
		Email string    `json:"email"`
		Name  string    `json:"name"`
		Admin bool      `json:"admin"`
	}
	ms := make([]memberJSON, 0, len(members))
	for _, tm := range members {
		ms = append(ms, memberJSON{tm.User.ID, tm.User.Email, tm.User.Name, tm.Admin})
	}
	p, _ := auth.PrincipalFrom(r.Context())
	e, sub, obj := h.z.Enforcer(), authz.UserSubject(p.User.ID), authz.TeamObject(id)
	writeJSON(w, http.StatusOK, map[string]any{
		"id": team.ID, "name": team.Name, "members": ms,
		"canManageMembers": e.Allowed(sub, m.Org.ID, obj, "team:manage-members"),
		"canUpdate":        e.Allowed(sub, m.Org.ID, obj, store.ActionTeamUpdate),
		"canDelete":        e.Allowed(sub, m.Org.ID, obj, store.ActionTeamDelete),
	})
}

func (h *api) renameTeam(w http.ResponseWriter, r *http.Request) {
	m, id, ok := h.teamAction(w, r, store.ActionTeamUpdate)
	if !ok {
		return
	}
	name, ok := readName(w, r)
	if !ok {
		return
	}
	if !h.commit(w, r, func(tx *store.Store) error {
		if err := tx.RenameTeam(r.Context(), m.Org.ID, id, name); err != nil {
			return err
		}
		e := auditEntry(r, &m.Org.ID, store.ActionTeamUpdate, authz.TeamObject(id), store.ResultAllowed)
		e.Details = map[string]any{"name": name}
		return tx.Audit(r.Context(), e)
	}) {
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *api) deleteTeam(w http.ResponseWriter, r *http.Request) {
	m, id, ok := h.teamAction(w, r, store.ActionTeamDelete)
	if !ok {
		return
	}
	if !h.commit(w, r, func(tx *store.Store) error {
		if err := tx.DeleteTeam(r.Context(), m.Org.ID, id); err != nil {
			return err
		}
		return tx.Audit(r.Context(), auditEntry(r, &m.Org.ID, store.ActionTeamDelete, authz.TeamObject(id), store.ResultAllowed))
	}) {
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *api) setTeamMember(w http.ResponseWriter, r *http.Request) {
	m, id, ok := h.teamAction(w, r, "team:manage-members")
	if !ok {
		return
	}
	user, ok := pathID(w, r, "user")
	if !ok {
		return
	}
	var body struct {
		Admin *bool `json:"admin"`
	}
	if !readJSON(w, r, &body) {
		return
	}
	if body.Admin == nil {
		writeError(w, http.StatusBadRequest, "admin is required")
		return
	}
	admin := *body.Admin
	if admin && !h.authorize(w, r, m.Org.ID, authz.TeamObject(id), store.ActionTeamUpdate) {
		return
	}
	var denied bool
	if !h.commit(w, r, func(tx *store.Store) error {
		if denied = h.demotesTeamAdmin(r, tx, m.Org.ID, id, user); denied {
			return errForbidden
		}
		if err := tx.SetTeamMember(r.Context(), m.Org.ID, id, user, admin); err != nil {
			return err
		}
		e := auditEntry(r, &m.Org.ID, store.ActionTeamMemberSet, authz.TeamObject(id)+"/members/"+user.String(), store.ResultAllowed)
		e.Details = map[string]any{"admin": admin}
		return tx.Audit(r.Context(), e)
	}) {
		if denied {
			h.auditDenied(r, m.Org.ID, authz.TeamObject(id), store.ActionTeamUpdate)
		}
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *api) removeTeamMember(w http.ResponseWriter, r *http.Request) {
	m, id, ok := h.teamAction(w, r, "team:manage-members")
	if !ok {
		return
	}
	user, ok := pathID(w, r, "user")
	if !ok {
		return
	}
	var denied bool
	if !h.commit(w, r, func(tx *store.Store) error {
		if denied = h.demotesTeamAdmin(r, tx, m.Org.ID, id, user); denied {
			return errForbidden
		}
		if err := tx.RemoveTeamMember(r.Context(), m.Org.ID, id, user); err != nil {
			return err
		}
		return tx.Audit(r.Context(), auditEntry(r, &m.Org.ID, store.ActionTeamMemberRemove, authz.TeamObject(id)+"/members/"+user.String(), store.ResultAllowed))
	}) {
		if denied {
			h.auditDenied(r, m.Org.ID, authz.TeamObject(id), store.ActionTeamUpdate)
		}
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// teamAction resolves {org} and {team} and authorizes act on that team; a malformed team ID is 404 before any check.
func (h *api) teamAction(w http.ResponseWriter, r *http.Request, act string) (store.Membership, uuid.UUID, bool) {
	m, ok := h.orgFromPath(w, r)
	if !ok {
		return m, uuid.Nil, false
	}
	id, ok := pathID(w, r, "team")
	if !ok || !h.authorize(w, r, m.Org.ID, authz.TeamObject(id), act) {
		return m, uuid.Nil, false
	}
	return m, id, true
}

// demotesTeamAdmin reports whether the target is a team admin (read under lock) and the caller lacks team:update, which changing an admin requires.
func (h *api) demotesTeamAdmin(r *http.Request, tx *store.Store, org, team, user uuid.UUID) bool {
	admin, err := tx.LockTeamMember(r.Context(), org, team, user)
	if err != nil || !admin {
		return false
	}
	p, _ := auth.PrincipalFrom(r.Context())
	return !h.z.Enforcer().Allowed(authz.UserSubject(p.User.ID), org, authz.TeamObject(team), store.ActionTeamUpdate)
}

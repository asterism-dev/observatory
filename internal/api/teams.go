package api

import (
	"errors"
	"fmt"
	"net/http"
	"slices"
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

// readTeamName refuses the qualifiers of image refs, which a team name would shadow.
func readTeamName(w http.ResponseWriter, r *http.Request) (string, bool) {
	name, ok := readName(w, r)
	if ok && slices.Contains([]string{"me", "org", "observatory"}, name) {
		writeError(w, http.StatusUnprocessableEntity, fmt.Sprintf("%q is reserved for image references", name))
		return "", false
	}
	return name, ok
}

func (h *api) createTeam(w http.ResponseWriter, r *http.Request) {
	m, ok := h.orgFromPath(w, r)
	if !ok || !h.authorize(w, r, m.Org.ID, authz.ObjTeams, store.ActionTeamCreate) {
		return
	}
	name, ok := readTeamName(w, r)
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
		"canLeave":         e.Allowed(sub, m.Org.ID, obj, store.ActionTeamLeave),
	})
}

func (h *api) renameTeam(w http.ResponseWriter, r *http.Request) {
	m, id, ok := h.teamAction(w, r, store.ActionTeamUpdate)
	if !ok {
		return
	}
	name, ok := readTeamName(w, r)
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
	p, _ := auth.PrincipalFrom(r.Context())
	actor := authz.UserSubject(p.User.ID)
	var denied error
	var images map[uuid.UUID][]string
	if !h.commit(w, r, func(tx *store.Store) error {
		cur, e, err := lockedPolicy(r, tx, m.Org.ID)
		if err != nil {
			return err
		}
		if !privileged(cur.Role) {
			if denied = teamCarriesDeny(r, tx, e, m.Org.ID, id); denied != nil {
				return denied
			}
		}
		if images, err = ownedImageVersions(r.Context(), tx, m.Org.ID, store.EnvScopeTeam, id); err != nil {
			return err
		}
		if denied = guardOwnDenies(r.Context(), tx, cur, actor, func() error { return tx.DeleteTeam(r.Context(), m.Org.ID, id) }); denied != nil {
			return denied
		}
		return tx.Audit(r.Context(), auditEntry(r, &m.Org.ID, store.ActionTeamDelete, authz.TeamObject(id), store.ResultAllowed))
	}) {
		h.auditGuardDenial(r, m.Org.ID, store.ActionTeamDelete, authz.TeamObject(id), denied)
		return
	}
	for image, digests := range images {
		h.deleteManifests(r.Context(), image, digests)
	}
	h.fleet.NotifyOrg(m.Org.ID)
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
	p, _ := auth.PrincipalFrom(r.Context())
	obj := authz.TeamObject(id) + "/members/" + user.String()
	var (
		denied   bool
		guardErr error
	)
	if !h.commit(w, r, func(tx *store.Store) error {
		cur, e, err := lockedPolicy(r, tx, m.Org.ID)
		if err != nil {
			return err
		}
		if denied, err = demotesTeamAdmin(r, tx, e, m.Org.ID, id, user); err != nil {
			return err
		} else if denied {
			return errForbidden
		}
		if guardErr = checkTeamRoles(r, tx, e, cur, authz.UserSubject(p.User.ID), id); guardErr != nil {
			return guardErr
		}
		if err := tx.SetTeamMember(r.Context(), m.Org.ID, id, user, admin); err != nil {
			return err
		}
		ae := auditEntry(r, &m.Org.ID, store.ActionTeamMemberSet, obj, store.ResultAllowed)
		ae.Details = map[string]any{"admin": admin}
		return tx.Audit(r.Context(), ae)
	}) {
		if denied {
			h.auditDenied(r, m.Org.ID, authz.TeamObject(id), store.ActionTeamUpdate)
		}
		h.auditGuardDenial(r, m.Org.ID, store.ActionTeamMemberSet, obj, guardErr)
		return
	}
	h.fleet.NotifyOrg(m.Org.ID)
	w.WriteHeader(http.StatusNoContent)
}

func (h *api) removeTeamMember(w http.ResponseWriter, r *http.Request) {
	p, _ := auth.PrincipalFrom(r.Context())
	// Leaving needs team:leave instead of team:manage-members; the own-deny guard below still applies.
	leaving := r.PathValue("user") == p.User.ID.String()
	act, auditAct := "team:manage-members", store.ActionTeamMemberRemove
	if leaving {
		act, auditAct = store.ActionTeamLeave, store.ActionTeamLeave
	}
	m, id, ok := h.teamAction(w, r, act)
	if !ok {
		return
	}
	user, ok := pathID(w, r, "user")
	if !ok {
		return
	}
	obj := authz.TeamObject(id) + "/members/" + user.String()
	var (
		denied, leaveDenied bool
		guardErr            error
	)
	if !h.commit(w, r, func(tx *store.Store) error {
		cur, e, err := lockedPolicy(r, tx, m.Org.ID)
		if err != nil {
			return err
		}
		if leaving {
			if leaveDenied = !e.Allowed(authz.UserSubject(p.User.ID), m.Org.ID, authz.TeamObject(id), store.ActionTeamLeave); leaveDenied {
				return errForbidden
			}
		} else {
			if denied, err = demotesTeamAdmin(r, tx, e, m.Org.ID, id, user); err != nil {
				return err
			} else if denied {
				return errForbidden
			}
			// Removing someone from a team lifts its deny rules from them, which only org owners and admins may do.
			if !privileged(cur.Role) {
				if guardErr = teamCarriesDeny(r, tx, e, m.Org.ID, id); guardErr != nil {
					return guardErr
				}
			}
		}
		if guardErr = guardOwnDenies(r.Context(), tx, cur, authz.UserSubject(p.User.ID), func() error { return tx.RemoveTeamMember(r.Context(), m.Org.ID, id, user) }); guardErr != nil {
			return guardErr
		}
		return tx.Audit(r.Context(), auditEntry(r, &m.Org.ID, auditAct, obj, store.ResultAllowed))
	}) {
		if denied {
			h.auditDenied(r, m.Org.ID, authz.TeamObject(id), store.ActionTeamUpdate)
		}
		if leaveDenied {
			h.auditDenied(r, m.Org.ID, authz.TeamObject(id), store.ActionTeamLeave)
		}
		h.auditGuardDenial(r, m.Org.ID, auditAct, obj, guardErr)
		return
	}
	h.fleet.NotifyOrg(m.Org.ID)
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
func demotesTeamAdmin(r *http.Request, tx *store.Store, e *authz.Enforcer, org, team, user uuid.UUID) (bool, error) {
	admin, err := tx.LockTeamMember(r.Context(), org, team, user)
	if errors.Is(err, store.ErrNotFound) {
		return false, nil
	}
	if err != nil || !admin {
		return false, err
	}
	p, _ := auth.PrincipalFrom(r.Context())
	return !e.Allowed(authz.UserSubject(p.User.ID), org, authz.TeamObject(team), store.ActionTeamUpdate), nil
}

// teamRoles returns the IDs of the custom roles bound to team.
func teamRoles(r *http.Request, tx *store.Store, org, team uuid.UUID) ([]uuid.UUID, error) {
	bs, err := tx.Bindings(r.Context(), org)
	if err != nil {
		return nil, err
	}
	var ids []uuid.UUID
	for _, b := range bs {
		if b.Subject == authz.TeamSubject(team) {
			ids = append(ids, b.RoleID)
		}
	}
	return ids, nil
}

// checkTeamRoles applies the bind guard to every role bound to team, since adding a member grants them those roles.
func checkTeamRoles(r *http.Request, tx *store.Store, e *authz.Enforcer, m store.Membership, actor string, team uuid.UUID) error {
	ids, err := teamRoles(r, tx, m.Org.ID, team)
	if err != nil || len(ids) == 0 {
		return err
	}
	keys := make([]string, 0, len(ids))
	for _, id := range ids {
		keys = append(keys, id.String())
	}
	if err := e.CanGrantRole(actor, m.Org.ID, privileged(m.Role), nil, keys); err != nil {
		return err
	}
	if privileged(m.Role) {
		return nil
	}
	for _, id := range ids {
		if err := carriesDeny(e, m.Org.ID, id); err != nil {
			return err
		}
	}
	return nil
}

// teamCarriesDeny fails with ErrDenyNotAllowed if a role bound to team holds deny rules.
func teamCarriesDeny(r *http.Request, tx *store.Store, e *authz.Enforcer, org, team uuid.UUID) error {
	ids, err := teamRoles(r, tx, org, team)
	if err != nil {
		return err
	}
	for _, id := range ids {
		if err := carriesDeny(e, org, id); err != nil {
			return err
		}
	}
	return nil
}

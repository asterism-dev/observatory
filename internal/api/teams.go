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
	if !ok || !h.authorize(w, r, m.OrgID, authz.ObjTeams, "iam.teams.list") {
		return
	}
	ts, err := h.store.Teams(r.Context(), m.OrgID)
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
	if !ok || !h.authorize(w, r, m.OrgID, authz.ObjTeams, "iam.teams.create") {
		return
	}
	name, ok := readTeamName(w, r)
	if !ok {
		return
	}
	var team store.Team
	if !h.commit(w, r, func(tx *store.Store) error {
		var err error
		if team, err = tx.CreateTeam(r.Context(), m.OrgID, name); err != nil {
			return err
		}
		e := auditEntry(r, &m.OrgID, store.ActionTeamCreate, authz.TeamObject(team.ID), store.ResultAllowed)
		e.Details = map[string]any{"name": name}
		return tx.Audit(r.Context(), e)
	}) {
		return
	}
	writeJSON(w, http.StatusCreated, toTeamJSON(team))
}

func (h *api) getTeam(w http.ResponseWriter, r *http.Request) {
	m, ok := h.orgFromPath(w, r)
	if !ok || !h.authorize(w, r, m.OrgID, authz.ObjTeams, "iam.teams.list") {
		return
	}
	id, ok := pathID(w, r, "team")
	if !ok {
		return
	}
	team, err := h.store.Team(r.Context(), m.OrgID, id)
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "not found")
		return
	} else if err != nil {
		serverError(w, "team", err)
		return
	}
	members, err := h.store.TeamMembers(r.Context(), m.OrgID, id)
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
		"canManageMembers": e.Allowed(sub, m.OrgID, obj, "iam.teams.manageMembers"),
		"canUpdate":        e.Allowed(sub, m.OrgID, obj, "iam.teams.update"),
		"canDelete":        e.Allowed(sub, m.OrgID, obj, "iam.teams.delete"),
		"canLeave":         e.Allowed(sub, m.OrgID, obj, "iam.teams.leave"),
	})
}

func (h *api) renameTeam(w http.ResponseWriter, r *http.Request) {
	m, id, ok := h.teamAction(w, r, "iam.teams.update")
	if !ok {
		return
	}
	name, ok := readTeamName(w, r)
	if !ok {
		return
	}
	if !h.commit(w, r, func(tx *store.Store) error {
		if err := tx.RenameTeam(r.Context(), m.OrgID, id, name); err != nil {
			return err
		}
		e := auditEntry(r, &m.OrgID, store.ActionTeamUpdate, authz.TeamObject(id), store.ResultAllowed)
		e.Details = map[string]any{"name": name}
		return tx.Audit(r.Context(), e)
	}) {
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *api) deleteTeam(w http.ResponseWriter, r *http.Request) {
	m, id, ok := h.teamAction(w, r, "iam.teams.delete")
	if !ok {
		return
	}
	p, _ := auth.PrincipalFrom(r.Context())
	actor := authz.UserSubject(p.User.ID)
	var denied error
	var images map[uuid.UUID][]string
	if !h.commit(w, r, func(tx *store.Store) error {
		cur, e, err := lockedPolicy(r, tx, m.OrgID)
		if err != nil {
			return err
		}
		if !privileged(cur.Role) {
			if denied = teamCarriesDeny(r, tx, e, m.OrgID, id); denied != nil {
				return denied
			}
		}
		if err := h.checkOwnership(r.Context(), m.OrgID, authz.TeamSubject(id)); err != nil {
			return err
		}
		if images, err = ownedImageVersions(r.Context(), tx, m.OrgID, store.EnvScopeTeam, id); err != nil {
			return err
		}
		if denied = guardOwnDenies(r.Context(), tx, cur, actor, func() error { return tx.DeleteTeam(r.Context(), m.OrgID, id) }); denied != nil {
			return denied
		}
		return tx.Audit(r.Context(), auditEntry(r, &m.OrgID, store.ActionTeamDelete, authz.TeamObject(id), store.ResultAllowed))
	}) {
		h.auditGuardDenial(r, m.OrgID, store.ActionTeamDelete, authz.TeamObject(id), denied)
		return
	}
	for image, digests := range images {
		h.deleteManifests(r.Context(), image, digests)
	}
	h.fleet.NotifyOrg(m.OrgID)
	w.WriteHeader(http.StatusNoContent)
}

func (h *api) setTeamMember(w http.ResponseWriter, r *http.Request) {
	m, id, ok := h.teamAction(w, r, "iam.teams.manageMembers")
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
	if admin && !h.authorize(w, r, m.OrgID, authz.TeamObject(id), "iam.teams.update") {
		return
	}
	p, _ := auth.PrincipalFrom(r.Context())
	obj := authz.TeamObject(id) + "/members/" + user.String()
	var (
		denied   bool
		guardErr error
	)
	if !h.commit(w, r, func(tx *store.Store) error {
		cur, e, err := lockedPolicy(r, tx, m.OrgID)
		if err != nil {
			return err
		}
		if denied, err = demotesTeamAdmin(r, tx, e, m.OrgID, id, user); err != nil {
			return err
		} else if denied {
			return errForbidden
		}
		if guardErr = checkTeamRoles(r, tx, e, cur, authz.UserSubject(p.User.ID), id); guardErr != nil {
			return guardErr
		}
		if err := tx.SetTeamMember(r.Context(), m.OrgID, id, user, admin); err != nil {
			return err
		}
		ae := auditEntry(r, &m.OrgID, store.ActionTeamMemberSet, obj, store.ResultAllowed)
		ae.Details = map[string]any{"admin": admin}
		return tx.Audit(r.Context(), ae)
	}) {
		if denied {
			h.auditDenied(r, m.OrgID, authz.TeamObject(id), store.ActionTeamUpdate)
		}
		h.auditGuardDenial(r, m.OrgID, store.ActionTeamMemberSet, obj, guardErr)
		return
	}
	h.fleet.NotifyOrg(m.OrgID)
	w.WriteHeader(http.StatusNoContent)
}

func (h *api) removeTeamMember(w http.ResponseWriter, r *http.Request) {
	p, _ := auth.PrincipalFrom(r.Context())
	// Leaving needs iam.teams.leave instead of iam.teams.manageMembers; the own-deny guard below still applies.
	leaving := r.PathValue("user") == p.User.ID.String()
	act, auditAct := "iam.teams.manageMembers", store.ActionTeamMemberRemove
	if leaving {
		act, auditAct = "iam.teams.leave", store.ActionTeamLeave
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
		cur, e, err := lockedPolicy(r, tx, m.OrgID)
		if err != nil {
			return err
		}
		if leaving {
			if leaveDenied = !e.Allowed(authz.UserSubject(p.User.ID), m.OrgID, authz.TeamObject(id), "iam.teams.leave"); leaveDenied {
				return errForbidden
			}
		} else {
			if denied, err = demotesTeamAdmin(r, tx, e, m.OrgID, id, user); err != nil {
				return err
			} else if denied {
				return errForbidden
			}
			// Removing someone from a team lifts its deny rules from them, which only org owners and admins may do.
			if !privileged(cur.Role) {
				if guardErr = teamCarriesDeny(r, tx, e, m.OrgID, id); guardErr != nil {
					return guardErr
				}
			}
		}
		if guardErr = guardOwnDenies(r.Context(), tx, cur, authz.UserSubject(p.User.ID), func() error { return tx.RemoveTeamMember(r.Context(), m.OrgID, id, user) }); guardErr != nil {
			return guardErr
		}
		return tx.Audit(r.Context(), auditEntry(r, &m.OrgID, auditAct, obj, store.ResultAllowed))
	}) {
		if denied {
			h.auditDenied(r, m.OrgID, authz.TeamObject(id), store.ActionTeamUpdate)
		}
		if leaveDenied {
			h.auditDenied(r, m.OrgID, authz.TeamObject(id), store.ActionTeamLeave)
		}
		h.auditGuardDenial(r, m.OrgID, auditAct, obj, guardErr)
		return
	}
	h.fleet.NotifyOrg(m.OrgID)
	w.WriteHeader(http.StatusNoContent)
}

// teamAction resolves {org} and {team} and authorizes act on that team; a malformed team ID is 404 before any check.
func (h *api) teamAction(w http.ResponseWriter, r *http.Request, act string) (orgMember, uuid.UUID, bool) {
	m, ok := h.orgFromPath(w, r)
	if !ok {
		return m, uuid.Nil, false
	}
	id, ok := pathID(w, r, "team")
	if !ok || !h.authorize(w, r, m.OrgID, authz.TeamObject(id), act) {
		return m, uuid.Nil, false
	}
	return m, id, true
}

// demotesTeamAdmin reports whether the target is a team admin (read under lock) and the caller lacks iam.teams.update, which changing an admin requires.
func demotesTeamAdmin(r *http.Request, tx *store.Store, e *authz.Enforcer, org, team, user uuid.UUID) (bool, error) {
	admin, err := tx.LockTeamMember(r.Context(), org, team, user)
	if errors.Is(err, store.ErrNotFound) {
		return false, nil
	}
	if err != nil || !admin {
		return false, err
	}
	p, _ := auth.PrincipalFrom(r.Context())
	return !e.Allowed(authz.UserSubject(p.User.ID), org, authz.TeamObject(team), "iam.teams.update"), nil
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
func checkTeamRoles(r *http.Request, tx *store.Store, e *authz.Enforcer, m orgMember, actor string, team uuid.UUID) error {
	ids, err := teamRoles(r, tx, m.OrgID, team)
	if err != nil || len(ids) == 0 {
		return err
	}
	keys := make([]string, 0, len(ids))
	for _, id := range ids {
		keys = append(keys, id.String())
	}
	if err := e.CanGrantRole(actor, m.OrgID, privileged(m.Role), nil, keys); err != nil {
		return err
	}
	if privileged(m.Role) {
		return nil
	}
	for _, id := range ids {
		if err := carriesDeny(e, m.OrgID, id); err != nil {
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

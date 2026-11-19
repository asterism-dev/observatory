package api

import (
	"errors"
	"net/http"
	"regexp"
	"slices"
	"time"

	"github.com/google/uuid"

	"github.com/asterism-dev/observatory/internal/auth"
	"github.com/asterism-dev/observatory/internal/authz"
	"github.com/asterism-dev/observatory/internal/egress"
	"github.com/asterism-dev/observatory/internal/store"
)

var envNamePattern = regexp.MustCompile(`^[A-Z_][A-Z0-9_]*$`)

const maxEnvValue = 64 << 10

func validEnvName(s string) bool { return len(s) <= 128 && envNamePattern.MatchString(s) }

func validEnvKind(k store.EnvKind) bool { return k == store.EnvPlain || k == store.EnvSecret }

func envObject(scope store.EnvScope, owner *uuid.UUID) string {
	switch scope {
	case store.EnvScopeTeam:
		return authz.TeamEnvObject(*owner)
	case store.EnvScopeUser:
		return authz.UserEnvObject(*owner)
	}
	return authz.ObjEnv
}

func envAudit(r *http.Request, action string, v store.EnvVar) store.AuditEntry {
	e := auditEntry(r, &v.OrgID, action, envObject(v.Scope, v.OwnerID), store.ResultAllowed)
	e.Details = map[string]any{"name": v.Name, "kind": v.Kind, "scope": v.Scope, "ownerId": v.OwnerID}
	if len(v.Hosts) > 0 {
		e.Details["hosts"] = v.Hosts
	}
	return e
}

func (h *api) listEnv(w http.ResponseWriter, r *http.Request) {
	m, ok := h.orgFromPath(w, r)
	if !ok {
		return
	}
	ctx := r.Context()
	vars, err := h.store.EnvVars(ctx, m.Org.ID)
	if err != nil {
		serverError(w, "env vars", err)
		return
	}
	teams, err := h.store.Teams(ctx, m.Org.ID)
	if err != nil {
		serverError(w, "teams", err)
		return
	}
	members, err := h.store.Members(ctx, m.Org.ID)
	if err != nil {
		serverError(w, "members", err)
		return
	}
	teamNames := make(map[uuid.UUID]string, len(teams))
	for _, t := range teams {
		teamNames[t.ID] = t.Name
	}
	userNames := make(map[uuid.UUID]string, len(members))
	for _, mb := range members {
		userNames[mb.User.ID] = mb.User.Name
	}
	type userRef struct {
		ID   uuid.UUID `json:"id"`
		Name string    `json:"name"`
	}
	type varJSON struct {
		ID        uuid.UUID      `json:"id"`
		Name      string         `json:"name"`
		Kind      store.EnvKind  `json:"kind"`
		Scope     store.EnvScope `json:"scope"`
		TeamID    *uuid.UUID     `json:"teamId,omitempty"`
		TeamName  string         `json:"teamName,omitempty"`
		Value     *string        `json:"value,omitempty"`
		Hosts     []string       `json:"hosts"`
		UpdatedAt time.Time      `json:"updatedAt"`
		UpdatedBy userRef        `json:"updatedBy"`
		CanEdit   bool           `json:"canEdit"`
		CanDelete bool           `json:"canDelete"`
	}
	type scopeJSON struct {
		Scope    store.EnvScope `json:"scope"`
		TeamID   *uuid.UUID     `json:"teamId,omitempty"`
		TeamName string         `json:"teamName,omitempty"`
	}
	p, _ := auth.PrincipalFrom(ctx)
	e, sub := h.z.Enforcer(), authz.UserSubject(p.User.ID)
	out := []varJSON{}
	for _, v := range vars {
		// Another member's private scope is never listed, whatever rules a custom role grants.
		if v.Scope == store.EnvScopeUser && *v.OwnerID != p.User.ID {
			continue
		}
		obj := envObject(v.Scope, v.OwnerID)
		if !e.Allowed(sub, m.Org.ID, obj, "env:list") {
			continue
		}
		j := varJSON{ID: v.ID, Name: v.Name, Kind: v.Kind, Scope: v.Scope, Hosts: append([]string{}, v.Hosts...), UpdatedAt: v.UpdatedAt,
			UpdatedBy: userRef{v.UpdatedBy, userNames[v.UpdatedBy]},
			CanEdit:   e.Allowed(sub, m.Org.ID, obj, store.ActionEnvSet),
			CanDelete: e.Allowed(sub, m.Org.ID, obj, store.ActionEnvDelete)}
		if v.Scope == store.EnvScopeTeam {
			j.TeamID, j.TeamName = v.OwnerID, teamNames[*v.OwnerID]
		}
		// ponytail: one decrypt query per plain var; batch in the store if lists get large.
		if v.Kind == store.EnvPlain {
			val, err := h.store.EnvValue(ctx, v)
			if err != nil {
				serverError(w, "env value", err)
				return
			}
			j.Value = &val
		}
		out = append(out, j)
	}
	scopes := []scopeJSON{}
	if e.Allowed(sub, m.Org.ID, authz.ObjEnv, store.ActionEnvSet) {
		scopes = append(scopes, scopeJSON{Scope: store.EnvScopeOrg})
	}
	for _, t := range teams {
		if e.Allowed(sub, m.Org.ID, authz.TeamEnvObject(t.ID), store.ActionEnvSet) {
			scopes = append(scopes, scopeJSON{store.EnvScopeTeam, &t.ID, t.Name})
		}
	}
	if e.Allowed(sub, m.Org.ID, authz.UserEnvObject(p.User.ID), store.ActionEnvSet) {
		scopes = append(scopes, scopeJSON{Scope: store.EnvScopeUser})
	}
	writeJSON(w, http.StatusOK, map[string]any{"vars": out, "writableScopes": scopes})
}

// envTarget resolves a requested scope to its owner; user scope is always the caller's own.
func (h *api) envTarget(w http.ResponseWriter, r *http.Request, org uuid.UUID, scope store.EnvScope, teamID *uuid.UUID) (*uuid.UUID, bool) {
	switch scope {
	case store.EnvScopeOrg:
		return nil, true
	case store.EnvScopeUser:
		p, _ := auth.PrincipalFrom(r.Context())
		return &p.User.ID, true
	case store.EnvScopeTeam:
		if teamID != nil {
			_, err := h.store.Team(r.Context(), org, *teamID)
			if err == nil {
				return teamID, true
			}
			if !errors.Is(err, store.ErrNotFound) {
				serverError(w, "team", err)
				return nil, false
			}
		}
		writeError(w, http.StatusBadRequest, "invalid team")
		return nil, false
	}
	writeError(w, http.StatusBadRequest, "invalid scope")
	return nil, false
}

// envFromPath loads {id}; another member's private var is 404 so its existence doesn't leak.
func (h *api) envFromPath(w http.ResponseWriter, r *http.Request) (store.EnvVar, bool) {
	m, ok := h.orgFromPath(w, r)
	if !ok {
		return store.EnvVar{}, false
	}
	id, ok := pathID(w, r, "id")
	if !ok {
		return store.EnvVar{}, false
	}
	v, err := h.store.EnvVar(r.Context(), m.Org.ID, id)
	p, _ := auth.PrincipalFrom(r.Context())
	if errors.Is(err, store.ErrNotFound) || (err == nil && v.Scope == store.EnvScopeUser && *v.OwnerID != p.User.ID) {
		writeError(w, http.StatusNotFound, "not found")
		return store.EnvVar{}, false
	} else if err != nil {
		serverError(w, "env var", err)
		return store.EnvVar{}, false
	}
	return v, true
}

func (h *api) createEnv(w http.ResponseWriter, r *http.Request) {
	m, ok := h.orgFromPath(w, r)
	if !ok {
		return
	}
	var body struct {
		Name, Value string
		Kind        store.EnvKind
		Scope       store.EnvScope
		TeamID      *uuid.UUID
		Hosts       []string
	}
	if !readJSON(w, r, &body) {
		return
	}
	hosts, err := egress.NormalizeHosts(body.Hosts)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	switch {
	case !validEnvName(body.Name):
		writeError(w, http.StatusBadRequest, "invalid name")
		return
	case len(body.Value) > maxEnvValue:
		writeError(w, http.StatusBadRequest, "value too long")
		return
	case !validEnvKind(body.Kind):
		writeError(w, http.StatusBadRequest, "invalid kind")
		return
	}
	owner, ok := h.envTarget(w, r, m.Org.ID, body.Scope, body.TeamID)
	if !ok || !h.authorize(w, r, m.Org.ID, envObject(body.Scope, owner), store.ActionEnvSet) {
		return
	}
	p, _ := auth.PrincipalFrom(r.Context())
	v := store.EnvVar{OrgID: m.Org.ID, Kind: body.Kind, Scope: body.Scope, OwnerID: owner, Name: body.Name, Hosts: hosts, CreatedBy: p.User.ID}
	if !h.commit(w, r, func(tx *store.Store) error {
		var err error
		if v, err = tx.CreateEnvVar(r.Context(), v, body.Value); err != nil {
			return err
		}
		e := envAudit(r, store.ActionEnvSet, v)
		e.Details["created"] = true
		return tx.Audit(r.Context(), e)
	}) {
		return
	}
	h.fleet.NotifyOrg(m.Org.ID)
	writeJSON(w, http.StatusCreated, map[string]any{"id": v.ID})
}

func (h *api) updateEnv(w http.ResponseWriter, r *http.Request) {
	old, ok := h.envFromPath(w, r)
	if !ok || !h.authorize(w, r, old.OrgID, envObject(old.Scope, old.OwnerID), store.ActionEnvSet) {
		return
	}
	v := old
	var body struct {
		Name, Value *string
		Kind        *store.EnvKind
		Hosts       *[]string
	}
	if !readJSON(w, r, &body) {
		return
	}
	if body.Hosts != nil {
		hosts, err := egress.NormalizeHosts(*body.Hosts)
		if err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		v.Hosts = hosts
	}
	if body.Name != nil {
		if !validEnvName(*body.Name) {
			writeError(w, http.StatusBadRequest, "invalid name")
			return
		}
		v.Name = *body.Name
	}
	if body.Value != nil && len(*body.Value) > maxEnvValue {
		writeError(w, http.StatusBadRequest, "value too long")
		return
	}
	if body.Kind != nil {
		if !validEnvKind(*body.Kind) {
			writeError(w, http.StatusBadRequest, "invalid kind")
			return
		}
		// Turning a secret into a plain var would make its value readable.
		if v.Kind == store.EnvSecret && *body.Kind == store.EnvPlain {
			writeError(w, http.StatusBadRequest, "a secret cannot become a plain variable")
			return
		}
		v.Kind = *body.Kind
	}
	p, _ := auth.PrincipalFrom(r.Context())
	v.UpdatedBy = p.User.ID
	if !h.commit(w, r, func(tx *store.Store) error {
		if err := tx.UpdateEnvVar(r.Context(), old, v, body.Value); err != nil {
			return err
		}
		e := envAudit(r, store.ActionEnvSet, v)
		e.Details["created"] = false
		if body.Hosts != nil {
			e.Details["hosts"] = append([]string{}, v.Hosts...)
		}
		return tx.Audit(r.Context(), e)
	}) {
		return
	}
	h.fleet.NotifyOrg(old.OrgID)
	w.WriteHeader(http.StatusNoContent)
}

func (h *api) moveEnv(w http.ResponseWriter, r *http.Request) {
	old, ok := h.envFromPath(w, r)
	if !ok || !h.authorize(w, r, old.OrgID, envObject(old.Scope, old.OwnerID), store.ActionEnvDelete) {
		return
	}
	var body struct {
		Scope  store.EnvScope
		TeamID *uuid.UUID
	}
	if !readJSON(w, r, &body) {
		return
	}
	owner, ok := h.envTarget(w, r, old.OrgID, body.Scope, body.TeamID)
	if !ok || !h.authorize(w, r, old.OrgID, envObject(body.Scope, owner), store.ActionEnvSet) {
		return
	}
	p, _ := auth.PrincipalFrom(r.Context())
	v := old
	v.Scope, v.OwnerID, v.UpdatedBy = body.Scope, owner, p.User.ID
	if !h.commit(w, r, func(tx *store.Store) error {
		if err := tx.MoveEnvVar(r.Context(), old, v); err != nil {
			return err
		}
		return tx.Audit(r.Context(), envAudit(r, store.ActionEnvMove, v))
	}) {
		return
	}
	h.fleet.NotifyOrg(old.OrgID)
	w.WriteHeader(http.StatusNoContent)
}

func (h *api) deleteEnv(w http.ResponseWriter, r *http.Request) {
	v, ok := h.envFromPath(w, r)
	if !ok || !h.authorize(w, r, v.OrgID, envObject(v.Scope, v.OwnerID), store.ActionEnvDelete) {
		return
	}
	if !h.commit(w, r, func(tx *store.Store) error {
		if err := tx.DeleteEnvVar(r.Context(), v); err != nil {
			return err
		}
		return tx.Audit(r.Context(), envAudit(r, store.ActionEnvDelete, v))
	}) {
		return
	}
	h.fleet.NotifyOrg(v.OrgID)
	w.WriteHeader(http.StatusNoContent)
}

func (h *api) vmEgress(w http.ResponseWriter, r *http.Request) {
	v, ok := h.vmFromPath(w, r, "vm:read")
	if !ok {
		return
	}
	ctx := r.Context()
	owner := ""
	switch {
	case v.UserID != nil:
		owner = authz.UserSubject(*v.UserID)
	case v.TeamID != nil:
		owner = authz.TeamSubject(*v.TeamID)
	}
	_, bound, err := h.store.ResolveVMEnv(ctx, h.z.Enforcer(), v.OrgID, owner)
	if err != nil {
		serverError(w, "vm egress env", err)
		return
	}
	cas, err := h.store.EgressCAInfos(ctx, v.ID, time.Now())
	if err != nil {
		serverError(w, "vm egress cas", err)
		return
	}
	hostSet := map[string]bool{}
	for _, b := range bound {
		for _, host := range b.Hosts {
			hostSet[host] = true
		}
	}
	hosts := make([]string, 0, len(hostSet))
	for host := range hostSet {
		hosts = append(hosts, host)
	}
	slices.Sort(hosts)
	// The oldest CA is the one a long-running guest process may still hold.
	var notAfter *time.Time
	trustChanged := false
	if len(cas) > 0 {
		notAfter = &cas[0].NotAfter
		trustChanged = slices.ContainsFunc(hosts, func(host string) bool { return !slices.Contains(cas[0].Hosts, host) })
	}
	writeJSON(w, http.StatusOK, map[string]any{"secrets": len(bound), "hosts": hosts, "caNotAfter": notAfter, "trustChanged": trustChanged})
}

package api

import (
	"crypto/subtle"
	"errors"
	"log/slog"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/asterism-dev/observatory/internal/auth"
	"github.com/asterism-dev/observatory/internal/authz"
	"github.com/asterism-dev/observatory/internal/integrations"
	"github.com/asterism-dev/observatory/internal/store"
)

// Integrations holds the provider clients; tests and dev mode swap in fakes.
type Integrations struct {
	Verifier integrations.Verifier
	OAuth    integrations.ClaudeOAuth
}

type signIn struct {
	Scope     store.EnvScope
	OwnerID   *uuid.UUID
	Mode      integrations.Mode
	ReplaceID *uuid.UUID
}

const signInTTL = 10 * time.Minute

func integrationObject(scope store.EnvScope, owner *uuid.UUID) string {
	switch scope {
	case store.EnvScopeTeam:
		return authz.TeamIntegrationsObject(*owner)
	case store.EnvScopeUser:
		return authz.UserIntegrationsObject(*owner)
	}
	return authz.ObjIntegrations
}

func integrationAudit(r *http.Request, action string, v store.Integration) store.AuditEntry {
	e := auditEntry(r, &v.OrgID, action, integrationObject(v.Scope, v.OwnerID), store.ResultAllowed)
	e.Details = map[string]any{"kind": v.Kind, "source": v.Source, "scope": v.Scope, "ownerId": v.OwnerID, "mode": v.Mode, "account": v.Account}
	return e
}

func validMode(m integrations.Mode) bool {
	return m == integrations.ModeProxy || m == integrations.ModeVM
}

// pastedSource reports whether src is a token the user may paste for kind; oauth tokens only come from the sign-in.
func pastedSource(kind integrations.Kind, src integrations.Source) bool {
	spec, ok := integrations.Kinds[kind]
	return ok && src != integrations.SourceOAuth && slices.Contains(spec.Sources, src)
}

// providerError maps verifier and OAuth errors to fixed messages; their texts name only kind and HTTP status.
func providerError(w http.ResponseWriter, err error, rejected string) {
	switch {
	case errors.Is(err, integrations.ErrRejected), errors.Is(err, integrations.ErrInvalidGrant):
		slog.Info("integration provider refused", "err", err)
		writeError(w, http.StatusBadRequest, rejected)
	case errors.Is(err, integrations.ErrUnreachable):
		slog.Warn("integration provider unreachable", "err", err)
		writeError(w, http.StatusBadGateway, "provider unreachable")
	default:
		serverError(w, "integration provider", err)
	}
}

func (h *api) verify(w http.ResponseWriter, r *http.Request, kind integrations.Kind, src integrations.Source, raw string) (string, integrations.Account, bool) {
	token, err := integrations.NormalizeToken(raw)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid token")
		return "", integrations.Account{}, false
	}
	acc, err := h.ig.Verifier.Verify(r.Context(), kind, src, token)
	if err != nil {
		providerError(w, err, "verification failed: provider rejected the token")
		return "", integrations.Account{}, false
	}
	return token, acc, true
}

// loadIntegration returns 404 for another member's private integration so its existence doesn't leak.
func (h *api) loadIntegration(w http.ResponseWriter, r *http.Request, org, id uuid.UUID) (store.Integration, bool) {
	v, err := h.store.Integration(r.Context(), org, id)
	p, _ := auth.PrincipalFrom(r.Context())
	if errors.Is(err, store.ErrNotFound) || (err == nil && v.Scope == store.EnvScopeUser && *v.OwnerID != p.User.ID) {
		writeError(w, http.StatusNotFound, "not found")
		return store.Integration{}, false
	} else if err != nil {
		serverError(w, "integration", err)
		return store.Integration{}, false
	}
	return v, true
}

func (h *api) integrationFromPath(w http.ResponseWriter, r *http.Request) (store.Integration, bool) {
	m, ok := h.orgFromPath(w, r)
	if !ok {
		return store.Integration{}, false
	}
	id, ok := pathID(w, r, "id")
	if !ok {
		return store.Integration{}, false
	}
	return h.loadIntegration(w, r, m.Org.ID, id)
}

func (h *api) listIntegrations(w http.ResponseWriter, r *http.Request) {
	m, ok := h.orgFromPath(w, r)
	if !ok {
		return
	}
	ctx := r.Context()
	list, err := h.store.Integrations(ctx, m.Org.ID)
	if err != nil {
		serverError(w, "integrations", err)
		return
	}
	teams, err := h.store.Teams(ctx, m.Org.ID)
	if err != nil {
		serverError(w, "teams", err)
		return
	}
	teamNames := make(map[uuid.UUID]string, len(teams))
	for _, t := range teams {
		teamNames[t.ID] = t.Name
	}
	type integrationJSON struct {
		ID                uuid.UUID           `json:"id"`
		Kind              integrations.Kind   `json:"kind"`
		Source            integrations.Source `json:"source"`
		Scope             store.EnvScope      `json:"scope"`
		TeamID            *uuid.UUID          `json:"teamId,omitempty"`
		TeamName          string              `json:"teamName,omitempty"`
		Mode              integrations.Mode   `json:"mode"`
		Account           string              `json:"account"`
		Scopes            string              `json:"scopes"`
		ProviderExpiresAt *time.Time          `json:"providerExpiresAt"`
		Status            string              `json:"status"`
		LastError         string              `json:"lastError"`
		WasInVM           bool                `json:"wasInVm"`
		UpdatedAt         time.Time           `json:"updatedAt"`
		CanEdit           bool                `json:"canEdit"`
		CanDelete         bool                `json:"canDelete"`
	}
	type scopeJSON struct {
		Scope     store.EnvScope      `json:"scope"`
		TeamID    *uuid.UUID          `json:"teamId,omitempty"`
		TeamName  string              `json:"teamName,omitempty"`
		Connected []integrations.Kind `json:"connected"`
	}
	p, _ := auth.PrincipalFrom(ctx)
	e, sub := h.z.Enforcer(), authz.UserSubject(p.User.ID)
	connected := map[string][]integrations.Kind{}
	out := []integrationJSON{}
	for _, v := range list {
		obj := integrationObject(v.Scope, v.OwnerID)
		connected[obj] = append(connected[obj], v.Kind)
		// Another member's private scope is never listed, whatever rules a custom role grants.
		if v.Scope == store.EnvScopeUser && *v.OwnerID != p.User.ID {
			continue
		}
		if !e.Allowed(sub, m.Org.ID, obj, "integration:list") {
			continue
		}
		j := integrationJSON{ID: v.ID, Kind: v.Kind, Source: v.Source, Scope: v.Scope, Mode: v.Mode, Account: v.Account, Scopes: v.ScopesInfo,
			ProviderExpiresAt: v.ProviderExpiresAt, Status: v.Status, LastError: v.LastError, WasInVM: v.WasInVM, UpdatedAt: v.UpdatedAt,
			CanEdit:   e.Allowed(sub, m.Org.ID, obj, store.ActionIntegrationSet),
			CanDelete: e.Allowed(sub, m.Org.ID, obj, store.ActionIntegrationDelete)}
		if v.Scope == store.EnvScopeTeam {
			j.TeamID, j.TeamName = v.OwnerID, teamNames[*v.OwnerID]
		}
		out = append(out, j)
	}
	scopes := []scopeJSON{}
	add := func(s scopeJSON, obj string) {
		if e.Allowed(sub, m.Org.ID, obj, store.ActionIntegrationSet) {
			s.Connected = append([]integrations.Kind{}, connected[obj]...)
			slices.Sort(s.Connected)
			scopes = append(scopes, s)
		}
	}
	add(scopeJSON{Scope: store.EnvScopeOrg}, authz.ObjIntegrations)
	for _, t := range teams {
		add(scopeJSON{Scope: store.EnvScopeTeam, TeamID: &t.ID, TeamName: t.Name}, authz.TeamIntegrationsObject(t.ID))
	}
	add(scopeJSON{Scope: store.EnvScopeUser}, authz.UserIntegrationsObject(p.User.ID))
	writeJSON(w, http.StatusOK, map[string]any{"integrations": out, "writableScopes": scopes})
}

func (h *api) connectIntegration(w http.ResponseWriter, r *http.Request) {
	m, ok := h.orgFromPath(w, r)
	if !ok {
		return
	}
	var body struct {
		Kind   integrations.Kind
		Source integrations.Source
		Scope  store.EnvScope
		TeamID *uuid.UUID
		Mode   integrations.Mode
		Token  string
		DryRun bool
	}
	if !readJSON(w, r, &body) {
		return
	}
	if body.Mode == "" {
		body.Mode = integrations.ModeProxy
	}
	switch {
	case !pastedSource(body.Kind, body.Source):
		writeError(w, http.StatusBadRequest, "invalid kind or source")
		return
	case !validMode(body.Mode):
		writeError(w, http.StatusBadRequest, "invalid mode")
		return
	}
	owner, ok := h.envTarget(w, r, m.Org.ID, body.Scope, body.TeamID)
	if !ok || !h.authorize(w, r, m.Org.ID, integrationObject(body.Scope, owner), store.ActionIntegrationSet) {
		return
	}
	token, acc, ok := h.verify(w, r, body.Kind, body.Source, body.Token)
	if !ok {
		return
	}
	if body.DryRun {
		writeJSON(w, http.StatusOK, map[string]any{"account": acc.Name})
		return
	}
	p, _ := auth.PrincipalFrom(r.Context())
	v := store.Integration{OrgID: m.Org.ID, Kind: body.Kind, Source: body.Source, Scope: body.Scope, OwnerID: owner, Mode: body.Mode,
		Account: acc.Name, ScopesInfo: acc.Scopes, ProviderExpiresAt: acc.ExpiresAt, CreatedBy: p.User.ID}
	if !h.commit(w, r, func(tx *store.Store) error {
		var err error
		if v, err = tx.CreateIntegration(r.Context(), v, store.IntegrationSecret{Token: token}); err != nil {
			return err
		}
		e := integrationAudit(r, store.ActionIntegrationSet, v)
		e.Details["created"] = true
		return tx.Audit(r.Context(), e)
	}) {
		return
	}
	h.fleet.NotifyOrg(m.Org.ID)
	writeJSON(w, http.StatusCreated, map[string]any{"id": v.ID, "account": v.Account})
}

func (h *api) replaceIntegrationToken(w http.ResponseWriter, r *http.Request) {
	old, ok := h.integrationFromPath(w, r)
	if !ok || !h.authorize(w, r, old.OrgID, integrationObject(old.Scope, old.OwnerID), store.ActionIntegrationSet) {
		return
	}
	var body struct {
		Token  string
		Source *integrations.Source
	}
	if !readJSON(w, r, &body) {
		return
	}
	src := old.Source
	if body.Source != nil {
		src = *body.Source
	}
	if !pastedSource(old.Kind, src) {
		writeError(w, http.StatusBadRequest, "invalid source")
		return
	}
	token, acc, ok := h.verify(w, r, old.Kind, src, body.Token)
	if !ok {
		return
	}
	p, _ := auth.PrincipalFrom(r.Context())
	v := old
	v.Source, v.Account, v.ScopesInfo, v.ProviderExpiresAt, v.UpdatedBy = src, acc.Name, acc.Scopes, acc.ExpiresAt, p.User.ID
	if !h.commitReplace(w, r, old, v, store.IntegrationSecret{Token: token}) {
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"account": v.Account})
}

func (h *api) commitReplace(w http.ResponseWriter, r *http.Request, old, v store.Integration, sec store.IntegrationSecret) bool {
	if !h.commit(w, r, func(tx *store.Store) error {
		if err := tx.ReplaceIntegrationToken(r.Context(), old, v, sec); err != nil {
			return err
		}
		e := integrationAudit(r, store.ActionIntegrationSet, v)
		e.Details["replaced"] = true
		return tx.Audit(r.Context(), e)
	}) {
		return false
	}
	h.fleet.NotifyOrg(old.OrgID)
	return true
}

func (h *api) setIntegrationMode(w http.ResponseWriter, r *http.Request) {
	old, ok := h.integrationFromPath(w, r)
	if !ok || !h.authorize(w, r, old.OrgID, integrationObject(old.Scope, old.OwnerID), store.ActionIntegrationSet) {
		return
	}
	var body struct{ Mode integrations.Mode }
	if !readJSON(w, r, &body) {
		return
	}
	if !validMode(body.Mode) {
		writeError(w, http.StatusBadRequest, "invalid mode")
		return
	}
	p, _ := auth.PrincipalFrom(r.Context())
	v := old
	v.Mode = body.Mode
	if !h.commit(w, r, func(tx *store.Store) error {
		if err := tx.SetIntegrationMode(r.Context(), old, body.Mode, p.User.ID); err != nil {
			return err
		}
		a := integrationAudit(r, store.ActionIntegrationSet, v)
		a.Details["previousMode"] = old.Mode
		return tx.Audit(r.Context(), a)
	}) {
		return
	}
	h.fleet.NotifyOrg(old.OrgID)
	w.WriteHeader(http.StatusNoContent)
}

func (h *api) deleteIntegration(w http.ResponseWriter, r *http.Request) {
	v, ok := h.integrationFromPath(w, r)
	if !ok || !h.authorize(w, r, v.OrgID, integrationObject(v.Scope, v.OwnerID), store.ActionIntegrationDelete) {
		return
	}
	if !h.commit(w, r, func(tx *store.Store) error {
		if err := tx.DeleteIntegration(r.Context(), v); err != nil {
			return err
		}
		return tx.Audit(r.Context(), integrationAudit(r, store.ActionIntegrationDelete, v))
	}) {
		return
	}
	h.fleet.NotifyOrg(v.OrgID)
	w.WriteHeader(http.StatusNoContent)
}

func (h *api) startClaudeSignIn(w http.ResponseWriter, r *http.Request) {
	m, ok := h.orgFromPath(w, r)
	if !ok {
		return
	}
	var body struct {
		Scope     store.EnvScope
		TeamID    *uuid.UUID
		Mode      integrations.Mode
		ReplaceID *uuid.UUID
	}
	if !readJSON(w, r, &body) {
		return
	}
	si := signIn{Mode: body.Mode}
	if si.Mode == "" {
		si.Mode = integrations.ModeProxy
	}
	var replaceKind integrations.Kind
	if body.ReplaceID != nil {
		old, ok := h.loadIntegration(w, r, m.Org.ID, *body.ReplaceID)
		if !ok {
			return
		}
		replaceKind, si.Scope, si.OwnerID, si.ReplaceID = old.Kind, old.Scope, old.OwnerID, &old.ID
	} else {
		if !validMode(si.Mode) {
			writeError(w, http.StatusBadRequest, "invalid mode")
			return
		}
		owner, ok := h.envTarget(w, r, m.Org.ID, body.Scope, body.TeamID)
		if !ok {
			return
		}
		si.Scope, si.OwnerID = body.Scope, owner
	}
	if !h.authorize(w, r, m.Org.ID, integrationObject(si.Scope, si.OwnerID), store.ActionIntegrationSet) {
		return
	}
	if si.ReplaceID != nil && replaceKind != integrations.Claude {
		writeError(w, http.StatusBadRequest, "not a claude integration")
		return
	}
	if si.ReplaceID == nil {
		list, err := h.store.Integrations(r.Context(), m.Org.ID)
		if err != nil {
			serverError(w, "integrations", err)
			return
		}
		// Refuse before the user signs in rather than with a conflict at finish.
		if slices.ContainsFunc(list, func(v store.Integration) bool {
			return v.Kind == integrations.Claude && integrationObject(v.Scope, v.OwnerID) == integrationObject(si.Scope, si.OwnerID)
		}) {
			writeError(w, http.StatusConflict, store.ErrConflict.Error())
			return
		}
	}
	p, _ := auth.PrincipalFrom(r.Context())
	state, challenge, err := h.signIns.Start(p.User.ID, m.Org.ID, si)
	if errors.Is(err, integrations.ErrTooManySignIns) {
		writeError(w, http.StatusTooManyRequests, "too many pending sign-ins")
		return
	} else if err != nil {
		serverError(w, "start sign-in", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"url": h.ig.OAuth.AuthorizeURL(state, challenge), "state": state})
}

func (h *api) finishClaudeSignIn(w http.ResponseWriter, r *http.Request) {
	m, ok := h.orgFromPath(w, r)
	if !ok {
		return
	}
	var body struct{ State, Code string }
	if !readJSON(w, r, &body) {
		return
	}
	// The provider's callback page shows "code#state"; only the code goes to the token endpoint.
	code, codeState, hasState := strings.Cut(strings.TrimSpace(body.Code), "#")
	if body.State == "" || code == "" || len(body.Code) > 4096 {
		writeError(w, http.StatusBadRequest, "invalid code")
		return
	}
	if hasState && subtle.ConstantTimeCompare([]byte(codeState), []byte(body.State)) != 1 {
		writeError(w, http.StatusBadRequest, "code does not belong to this sign-in")
		return
	}
	p, _ := auth.PrincipalFrom(r.Context())
	verifier, si, ok := h.signIns.Take(body.State, p.User.ID, m.Org.ID)
	if !ok {
		writeError(w, http.StatusBadRequest, "sign-in expired, start again")
		return
	}
	// The caller's rights may have changed since the sign-in started.
	if !h.authorize(w, r, m.Org.ID, integrationObject(si.Scope, si.OwnerID), store.ActionIntegrationSet) {
		return
	}
	var old store.Integration
	if si.ReplaceID != nil {
		if old, ok = h.loadIntegration(w, r, m.Org.ID, *si.ReplaceID); !ok {
			return
		}
	}
	t, err := h.ig.OAuth.Exchange(r.Context(), code, body.State, verifier)
	if err != nil {
		providerError(w, err, "sign-in failed: provider rejected the code")
		return
	}
	if t.Refresh == "" {
		writeError(w, http.StatusBadGateway, "provider returned no refresh token")
		return
	}
	access, err := integrations.NormalizeToken(t.Access)
	if err != nil {
		writeError(w, http.StatusBadGateway, "provider returned an invalid token")
		return
	}
	sec := store.IntegrationSecret{Token: access, Refresh: t.Refresh, AccessExpiresAt: &t.ExpiresAt}
	if si.ReplaceID != nil {
		v := old
		v.Source, v.Account, v.ScopesInfo, v.ProviderExpiresAt, v.UpdatedBy = integrations.SourceOAuth, t.Email, "", nil, p.User.ID
		if !h.commitReplace(w, r, old, v, sec) {
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"id": v.ID, "account": v.Account})
		return
	}
	v := store.Integration{OrgID: m.Org.ID, Kind: integrations.Claude, Source: integrations.SourceOAuth, Scope: si.Scope, OwnerID: si.OwnerID,
		Mode: si.Mode, Account: t.Email, CreatedBy: p.User.ID}
	if !h.commit(w, r, func(tx *store.Store) error {
		var err error
		if v, err = tx.CreateIntegration(r.Context(), v, sec); err != nil {
			return err
		}
		e := integrationAudit(r, store.ActionIntegrationSet, v)
		e.Details["created"] = true
		return tx.Audit(r.Context(), e)
	}) {
		return
	}
	h.fleet.NotifyOrg(m.Org.ID)
	writeJSON(w, http.StatusCreated, map[string]any{"id": v.ID, "account": v.Account})
}

func (h *api) vmIntegrations(w http.ResponseWriter, r *http.Request) {
	v, ok := h.vmFromPath(w, r, "vm:read")
	if !ok {
		return
	}
	ctx := r.Context()
	res, err := h.store.ResolveVMIntegrations(ctx, h.z.Enforcer(), v.OrgID, vmOwnerSubject(v))
	if err != nil {
		serverError(w, "vm integrations", err)
		return
	}
	type lineJSON struct {
		Kind     integrations.Kind `json:"kind"`
		Scope    store.EnvScope    `json:"scope,omitempty"`
		TeamName string            `json:"teamName,omitempty"`
		Mode     integrations.Mode `json:"mode,omitempty"`
	}
	p, _ := auth.PrincipalFrom(ctx)
	e, sub := h.z.Enforcer(), authz.UserSubject(p.User.ID)
	out := []lineJSON{}
	for _, ri := range res {
		j := lineJSON{Kind: ri.Kind}
		private := ri.Scope == store.EnvScopeUser && *ri.OwnerID != p.User.ID
		if !private && e.Allowed(sub, v.OrgID, integrationObject(ri.Scope, ri.OwnerID), "integration:list") {
			j.Scope, j.Mode = ri.Scope, ri.Mode
			if ri.Scope == store.EnvScopeTeam {
				// A team deleted since the resolve just loses its name.
				t, err := h.store.Team(ctx, v.OrgID, *ri.OwnerID)
				if err != nil && !errors.Is(err, store.ErrNotFound) {
					serverError(w, "team", err)
					return
				}
				j.TeamName = t.Name
			}
		}
		out = append(out, j)
	}
	writeJSON(w, http.StatusOK, map[string]any{"integrations": out})
}

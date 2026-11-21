package api

import (
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/google/uuid"

	"github.com/asterism-dev/observatory/internal/auth"
	"github.com/asterism-dev/observatory/internal/store"
)

// requirePlatformAdmin 403s and audits a denial for anyone else; it grants no access to tenant data.
func (h *api) requirePlatformAdmin(w http.ResponseWriter, r *http.Request, action, resource string) bool {
	p, _ := auth.PrincipalFrom(r.Context())
	if p.User.PlatformAdmin {
		return true
	}
	if err := h.store.Audit(r.Context(), auditEntry(r, nil, action, resource, store.ResultDenied)); err != nil {
		slog.Error("audit denied admin access", "user", p.User.ID, "action", action, "err", err)
	}
	writeError(w, http.StatusForbidden, "forbidden")
	return false
}

func (h *api) adminListOrgs(w http.ResponseWriter, r *http.Request) {
	if !h.requirePlatformAdmin(w, r, store.ActionAdminRead, "admin/orgs") {
		return
	}
	orgs, err := h.store.Orgs(r.Context())
	if err != nil {
		serverError(w, "orgs", err)
		return
	}
	type orgSummaryJSON struct {
		ID          uuid.UUID `json:"id"`
		Name        string    `json:"name"`
		Personal    bool      `json:"personal"`
		MemberCount int       `json:"memberCount"`
		CreatedAt   time.Time `json:"createdAt"`
	}
	out := make([]orgSummaryJSON, 0, len(orgs))
	for _, o := range orgs {
		out = append(out, orgSummaryJSON{o.Org.ID, o.Org.Name, o.Org.Personal, o.MemberCount, o.Org.CreatedAt})
	}
	writeJSON(w, http.StatusOK, out)
}

func (h *api) adminGetOrg(w http.ResponseWriter, r *http.Request) {
	if !h.requirePlatformAdmin(w, r, store.ActionAdminRead, "admin/orgs") {
		return
	}
	id, ok := pathID(w, r, "org")
	if !ok {
		return
	}
	ctx := r.Context()
	org, err := h.store.Org(ctx, id)
	if err != nil {
		h.adminReadError(w, "admin org", err)
		return
	}
	members, err := h.store.Members(ctx, id)
	if err != nil {
		serverError(w, "admin org members", err)
		return
	}
	ls, err := h.store.Limits(ctx, id)
	if err != nil {
		serverError(w, "admin org limits", err)
		return
	}
	type memberJSON struct {
		ID    uuid.UUID  `json:"id"`
		Email string     `json:"email"`
		Name  string     `json:"name"`
		Role  store.Role `json:"role"`
	}
	ms := make([]memberJSON, 0, len(members))
	for _, m := range members {
		ms = append(ms, memberJSON{m.User.ID, m.User.Email, m.User.Name, m.Role})
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"id": org.ID, "name": org.Name, "personal": org.Personal, "members": ms, "limits": toOrgLimitsJSON(ls),
	})
}

func (h *api) adminReadError(w http.ResponseWriter, what string, err error) {
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "not found")
		return
	}
	serverError(w, what, err)
}

func (h *api) adminSetOrgLimits(w http.ResponseWriter, r *http.Request) {
	if !h.requirePlatformAdmin(w, r, store.ActionLimitSet, "admin/orgs") {
		return
	}
	id, ok := pathID(w, r, "org")
	if !ok {
		return
	}
	l, ok := readLimits(w, r)
	if !ok {
		return
	}
	// The hub org lock no longer fails for an unknown org.
	if _, err := h.store.Org(r.Context(), id); err != nil {
		h.adminReadError(w, "org", err)
		return
	}
	h.commitLimits(w, r, id, store.ScopeOrg, l)
}

func (h *api) adminListUsers(w http.ResponseWriter, r *http.Request) {
	if !h.requirePlatformAdmin(w, r, store.ActionAdminRead, "admin/users") {
		return
	}
	users, err := h.store.Users(r.Context())
	if err != nil {
		serverError(w, "users", err)
		return
	}
	type userJSON struct {
		ID            uuid.UUID `json:"id"`
		Email         string    `json:"email"`
		Name          string    `json:"name"`
		PlatformAdmin bool      `json:"platformAdmin"`
		CreatedAt     time.Time `json:"createdAt"`
	}
	out := make([]userJSON, 0, len(users))
	for _, u := range users {
		out = append(out, userJSON{u.ID, u.Email, u.Name, u.PlatformAdmin, u.CreatedAt})
	}
	writeJSON(w, http.StatusOK, out)
}

func (h *api) adminSetPlatformAdmin(w http.ResponseWriter, r *http.Request) {
	if !h.requirePlatformAdmin(w, r, store.ActionPlatformGrantAdmin, "admin/users") {
		return
	}
	id, ok := pathID(w, r, "user")
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
	action := store.ActionPlatformRevokeAdmin
	if *body.Admin {
		action = store.ActionPlatformGrantAdmin
	}
	if !h.commit(w, r, func(tx *store.Store) error {
		if err := tx.SetPlatformAdmin(r.Context(), id, *body.Admin); err != nil {
			return err
		}
		return tx.Audit(r.Context(), auditEntry(r, nil, action, "user/"+id.String(), store.ResultAllowed))
	}) {
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// adminRegistryGC 404s on hubs without hub-run garbage collection, so the GUI can hide it.
func (h *api) adminRegistryGC(w http.ResponseWriter, r *http.Request) {
	if !h.requirePlatformAdmin(w, r, store.ActionAdminRead, "admin/registry-gc") {
		return
	}
	if h.registryGC == nil {
		writeError(w, http.StatusNotFound, "the hub does not garbage-collect the registry")
		return
	}
	writeJSON(w, http.StatusOK, h.registryGC.Status())
}

func (h *api) adminRunRegistryGC(w http.ResponseWriter, r *http.Request) {
	if !h.requirePlatformAdmin(w, r, store.ActionRegistryGC, "admin/registry-gc") {
		return
	}
	if h.registryGC == nil {
		writeError(w, http.StatusNotFound, "the hub does not garbage-collect the registry")
		return
	}
	if err := h.registryGC.Trigger(); err != nil {
		writeError(w, http.StatusConflict, err.Error())
		return
	}
	if err := h.store.Audit(r.Context(), auditEntry(r, nil, store.ActionRegistryGC, "admin/registry-gc", store.ResultAllowed)); err != nil {
		slog.Error("audit registry gc", "err", err)
	}
	writeJSON(w, http.StatusAccepted, h.registryGC.Status())
}

func (h *api) adminGetNetwork(w http.ResponseWriter, r *http.Request) {
	if !h.requirePlatformAdmin(w, r, store.ActionAdminRead, "admin/network") {
		return
	}
	mtu, err := h.store.GuestMTU(r.Context())
	if err != nil {
		serverError(w, "guest mtu", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]int{"guestMTU": mtu})
}

func (h *api) adminSetNetwork(w http.ResponseWriter, r *http.Request) {
	if !h.requirePlatformAdmin(w, r, store.ActionPlatformGuestMTU, "admin/network") {
		return
	}
	var body struct {
		GuestMTU *int `json:"guestMTU"`
	}
	if !readJSON(w, r, &body) {
		return
	}
	if body.GuestMTU == nil {
		writeError(w, http.StatusBadRequest, "guestMTU is required")
		return
	}
	p, _ := auth.PrincipalFrom(r.Context())
	if err := h.store.SetGuestMTU(r.Context(), *body.GuestMTU, p.User.ID); err != nil {
		if errors.Is(err, store.ErrGuestMTURange) {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		serverError(w, "set guest mtu", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]int{"guestMTU": *body.GuestMTU})
}

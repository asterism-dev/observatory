package api

import (
	"errors"
	"log/slog"
	"net/http"

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

// orgExists 404s an org without members in the policy: iam keeps an owner in every org.
func (h *api) orgExists(w http.ResponseWriter, org uuid.UUID) bool {
	e, ok := h.z.Current()
	if !ok {
		writeError(w, http.StatusServiceUnavailable, "policy unavailable")
		return false
	}
	if len(e.UsersOf(org)) == 0 {
		writeError(w, http.StatusNotFound, "not found")
		return false
	}
	return true
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

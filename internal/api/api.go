package api

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"github.com/google/uuid"

	"github.com/asterism-dev/observatory/internal/auth"
	"github.com/asterism-dev/observatory/internal/authz"
	"github.com/asterism-dev/observatory/internal/disks"
	"github.com/asterism-dev/observatory/internal/fleet"
	"github.com/asterism-dev/observatory/internal/integrations"
	"github.com/asterism-dev/observatory/internal/network"
	"github.com/asterism-dev/observatory/internal/proto"
	"github.com/asterism-dev/observatory/internal/store"
	"github.com/asterism-dev/observatory/internal/version"
)

type api struct {
	store       *store.Store
	z           *authz.Service
	publicURL   *url.URL
	docsURL     string
	fleet       *fleet.Fleet
	network     *network.Hub
	disks       *disks.Hub
	ig          Integrations
	signIns     *integrations.SignIns[signIn]
	newer       func() string
	firecracker string

	diskErrors func(vm uuid.UUID) map[string]string
}

func New(s *store.Store, a *auth.Handler, z *authz.Service, f *fleet.Fleet, n *network.Hub, d *disks.Hub, ig Integrations, docsURL string, newer func() string, firecracker string) http.Handler {
	_, handler := newAPI(s, a, z, f, n, d, ig, docsURL, newer, firecracker)
	return handler
}

func newAPI(s *store.Store, a *auth.Handler, z *authz.Service, f *fleet.Fleet, n *network.Hub, d *disks.Hub, ig Integrations, docsURL string, newer func() string, firecracker string) (*api, http.Handler) {
	h := &api{store: s, z: z, publicURL: a.PublicURL, docsURL: docsURL, fleet: f, network: n, disks: d, diskErrors: f.DiskErrors,
		ig: ig, signIns: integrations.NewSignIns[signIn](signInTTL, time.Now), newer: newer, firecracker: firecracker}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/me", h.me)
	mux.HandleFunc("GET /api/me/sessions", h.sessions)
	mux.HandleFunc("DELETE /api/me/sessions/{id}", h.revokeSession)
	mux.HandleFunc("GET /api/me/ssh-keys", h.listSSHKeys)
	mux.HandleFunc("POST /api/me/ssh-keys", h.addSSHKey)
	mux.HandleFunc("DELETE /api/me/ssh-keys/{id}", h.deleteSSHKey)
	mux.HandleFunc("GET /api/admin/audit", h.adminAudit)
	mux.HandleFunc("GET /api/admin/orgs", h.adminListOrgs)
	mux.HandleFunc("GET /api/admin/orgs/{org}", h.adminGetOrg)
	mux.HandleFunc("PUT /api/admin/orgs/{org}/limits", h.adminSetOrgLimits)
	mux.HandleFunc("GET /api/admin/users", h.adminListUsers)
	mux.HandleFunc("PUT /api/admin/users/{user}/platform-admin", h.adminSetPlatformAdmin)
	mux.HandleFunc("POST /api/orgs", h.createOrg)
	mux.HandleFunc("GET /api/orgs/{org}", h.getOrg)
	mux.HandleFunc("PATCH /api/orgs/{org}", h.renameOrg)
	mux.HandleFunc("GET /api/orgs/{org}/audit", h.orgAudit)
	mux.HandleFunc("GET /api/orgs/{org}/members", h.listMembers)
	mux.HandleFunc("PATCH /api/orgs/{org}/members/{user}", h.setMemberRole)
	mux.HandleFunc("DELETE /api/orgs/{org}/members/{user}", h.removeMember)
	mux.HandleFunc("GET /api/orgs/{org}/invites", h.listInvites)
	mux.HandleFunc("POST /api/orgs/{org}/invites", h.createInvite)
	mux.HandleFunc("DELETE /api/orgs/{org}/invites/{id}", h.revokeInvite)
	mux.HandleFunc("POST /api/invites/accept", h.acceptInvite)
	mux.HandleFunc("GET /api/orgs/{org}/teams", h.listTeams)
	mux.HandleFunc("POST /api/orgs/{org}/teams", h.createTeam)
	mux.HandleFunc("GET /api/orgs/{org}/teams/{team}", h.getTeam)
	mux.HandleFunc("PATCH /api/orgs/{org}/teams/{team}", h.renameTeam)
	mux.HandleFunc("DELETE /api/orgs/{org}/teams/{team}", h.deleteTeam)
	mux.HandleFunc("PUT /api/orgs/{org}/teams/{team}/members/{user}", h.setTeamMember)
	mux.HandleFunc("DELETE /api/orgs/{org}/teams/{team}/members/{user}", h.removeTeamMember)
	mux.HandleFunc("GET /api/orgs/{org}/catalog", h.catalog)
	mux.HandleFunc("GET /api/orgs/{org}/roles", h.listRoles)
	mux.HandleFunc("POST /api/orgs/{org}/roles", h.createRole)
	mux.HandleFunc("PUT /api/orgs/{org}/roles/{role}", h.updateRole)
	mux.HandleFunc("DELETE /api/orgs/{org}/roles/{role}", h.deleteRole)
	mux.HandleFunc("GET /api/orgs/{org}/bindings", h.listBindings)
	mux.HandleFunc("POST /api/orgs/{org}/bindings", h.bind)
	mux.HandleFunc("DELETE /api/orgs/{org}/bindings/{id}", h.unbind)
	mux.HandleFunc("GET /api/orgs/{org}/limits", h.getLimits)
	mux.HandleFunc("PUT /api/orgs/{org}/limits/{scope}", h.setScopeLimits)
	mux.HandleFunc("GET /api/orgs/{org}/env", h.listEnv)
	mux.HandleFunc("POST /api/orgs/{org}/env", h.createEnv)
	mux.HandleFunc("PATCH /api/orgs/{org}/env/{id}", h.updateEnv)
	mux.HandleFunc("POST /api/orgs/{org}/env/{id}/move", h.moveEnv)
	mux.HandleFunc("DELETE /api/orgs/{org}/env/{id}", h.deleteEnv)
	mux.HandleFunc("GET /api/orgs/{org}/integrations", h.listIntegrations)
	mux.HandleFunc("POST /api/orgs/{org}/integrations", h.connectIntegration)
	mux.HandleFunc("PUT /api/orgs/{org}/integrations/{id}/token", h.replaceIntegrationToken)
	mux.HandleFunc("PATCH /api/orgs/{org}/integrations/{id}", h.setIntegrationMode)
	mux.HandleFunc("DELETE /api/orgs/{org}/integrations/{id}", h.deleteIntegration)
	mux.HandleFunc("POST /api/orgs/{org}/integrations/claude/sign-in", h.startClaudeSignIn)
	mux.HandleFunc("POST /api/orgs/{org}/integrations/claude/sign-in/finish", h.finishClaudeSignIn)
	mux.HandleFunc("GET /api/orgs/{org}/nodes", h.listNodes)
	mux.HandleFunc("POST /api/orgs/{org}/nodes", h.createNode)
	mux.HandleFunc("GET /api/orgs/{org}/nodes/{id}", h.getNode)
	mux.HandleFunc("PATCH /api/orgs/{org}/nodes/{id}", h.renameNode)
	mux.HandleFunc("POST /api/orgs/{org}/nodes/{id}/credential", h.regenerateNodeCredential)
	mux.HandleFunc("DELETE /api/orgs/{org}/nodes/{id}", h.deleteNode)
	mux.HandleFunc("POST /api/orgs/{org}/node-updates", h.startNodeUpdate)
	mux.HandleFunc("GET /api/orgs/{org}/node-updates/current", h.currentNodeUpdate)
	mux.HandleFunc("POST /api/orgs/{org}/node-updates/current/cancel", h.cancelNodeUpdate)
	mux.HandleFunc("GET /api/orgs/{org}/devices", h.listDevices)
	mux.HandleFunc("POST /api/orgs/{org}/devices", h.createDevice)
	mux.HandleFunc("DELETE /api/orgs/{org}/devices/{id}", h.deleteDevice)
	mux.HandleFunc("GET /api/orgs/{org}/vms", h.listVMs)
	mux.HandleFunc("POST /api/orgs/{org}/vms", h.createVM)
	mux.HandleFunc("GET /api/orgs/{org}/vms/{id}", h.getVM)
	mux.HandleFunc("POST /api/orgs/{org}/vms/{id}/start", h.powerVM("start", proto.PowerRunning, ""))
	mux.HandleFunc("POST /api/orgs/{org}/vms/{id}/shutdown", h.powerVM("shutdown", proto.PowerStopped, proto.StopGraceful))
	mux.HandleFunc("POST /api/orgs/{org}/vms/{id}/stop", h.powerVM("stop", proto.PowerStopped, proto.StopHard))
	mux.HandleFunc("POST /api/orgs/{org}/vms/{id}/pause", h.powerVM("pause", proto.PowerPaused, ""))
	mux.HandleFunc("POST /api/orgs/{org}/vms/{id}/hibernate", h.powerVM("hibernate", proto.PowerHibernated, ""))
	mux.HandleFunc("POST /api/orgs/{org}/vms/{id}/resume", h.powerVM("resume", proto.PowerRunning, ""))
	mux.HandleFunc("DELETE /api/orgs/{org}/vms/{id}", h.deleteVM)
	mux.HandleFunc("PUT /api/orgs/{org}/vms/{id}/auto-suspend", h.setAutoSuspend)
	mux.HandleFunc("PUT /api/orgs/{org}/vms/{id}/networks", h.setVMNetworks)
	mux.HandleFunc("PUT /api/orgs/{org}/vms/{id}/disks", h.setVMDisks)
	mux.HandleFunc("GET /api/orgs/{org}/vms/{id}/metrics", h.vmMetrics)
	mux.HandleFunc("GET /api/orgs/{org}/vms/{id}/egress", h.vmEgress)
	mux.HandleFunc("GET /api/orgs/{org}/vms/{id}/integrations", h.vmIntegrations)
	mux.HandleFunc("GET /api/orgs/{org}/vms/{id}/snapshots", h.listSnapshots)
	mux.HandleFunc("POST /api/orgs/{org}/vms/{id}/snapshots", h.createSnapshot)
	mux.HandleFunc("DELETE /api/orgs/{org}/vms/{id}/snapshots/{sid}", h.deleteSnapshot)
	mux.HandleFunc("POST /api/orgs/{org}/vms/{id}/snapshots/{sid}/restore", h.restoreSnapshot)
	mux.HandleFunc("POST /api/orgs/{org}/vms/{id}/snapshots/{sid}/fork", h.forkSnapshot)
	mux.HandleFunc("GET /api/orgs/{org}/networks", h.listNetworks)
	mux.HandleFunc("POST /api/orgs/{org}/networks", h.createNetwork)
	mux.HandleFunc("DELETE /api/orgs/{org}/networks/{id}", h.deleteNetwork)
	mux.HandleFunc("GET /api/orgs/{org}/disks", h.listDisks)
	mux.HandleFunc("POST /api/orgs/{org}/disks", h.createDisk)
	mux.HandleFunc("GET /api/orgs/{org}/disks/{id}", h.getDisk)
	mux.HandleFunc("DELETE /api/orgs/{org}/disks/{id}", h.deleteDisk)
	mux.HandleFunc("POST /api/orgs/{org}/disks/{id}/copy", h.copyDisk)
	mux.HandleFunc("POST /api/orgs/{org}/disks/{id}/grow", h.growDisk)
	mux.HandleFunc("POST /api/orgs/{org}/disks/{id}/release", h.releaseDisk)
	return h, a.Require(mux)
}

func (h *api) me(w http.ResponseWriter, r *http.Request) {
	p, _ := auth.PrincipalFrom(r.Context())
	ms, err := h.store.Memberships(r.Context(), p.User.ID)
	if err != nil {
		serverError(w, "memberships", err)
		return
	}
	orgs := make([]orgJSON, 0, len(ms))
	for _, m := range ms {
		orgs = append(orgs, toOrgJSON(m))
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"user":          map[string]any{"id": p.User.ID, "email": p.User.Email, "name": p.User.Name, "platformAdmin": p.User.PlatformAdmin},
		"orgs":          orgs,
		"docsURL":       h.docsURL,
		"version":       version.Version,
		"latestVersion": h.newer(),
	})
}

func (h *api) sessions(w http.ResponseWriter, r *http.Request) {
	p, _ := auth.PrincipalFrom(r.Context())
	ss, err := h.store.Sessions(r.Context(), p.User.ID)
	if err != nil {
		serverError(w, "sessions", err)
		return
	}
	type sessionJSON struct {
		ID         uuid.UUID         `json:"id"`
		Kind       store.SessionKind `json:"kind"`
		UserAgent  string            `json:"userAgent"`
		IP         string            `json:"ip"`
		CreatedAt  time.Time         `json:"createdAt"`
		LastUsedAt time.Time         `json:"lastUsedAt"`
		ExpiresAt  time.Time         `json:"expiresAt"`
		Current    bool              `json:"current"`
	}
	out := make([]sessionJSON, 0, len(ss))
	for _, s := range ss {
		out = append(out, sessionJSON{s.ID, s.Kind, s.UserAgent, s.IP, s.CreatedAt, s.LastUsedAt, s.ExpiresAt, s.ID == p.Session.ID})
	}
	writeJSON(w, http.StatusOK, out)
}

func (h *api) revokeSession(w http.ResponseWriter, r *http.Request) {
	p, _ := auth.PrincipalFrom(r.Context())
	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		writeError(w, http.StatusNotFound, "not found")
		return
	}
	ip, ua := auth.RequestMeta(r)
	err = h.store.InTx(r.Context(), func(tx *store.Store) error {
		if err := tx.RevokeSession(r.Context(), p.User.ID, id); err != nil {
			return err
		}
		return tx.Audit(r.Context(), store.AuditEntry{
			ActorKind: store.ActorUser, ActorID: p.User.ID.String(), Action: store.ActionSessionRevoke,
			Resource: "session/" + id.String(), Result: store.ResultAllowed, IP: ip, UserAgent: ua,
		})
	})
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "not found")
		return
	}
	if err != nil {
		serverError(w, "revoke session", err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *api) adminAudit(w http.ResponseWriter, r *http.Request) {
	p, _ := auth.PrincipalFrom(r.Context())
	if !p.User.PlatformAdmin {
		ip, ua := auth.RequestMeta(r)
		if err := h.store.Audit(r.Context(), store.AuditEntry{
			ActorKind: store.ActorUser, ActorID: p.User.ID.String(), Action: store.ActionAuditRead,
			Resource: "audit", Result: store.ResultDenied, IP: ip, UserAgent: ua,
		}); err != nil {
			slog.Error("audit denied audit read", "user", p.User.ID, "err", err)
		}
		writeError(w, http.StatusForbidden, "forbidden")
		return
	}
	f, err := auditFilter(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	h.writeAuditPage(w, r, f)
}

func (h *api) writeAuditPage(w http.ResponseWriter, r *http.Request, f store.AuditFilter) {
	entries, err := h.store.AuditEntries(r.Context(), f)
	if err != nil {
		serverError(w, "audit entries", err)
		return
	}
	type entryJSON struct {
		ID        int64           `json:"id"`
		At        time.Time       `json:"at"`
		ActorKind store.ActorKind `json:"actorKind"`
		ActorID   string          `json:"actorId"`
		OrgID     *uuid.UUID      `json:"orgId"`
		Action    string          `json:"action"`
		Resource  string          `json:"resource"`
		Result    store.Result    `json:"result"`
		IP        string          `json:"ip"`
		UserAgent string          `json:"userAgent"`
		Details   map[string]any  `json:"details"`
	}
	out := make([]entryJSON, 0, len(entries))
	for _, e := range entries {
		out = append(out, entryJSON{e.ID, e.At, e.ActorKind, e.ActorID, e.OrgID, e.Action, e.Resource, e.Result, e.IP, e.UserAgent, e.Details})
	}
	var next *int64
	if len(entries) == f.Limit {
		next = &entries[len(entries)-1].ID
	}
	writeJSON(w, http.StatusOK, map[string]any{"entries": out, "next": next})
}

func auditFilter(r *http.Request) (store.AuditFilter, error) {
	q := r.URL.Query()
	f := store.AuditFilter{ActorID: q.Get("actor"), Action: q.Get("action"), Limit: 50}
	var err error
	if v := q.Get("org"); v != "" {
		id, err := uuid.Parse(v)
		if err != nil {
			return f, errors.New("invalid org")
		}
		f.OrgID = &id
	}
	for key, dst := range map[string]*time.Time{"from": &f.From, "to": &f.To} {
		if v := q.Get(key); v != "" {
			if *dst, err = time.Parse(time.RFC3339, v); err != nil {
				return f, errors.New("invalid " + key)
			}
		}
	}
	if v := q.Get("before"); v != "" {
		if f.Before, err = strconv.ParseInt(v, 10, 64); err != nil || f.Before < 1 {
			return f, errors.New("invalid before")
		}
	}
	if v := q.Get("limit"); v != "" {
		if f.Limit, err = strconv.Atoi(v); err != nil || f.Limit < 1 {
			return f, errors.New("invalid limit")
		}
		f.Limit = min(f.Limit, 200)
	}
	return f, nil
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

func serverError(w http.ResponseWriter, what string, err error) {
	slog.Error(what, "err", err)
	writeError(w, http.StatusInternalServerError, "internal error")
}

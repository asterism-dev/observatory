package api

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/asterism-dev/observatory/gen/go/observatory/hub/v1/hubv1connect"
	"github.com/asterism-dev/observatory/gen/go/observatory/iam/v1/iamv1connect"
	"github.com/asterism-dev/observatory/internal/auth"
	"github.com/asterism-dev/observatory/internal/authz"
	"github.com/asterism-dev/observatory/internal/builds"
	"github.com/asterism-dev/observatory/internal/disks"
	"github.com/asterism-dev/observatory/internal/fleet"
	"github.com/asterism-dev/observatory/internal/integrations"
	"github.com/asterism-dev/observatory/internal/network"
	"github.com/asterism-dev/observatory/internal/proto"
	"github.com/asterism-dev/observatory/internal/registry"
	"github.com/asterism-dev/observatory/internal/store"
	"github.com/asterism-dev/observatory/internal/telemetry"
	"github.com/asterism-dev/observatory/internal/version"
)

type api struct {
	store       *store.Store
	iam         iamv1connect.IAMInternalServiceClient
	resources   hubv1connect.ResourceServiceClient
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
	manifests   ManifestDeleter
	tags        *TagSync
	builds      *builds.Scheduler
	registryGC  *registry.GC
	telemetry   *telemetry.Client
	now         func() time.Time
	// tokenChecks maps a telemetry token ID to when its creator's access was last confirmed.
	tokenChecks  sync.Map
	grafanaLimit *rateLimiter
	// dashboardLimit is keyed by user ID.
	dashboardLimit *rateLimiter
	// grafanaInflight caps a token's concurrent proxy requests, tails included; grafanaTails caps its tails.
	grafanaInflight, grafanaTails *tokenSlots
	// grafanaTailMax caps a Grafana logs tail; grafanaTailRecheck is how often the tail re-checks its token.
	grafanaTailMax, grafanaTailRecheck time.Duration
	// logsLimit, logsQueries and logsTails are keyed by user ID; logsHub caps explorer queries hub-wide.
	logsLimit              *rateLimiter
	logsQueries, logsTails *tokenSlots
	logsHub                chan struct{}
	logGroups              logGroupCache
	logsRetention          time.Duration

	diskErrors func(vm uuid.UUID) map[string]string
}

// New takes a nil builds when the hub has no registry; builds are refused then.
func New(s *store.Store, iam iamv1connect.IAMInternalServiceClient, resources hubv1connect.ResourceServiceClient, a *auth.Handler, z *authz.Service, f *fleet.Fleet, n *network.Hub, d *disks.Hub, ig Integrations, manifests ManifestDeleter, tags *TagSync, b *builds.Scheduler, gc *registry.GC, docsURL string, newer func() string, firecracker string, tc *telemetry.Client, logsRetention time.Duration) http.Handler {
	_, handler := newAPI(s, iam, resources, a, z, f, n, d, ig, manifests, tags, b, gc, docsURL, newer, firecracker, tc, logsRetention)
	return handler
}

func newAPI(s *store.Store, iam iamv1connect.IAMInternalServiceClient, resources hubv1connect.ResourceServiceClient, a *auth.Handler, z *authz.Service, f *fleet.Fleet, n *network.Hub, d *disks.Hub, ig Integrations, manifests ManifestDeleter, tags *TagSync, b *builds.Scheduler, gc *registry.GC, docsURL string, newer func() string, firecracker string, tc *telemetry.Client, logsRetention time.Duration) (*api, http.Handler) {
	h := &api{store: s, iam: iam, resources: resources, z: z, publicURL: a.PublicURL, docsURL: docsURL, fleet: f, network: n, disks: d, diskErrors: f.DiskErrors,
		ig: ig, signIns: integrations.NewSignIns[signIn](signInTTL, time.Now), newer: newer, firecracker: firecracker, manifests: manifests, tags: tags, builds: b, registryGC: gc, telemetry: tc, now: time.Now, grafanaLimit: newRateLimiter(20, 100), dashboardLimit: newRateLimiter(10, 40),
		grafanaInflight: newTokenSlots(20), grafanaTails: newTokenSlots(5),
		grafanaTailMax: 15 * time.Minute, grafanaTailRecheck: 10 * time.Second,
		logsLimit: newRateLimiter(10, 40), logsQueries: newTokenSlots(3), logsTails: newTokenSlots(5), logsHub: make(chan struct{}, 4), logsRetention: logsRetention}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/me", h.me)
	mux.HandleFunc("GET /api/me/sessions", h.sessions)
	mux.HandleFunc("DELETE /api/me/sessions/{id}", h.revokeSession)
	mux.HandleFunc("GET /api/me/ssh-keys", h.listSSHKeys)
	mux.HandleFunc("POST /api/me/ssh-keys", h.addSSHKey)
	mux.HandleFunc("DELETE /api/me/ssh-keys/{id}", h.deleteSSHKey)
	mux.HandleFunc("POST /api/registry/credentials", h.createRegistryCredential)
	mux.HandleFunc("GET /api/admin/orgs", h.adminListOrgs)
	mux.HandleFunc("GET /api/admin/orgs/{org}", h.adminGetOrg)
	mux.HandleFunc("PUT /api/admin/orgs/{org}/limits", h.adminSetOrgLimits)
	mux.HandleFunc("GET /api/admin/users", h.adminListUsers)
	mux.HandleFunc("PUT /api/admin/users/{user}/platform-admin", h.adminSetPlatformAdmin)
	mux.HandleFunc("GET /api/admin/network", h.adminGetNetwork)
	mux.HandleFunc("PUT /api/admin/network", h.adminSetNetwork)
	mux.HandleFunc("GET /api/admin/registry-gc", h.adminRegistryGC)
	mux.HandleFunc("POST /api/admin/registry-gc", h.adminRunRegistryGC)
	mux.HandleFunc("POST /api/orgs", h.createOrg)
	mux.HandleFunc("GET /api/orgs/{org}", h.getOrg)
	mux.HandleFunc("PATCH /api/orgs/{org}", h.renameOrg)
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
	mux.HandleFunc("GET /api/orgs/{org}/telemetry-tokens", h.listTelemetryTokens)
	mux.HandleFunc("POST /api/orgs/{org}/telemetry-tokens", h.createTelemetryToken)
	mux.HandleFunc("DELETE /api/orgs/{org}/telemetry-tokens/{id}", h.revokeTelemetryToken)
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
	mux.HandleFunc("POST /api/orgs/{org}/integrations/{id}/accounts", h.addIntegrationAccount)
	mux.HandleFunc("PUT /api/orgs/{org}/integrations/{id}/accounts/order", h.orderIntegrationAccounts)
	mux.HandleFunc("PUT /api/orgs/{org}/integrations/{id}/accounts/{accountId}/token", h.replaceAccountToken)
	mux.HandleFunc("POST /api/orgs/{org}/integrations/{id}/accounts/{accountId}/activate", h.activateIntegrationAccount)
	mux.HandleFunc("DELETE /api/orgs/{org}/integrations/{id}/accounts/{accountId}", h.deleteIntegrationAccount)
	mux.HandleFunc("POST /api/orgs/{org}/integrations/claude/sign-in", h.startClaudeSignIn)
	mux.HandleFunc("POST /api/orgs/{org}/integrations/claude/sign-in/finish", h.finishClaudeSignIn)
	mux.HandleFunc("GET /api/orgs/{org}/images", h.listImages)
	mux.HandleFunc("POST /api/orgs/{org}/images", h.createImage)
	mux.HandleFunc("GET /api/orgs/{org}/images/{id}", h.getImage)
	mux.HandleFunc("PATCH /api/orgs/{org}/images/{id}", h.updateImage)
	mux.HandleFunc("DELETE /api/orgs/{org}/images/{id}", h.deleteImage)
	mux.HandleFunc("GET /api/orgs/{org}/images/{id}/recipe", h.getRecipe)
	mux.HandleFunc("GET /api/orgs/{org}/images/{id}/recipe/files/{path...}", h.getRecipeFile)
	mux.HandleFunc("PUT /api/orgs/{org}/images/{id}/recipe", h.putRecipe)
	mux.HandleFunc("POST /api/orgs/{org}/images/{id}/recipe/edits", h.editRecipe)
	mux.HandleFunc("GET /api/orgs/{org}/images/{id}/versions", h.listImageVersions)
	mux.HandleFunc("DELETE /api/orgs/{org}/images/{id}/versions/{digest}", h.deleteImageVersion)
	mux.HandleFunc("PUT /api/orgs/{org}/images/{id}/tags/{tag}", h.setImageTag)
	mux.HandleFunc("DELETE /api/orgs/{org}/images/{id}/tags/{tag}", h.deleteImageTag)
	mux.HandleFunc("POST /api/orgs/{org}/images/{id}/builds", h.startBuild)
	mux.HandleFunc("GET /api/orgs/{org}/images/{id}/builds", h.listBuilds)
	mux.HandleFunc("GET /api/orgs/{org}/images/{id}/builds/{bid}", h.getBuild)
	mux.HandleFunc("GET /api/orgs/{org}/images/{id}/builds/{bid}/log", h.buildLog)
	mux.HandleFunc("POST /api/orgs/{org}/images/{id}/builds/{bid}/cancel", h.cancelBuild)
	mux.HandleFunc("GET /api/orgs/{org}/image-defaults/{level}", h.getImageDefault)
	mux.HandleFunc("PUT /api/orgs/{org}/image-defaults/{level}", h.setImageDefault)
	mux.HandleFunc("DELETE /api/orgs/{org}/image-defaults/{level}", h.clearImageDefault)
	mux.HandleFunc("GET /api/orgs/{org}/image-defaults/team/{team}", h.getImageDefault)
	mux.HandleFunc("PUT /api/orgs/{org}/image-defaults/team/{team}", h.setImageDefault)
	mux.HandleFunc("DELETE /api/orgs/{org}/image-defaults/team/{team}", h.clearImageDefault)
	mux.HandleFunc("GET /api/orgs/{org}/dashboards", h.listDashboards)
	mux.HandleFunc("POST /api/orgs/{org}/dashboards", h.createDashboard)
	mux.HandleFunc("GET /api/orgs/{org}/dashboards/{id}", h.getDashboard)
	mux.HandleFunc("PUT /api/orgs/{org}/dashboards/{id}", h.updateDashboard)
	mux.HandleFunc("DELETE /api/orgs/{org}/dashboards/{id}", h.deleteDashboard)
	mux.HandleFunc("POST /api/orgs/{org}/dashboards/query", h.dashboardQuery)
	mux.HandleFunc("GET /api/orgs/{org}/dashboards/metric-names", h.dashboardMetricNames)
	mux.HandleFunc("GET /api/orgs/{org}/logs/groups", h.listLogGroups)
	mux.HandleFunc("GET /api/orgs/{org}/logs/groups/{kind}/{id}/streams", h.listLogStreams)
	mux.HandleFunc("GET /api/orgs/{org}/logs/groups/{kind}/{id}/entries", h.listLogEntries)
	mux.HandleFunc("GET /api/orgs/{org}/logs/groups/{kind}/{id}/entries/tail", h.tailLogEntries)
	mux.HandleFunc("POST /api/orgs/{org}/logs/query", h.queryLogs)
	mux.HandleFunc("GET /api/orgs/{org}/nodes", h.listNodes)
	mux.HandleFunc("POST /api/orgs/{org}/nodes", h.createNode)
	mux.HandleFunc("GET /api/orgs/{org}/nodes/{id}", h.getNode)
	mux.HandleFunc("PATCH /api/orgs/{org}/nodes/{id}", h.renameNode)
	mux.HandleFunc("POST /api/orgs/{org}/nodes/{id}/credential", h.regenerateNodeCredential)
	mux.HandleFunc("DELETE /api/orgs/{org}/nodes/{id}", h.deleteNode)
	mux.HandleFunc("GET /api/orgs/{org}/nodes/{id}/usage", h.usageNode)
	mux.HandleFunc("GET /api/orgs/{org}/nodes/{id}/logs", h.nodeLogs)
	mux.HandleFunc("GET /api/orgs/{org}/nodes/{id}/logs/tail", h.tailNodeLogs)
	mux.HandleFunc("POST /api/orgs/{org}/node-updates", h.startNodeUpdate)
	mux.HandleFunc("GET /api/orgs/{org}/node-updates/current", h.currentNodeUpdate)
	mux.HandleFunc("POST /api/orgs/{org}/node-updates/current/cancel", h.cancelNodeUpdate)
	mux.HandleFunc("GET /api/orgs/{org}/devices", h.listDevices)
	mux.HandleFunc("POST /api/orgs/{org}/devices", h.createDevice)
	mux.HandleFunc("DELETE /api/orgs/{org}/devices/{id}", h.deleteDevice)
	mux.HandleFunc("GET /api/orgs/{org}/vms", h.listVMs)
	mux.HandleFunc("POST /api/orgs/{org}/vms", h.createVM)
	mux.HandleFunc("GET /api/orgs/{org}/vms/history", h.vmHistory)
	mux.HandleFunc("GET /api/orgs/{org}/vms/{id}", h.getVM)
	mux.HandleFunc("GET /api/orgs/{org}/builder-vms", h.listBuilderVMs)
	mux.HandleFunc("POST /api/orgs/{org}/vms/{id}/start", h.powerVM("start", proto.PowerRunning, ""))
	mux.HandleFunc("POST /api/orgs/{org}/vms/{id}/shutdown", h.powerVM("shutdown", proto.PowerStopped, proto.StopGraceful))
	mux.HandleFunc("POST /api/orgs/{org}/vms/{id}/stop", h.powerVM("stop", proto.PowerStopped, proto.StopHard))
	mux.HandleFunc("POST /api/orgs/{org}/vms/{id}/pause", h.powerVM("pause", proto.PowerPaused, ""))
	mux.HandleFunc("POST /api/orgs/{org}/vms/{id}/hibernate", h.powerVM("hibernate", proto.PowerHibernated, ""))
	mux.HandleFunc("POST /api/orgs/{org}/vms/{id}/resume", h.powerVM("resume", proto.PowerRunning, ""))
	mux.HandleFunc("DELETE /api/orgs/{org}/vms/{id}", h.deleteVM)
	mux.HandleFunc("PUT /api/orgs/{org}/vms/{id}/auto-suspend", h.setAutoSuspend)
	mux.HandleFunc("PUT /api/orgs/{org}/vms/{id}/delete-after", h.setDeleteAfter)
	mux.HandleFunc("PUT /api/orgs/{org}/vms/{id}/networks", h.setVMNetworks)
	mux.HandleFunc("PUT /api/orgs/{org}/vms/{id}/disks", h.setVMDisks)
	mux.HandleFunc("GET /api/orgs/{org}/vms/{id}/usage", h.usageVM)
	mux.HandleFunc("GET /api/orgs/{org}/vms/{id}/logs", h.vmLogs)
	mux.HandleFunc("GET /api/orgs/{org}/vms/{id}/logs/tail", h.tailVMLogs)
	mux.HandleFunc("GET /api/orgs/{org}/vms/{id}/processes", h.vmProcesses)
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
	users := a.Require(mux)
	return h, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/grafana/logs/") {
			h.logsProxy(w, r)
			return
		}
		if strings.HasPrefix(r.URL.Path, "/grafana/") {
			h.promProxy(w, r)
			return
		}
		users.ServeHTTP(w, r)
	})
}

func (h *api) me(w http.ResponseWriter, r *http.Request) {
	p, _ := auth.PrincipalFrom(r.Context())
	// The JWT carries no e-mail; the hub still owns the users table until iam moves out.
	u, err := h.store.User(r.Context(), p.User.ID)
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	if err != nil {
		serverError(w, "user", err)
		return
	}
	ms, err := h.store.Memberships(r.Context(), p.User.ID)
	if err != nil {
		serverError(w, "memberships", err)
		return
	}
	orgs := make([]orgJSON, 0, len(ms))
	for _, m := range ms {
		orgs = append(orgs, toOrgJSON(m.Org, m.Role))
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"user":          map[string]any{"id": u.ID, "email": u.Email, "name": u.Name, "platformAdmin": p.User.PlatformAdmin},
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
		// ponytail: iam writes a hub table so the registry refuses without the bus; 5b moves this into iam's revoke path.
		if err := tx.DeleteSessionCredentials(r.Context(), id); err != nil {
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

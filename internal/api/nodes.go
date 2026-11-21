package api

import (
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/asterism-dev/observatory/internal/auth"
	"github.com/asterism-dev/observatory/internal/authz"
	"github.com/asterism-dev/observatory/internal/fleet"
	"github.com/asterism-dev/observatory/internal/netaddr"
	"github.com/asterism-dev/observatory/internal/network"
	"github.com/asterism-dev/observatory/internal/proto"
	"github.com/asterism-dev/observatory/internal/store"
	"github.com/asterism-dev/observatory/internal/version"
)

type nodeJSON struct {
	ID               uuid.UUID          `json:"id"`
	Name             string             `json:"name"`
	State            string             `json:"state"`
	Version          string             `json:"version"`
	Outdated         bool               `json:"outdated"`
	CPUs             int                `json:"cpus"`
	MemoryMB         int                `json:"memoryMB"`
	Machine          any                `json:"machine"`
	LastSeen         *time.Time         `json:"lastSeen"`
	VMCount          int                `json:"vmCount"`
	UsedCPUs         int                `json:"usedCPUs"`
	UsedMemoryMB     int                `json:"usedMemoryMB"`
	CanUpdate        bool               `json:"canUpdate"`
	CanDelete        bool               `json:"canDelete"`
	CanTelemetry     bool               `json:"canTelemetry"`
	Underlay         *underlayJSON      `json:"underlay"`
	Quarantined      bool               `json:"quarantined"`
	Image            *proto.ImageStatus `json:"image"`
	Hypervisor       *proto.Hypervisor  `json:"hypervisor"`
	Updatable        bool               `json:"updatable"`
	Updating         bool               `json:"updating"`
	UpdatePausesOnly bool               `json:"updatePausesOnly"`
	Paths            []pathJSON         `json:"paths"`
}

type pathJSON struct {
	Node                uuid.UUID  `json:"node"`
	NodeName            string     `json:"nodeName"`
	Mode                string     `json:"mode"`
	Endpoint            string     `json:"endpoint,omitempty"`
	MTU                 int        `json:"mtu"`
	Handshake           *time.Time `json:"handshake"`
	Warning             bool       `json:"warning"`
	RecommendedGuestMTU int        `json:"recommendedGuestMTU,omitempty"`
}

const tunnelOverhead = netaddr.GeneveOverhead + netaddr.WireGuardOverhead

func pathWarning(guestMTU, pathMTU int) (warn bool, recommended int) {
	if pathMTU <= 0 || guestMTU+tunnelOverhead <= pathMTU {
		return false, 0
	}
	return true, (pathMTU - tunnelOverhead) / 10 * 10
}

type underlayJSON struct {
	IP            string     `json:"ip"`
	Registered    bool       `json:"registered"`
	LastHandshake *time.Time `json:"lastHandshake"`
}

func (h *api) installCommand(token string) string {
	base := strings.TrimRight(h.publicURL.String(), "/")
	return "curl -fsSL " + base + "/node/install.sh | sudo sh -s -- --hub " + base + " --token " + token
}

func (h *api) nodeJSON(r *http.Request, n store.Node) (nodeJSON, error) {
	vms, err := h.store.VMsOnNode(r.Context(), n.ID)
	if err != nil {
		return nodeJSON{}, err
	}
	j := nodeJSON{ID: n.ID, Name: n.Name, State: "pending", Version: n.Version, Outdated: version.Older(n.Version, version.Version), CPUs: n.CPUs, MemoryMB: n.MemoryMB,
		LastSeen: n.LastSeen, VMCount: len(vms), Quarantined: n.Quarantined, Image: n.Image, Hypervisor: n.Hypervisor,
		Updatable: h.fleet.Updatable(n), Updating: h.fleet.Updating(n.ID), UpdatePausesOnly: n.Hypervisor != nil && n.Hypervisor.Version == h.firecracker}
	if len(n.Machine) > 0 {
		j.Machine = n.Machine
	}
	if n.Enrolled {
		switch {
		case h.fleet.Online(n.ID):
			j.State = "online"
		case h.fleet.UnderlayDown(n.ID):
			j.State = "underlay_down"
		default:
			j.State = "offline"
		}
	}
	paths := h.fleet.Paths(n.ID)
	j.Paths = []pathJSON{}
	if len(paths) > 0 {
		// ponytail: one extra node query per node with paths; cache names if lists grow large.
		peers, err := h.store.Nodes(r.Context(), n.OrgID)
		if err != nil {
			return nodeJSON{}, err
		}
		names := map[string]string{}
		for _, p := range peers {
			names[p.ID.String()] = p.Name
		}
		guestMTU, err := h.store.GuestMTU(r.Context())
		if err != nil {
			return nodeJSON{}, err
		}
		for _, p := range paths {
			id, err := uuid.Parse(p.NodeID)
			name, same := names[p.NodeID]
			if err != nil || !same {
				continue
			}
			warn, rec := pathWarning(guestMTU, p.MTU)
			j.Paths = append(j.Paths, pathJSON{Node: id, NodeName: name, Mode: p.Mode, Endpoint: p.Endpoint, MTU: p.MTU, Handshake: p.Handshake, Warning: warn, RecommendedGuestMTU: rec})
		}
	}
	if cs, ok := h.network.ChassisStatus(n.ID); ok {
		j.Underlay = &underlayJSON{IP: cs.IP, Registered: cs.Registered, LastHandshake: cs.LastHandshake}
	}
	for _, v := range vms {
		j.UsedCPUs += v.CPUs
		j.UsedMemoryMB += v.MemoryMB
	}
	p, _ := auth.PrincipalFrom(r.Context())
	e, sub := h.z.Enforcer(), authz.UserSubject(p.User.ID)
	j.CanUpdate = e.Allowed(sub, n.OrgID, authz.NodeObject(n.ID), "compute.nodes.update")
	j.CanDelete = e.Allowed(sub, n.OrgID, authz.NodeObject(n.ID), "compute.nodes.delete")
	j.CanTelemetry = h.telemetry != nil && e.Allowed(sub, n.OrgID, authz.NodeObject(n.ID), "compute.nodes.getMetrics")
	return j, nil
}

func nodeAudit(r *http.Request, action string, n store.Node) store.AuditEntry {
	e := auditEntry(r, &n.OrgID, action, authz.NodeObject(n.ID), store.ResultAllowed)
	e.Details = map[string]any{"name": n.Name}
	return e
}

// nodeFromPath loads {id} within the caller's org and authorizes act on it.
func (h *api) nodeFromPath(w http.ResponseWriter, r *http.Request, act string) (store.Node, bool) {
	m, ok := h.orgFromPath(w, r)
	if !ok {
		return store.Node{}, false
	}
	id, ok := pathID(w, r, "id")
	if !ok {
		return store.Node{}, false
	}
	n, err := h.store.Node(r.Context(), m.OrgID, id)
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "not found")
		return store.Node{}, false
	}
	if err != nil {
		serverError(w, "node", err)
		return store.Node{}, false
	}
	return n, h.authorize(w, r, n.OrgID, authz.NodeObject(n.ID), act)
}

func (h *api) listNodes(w http.ResponseWriter, r *http.Request) {
	m, ok := h.orgFromPath(w, r)
	if !ok || !h.authorize(w, r, m.OrgID, authz.ObjNodes, "compute.nodes.get") {
		return
	}
	nodes, err := h.store.Nodes(r.Context(), m.OrgID)
	if err != nil {
		serverError(w, "nodes", err)
		return
	}
	p, _ := auth.PrincipalFrom(r.Context())
	e, sub := h.z.Enforcer(), authz.UserSubject(p.User.ID)
	out := []nodeJSON{}
	for _, n := range nodes {
		if !e.Allowed(sub, m.OrgID, authz.NodeObject(n.ID), "compute.nodes.get") {
			continue
		}
		// ponytail: one VM query per node; aggregate in the store if orgs run many nodes.
		j, err := h.nodeJSON(r, n)
		if err != nil {
			serverError(w, "node vms", err)
			return
		}
		out = append(out, j)
	}
	writeJSON(w, http.StatusOK, out)
}

func (h *api) createNode(w http.ResponseWriter, r *http.Request) {
	m, ok := h.orgFromPath(w, r)
	if !ok || !h.authorize(w, r, m.OrgID, authz.ObjNodes, "compute.nodes.create") {
		return
	}
	name, ok := readName(w, r)
	if !ok {
		return
	}
	p, _ := auth.PrincipalFrom(r.Context())
	var n store.Node
	var token string
	if !h.commit(w, r, func(tx *store.Store) error {
		var err error
		if n, token, err = tx.CreateNode(r.Context(), store.Node{OrgID: m.OrgID, Name: name, CreatedBy: p.User.ID}); err != nil {
			return err
		}
		return tx.Audit(r.Context(), nodeAudit(r, store.ActionNodeCreate, n))
	}) {
		return
	}
	j, err := h.nodeJSON(r, n)
	if err != nil {
		serverError(w, "node vms", err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"node": j, "installCommand": h.installCommand(token)})
}

func (h *api) getNode(w http.ResponseWriter, r *http.Request) {
	n, ok := h.nodeFromPath(w, r, "compute.nodes.get")
	if !ok {
		return
	}
	j, err := h.nodeJSON(r, n)
	if err != nil {
		serverError(w, "node vms", err)
		return
	}
	writeJSON(w, http.StatusOK, j)
}

func (h *api) renameNode(w http.ResponseWriter, r *http.Request) {
	n, ok := h.nodeFromPath(w, r, "compute.nodes.update")
	if !ok {
		return
	}
	name, ok := readName(w, r)
	if !ok {
		return
	}
	old := n.Name
	n.Name = name
	if !h.commit(w, r, func(tx *store.Store) error {
		if err := tx.RenameNode(r.Context(), n.OrgID, n.ID, name); err != nil {
			return err
		}
		e := nodeAudit(r, store.ActionNodeUpdate, n)
		e.Details["from"] = old
		return tx.Audit(r.Context(), e)
	}) {
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *api) regenerateNodeCredential(w http.ResponseWriter, r *http.Request) {
	n, ok := h.nodeFromPath(w, r, "compute.nodes.update")
	if !ok {
		return
	}
	var token string
	if !h.commit(w, r, func(tx *store.Store) error {
		var err error
		if token, err = tx.RegenerateNodeCredential(r.Context(), n.OrgID, n.ID); err != nil {
			return err
		}
		if n.Enrolled {
			return tx.Audit(r.Context(), nodeAudit(r, store.ActionNodeRevoke, n))
		}
		e := nodeAudit(r, store.ActionNodeUpdate, n)
		e.Details["regenerated"] = true
		return tx.Audit(r.Context(), e)
	}) {
		return
	}
	h.fleet.Disconnect(n.ID)
	writeJSON(w, http.StatusOK, map[string]string{"installCommand": h.installCommand(token)})
}

func (h *api) deleteNode(w http.ResponseWriter, r *http.Request) {
	n, ok := h.nodeFromPath(w, r, "compute.nodes.delete")
	if !ok {
		return
	}
	if !h.commit(w, r, func(tx *store.Store) error {
		if err := tx.DeleteNode(r.Context(), n.OrgID, n.ID); err != nil {
			return err
		}
		return tx.Audit(r.Context(), nodeAudit(r, store.ActionNodeDelete, n))
	}) {
		return
	}
	h.fleet.Disconnect(n.ID)
	h.fleet.NotifyOrg(n.OrgID) // drops the node from the org's peer sets
	w.WriteHeader(http.StatusNoContent)
}

// NodeEndpoints registers the routes nodes call themselves; they authenticate with enroll tokens or credentials, not sessions.
func NodeEndpoints(mux *http.ServeMux, s *store.Store, f *fleet.Fleet, nh *network.Hub) {
	mux.HandleFunc("GET /api/nodes/connect", f.ServeConnect)
	mux.HandleFunc("GET /api/nodes/health", f.ServeHealth)
	mux.HandleFunc("POST /api/nodes/emergency", f.ServeEmergency)
	mux.HandleFunc("POST /api/nodes/enroll", func(w http.ResponseWriter, r *http.Request) {
		var body struct{ Token string }
		if !readJSON(w, r, &body) {
			return
		}
		var n store.Node
		var credential string
		err := s.InTx(r.Context(), func(tx *store.Store) error {
			var err error
			if n, credential, err = tx.EnrollNode(r.Context(), body.Token); err != nil {
				return err
			}
			ip, ua := auth.RequestMeta(r)
			return tx.Audit(r.Context(), store.AuditEntry{ActorKind: store.ActorNode, ActorID: n.ID.String(), OrgID: &n.OrgID,
				Action: store.ActionNodeEnroll, Resource: authz.NodeObject(n.ID), Result: store.ResultAllowed, IP: ip, UserAgent: ua,
				Details: map[string]any{"name": n.Name}})
		})
		if errors.Is(err, store.ErrEnrollInvalid) {
			writeError(w, http.StatusBadRequest, "invalid enrollment token")
			return
		}
		if err != nil {
			serverError(w, "enroll node", err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"nodeId": n.ID.String(), "credential": credential})
	})
	mux.HandleFunc("POST /api/nodes/underlay", func(w http.ResponseWriter, r *http.Request) {
		cred, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		node, err := s.NodeByCredential(r.Context(), cred)
		if !ok || errors.Is(err, store.ErrNotFound) {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		if err != nil {
			serverError(w, "node auth", err)
			return
		}
		if nh == nil {
			writeError(w, http.StatusServiceUnavailable, "network is not configured")
			return
		}
		var body proto.UnderlayRequest
		if !readJSON(w, r, &body) {
			return
		}
		u, err := nh.Underlay(r.Context(), node, body.ChassisKey, body.ChassisCSR)
		if errors.Is(err, network.ErrUnderlayRequest) {
			slog.Warn("node underlay request", "node", node.ID, "err", err)
			writeError(w, http.StatusBadRequest, "invalid chassis key or certificate request")
			return
		}
		if err != nil {
			serverError(w, "node underlay", err)
			return
		}
		writeJSON(w, http.StatusOK, u)
	})
}

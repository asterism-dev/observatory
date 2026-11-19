package api

import (
	"errors"
	"log/slog"
	"net/http"
	"slices"
	"time"

	"github.com/google/uuid"

	"github.com/asterism-dev/observatory/internal/auth"
	"github.com/asterism-dev/observatory/internal/authz"
	"github.com/asterism-dev/observatory/internal/fleet"
	"github.com/asterism-dev/observatory/internal/store"
)

type updateRunJSON struct {
	ID        uuid.UUID        `json:"id"`
	State     string           `json:"state"`
	StartedBy uuid.UUID        `json:"startedBy"`
	StartedAt time.Time        `json:"startedAt"`
	EndedAt   *time.Time       `json:"endedAt"`
	Nodes     []updateNodeJSON `json:"nodes"`
}

type updateNodeJSON struct {
	ID     uuid.UUID `json:"id"`
	Name   string    `json:"name"`
	Status string    `json:"status"`
	Reason string    `json:"reason"`
}

func (h *api) updateRunJSON(r *http.Request, run fleet.UpdateRun, names map[uuid.UUID]string) (updateRunJSON, error) {
	if names == nil {
		nodes, err := h.store.Nodes(r.Context(), run.Org)
		if err != nil {
			return updateRunJSON{}, err
		}
		names = map[uuid.UUID]string{}
		for _, n := range nodes {
			names[n.ID] = n.Name
		}
	}
	j := updateRunJSON{ID: run.ID, State: run.State, StartedBy: run.StartedBy, StartedAt: run.StartedAt, EndedAt: run.EndedAt, Nodes: []updateNodeJSON{}}
	for _, n := range run.Nodes {
		j.Nodes = append(j.Nodes, updateNodeJSON{ID: n.ID, Name: names[n.ID], Status: n.Status, Reason: n.Reason})
	}
	return j, nil
}

func (h *api) auditUpdate(r *http.Request, org uuid.UUID, details map[string]any) {
	e := auditEntry(r, &org, store.ActionNodeUpdate, authz.ObjNodes, store.ResultAllowed)
	e.Details = details
	if err := h.store.Audit(r.Context(), e); err != nil {
		slog.Error("audit node update", "org", org, "err", err)
	}
}

func (h *api) startNodeUpdate(w http.ResponseWriter, r *http.Request) {
	m, ok := h.orgFromPath(w, r)
	if !ok {
		return
	}
	var body struct{ Nodes []uuid.UUID }
	if !readJSON(w, r, &body) {
		return
	}
	if len(body.Nodes) == 0 {
		writeError(w, http.StatusBadRequest, "no nodes")
		return
	}
	var nodes []store.Node
	names := map[uuid.UUID]string{}
	var list []string
	for _, id := range body.Nodes {
		if _, dup := names[id]; dup {
			writeError(w, http.StatusBadRequest, "duplicate node")
			return
		}
		n, err := h.store.Node(r.Context(), m.Org.ID, id)
		if errors.Is(err, store.ErrNotFound) {
			writeError(w, http.StatusNotFound, "not found")
			return
		}
		if err != nil {
			serverError(w, "node", err)
			return
		}
		if !h.authorize(w, r, n.OrgID, authz.NodeObject(n.ID), store.ActionNodeUpdate) {
			return
		}
		nodes = append(nodes, n)
		names[id] = n.Name
		list = append(list, n.Name)
	}
	p, _ := auth.PrincipalFrom(r.Context())
	run, err := h.fleet.StartUpdate(r.Context(), m.Org.ID, p.User.ID, nodes)
	if errors.Is(err, fleet.ErrUpdateRunning) {
		writeError(w, http.StatusConflict, "an update is already running")
		return
	}
	if errors.Is(err, fleet.ErrNotUpdatable) {
		writeError(w, http.StatusConflict, notUpdatable(h.fleet, nodes))
		return
	}
	if err != nil {
		serverError(w, "start node update", err)
		return
	}
	h.auditUpdate(r, m.Org.ID, map[string]any{"update": "started", "nodes": list})
	j, err := h.updateRunJSON(r, run, names)
	if err != nil {
		serverError(w, "node update", err)
		return
	}
	writeJSON(w, http.StatusCreated, j)
}

func notUpdatable(f *fleet.Fleet, nodes []store.Node) string {
	i := slices.IndexFunc(nodes, func(n store.Node) bool { return !f.Updatable(n) })
	if i < 0 {
		return "these nodes cannot be updated"
	}
	return "node " + nodes[i].Name + " cannot be updated"
}

func (h *api) currentNodeUpdate(w http.ResponseWriter, r *http.Request) {
	m, ok := h.orgFromPath(w, r)
	if !ok {
		return
	}
	run, found := h.fleet.CurrentUpdate(m.Org.ID)
	if !found {
		writeError(w, http.StatusNotFound, "not found")
		return
	}
	j, err := h.updateRunJSON(r, run, nil)
	if err != nil {
		serverError(w, "node update", err)
		return
	}
	writeJSON(w, http.StatusOK, j)
}

func (h *api) cancelNodeUpdate(w http.ResponseWriter, r *http.Request) {
	m, ok := h.orgFromPath(w, r)
	if !ok {
		return
	}
	run, found := h.fleet.CurrentUpdate(m.Org.ID)
	if !found || run.State != "running" {
		writeError(w, http.StatusNotFound, "not found")
		return
	}
	for _, n := range run.Nodes {
		if !h.authorize(w, r, m.Org.ID, authz.NodeObject(n.ID), store.ActionNodeUpdate) {
			return
		}
	}
	if !h.fleet.CancelUpdate(m.Org.ID) {
		writeError(w, http.StatusNotFound, "not found")
		return
	}
	h.auditUpdate(r, m.Org.ID, map[string]any{"update": "cancelled"})
	w.WriteHeader(http.StatusNoContent)
}

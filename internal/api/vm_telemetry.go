package api

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/google/uuid"

	"github.com/asterism-dev/observatory/internal/auth"
	"github.com/asterism-dev/observatory/internal/authz"
	"github.com/asterism-dev/observatory/internal/fleet"
	"github.com/asterism-dev/observatory/internal/proto"
	"github.com/asterism-dev/observatory/internal/store"
	"github.com/asterism-dev/observatory/internal/telemetry"
)

type vmUsageJSON struct {
	At          time.Time `json:"at"`
	CPUPercent  float64   `json:"cpuPercent"`
	MemUsedMB   int64     `json:"memUsedMB"`
	MemTotalMB  int64     `json:"memTotalMB"`
	DiskUsedMB  int64     `json:"diskUsedMB"`
	DiskTotalMB int64     `json:"diskTotalMB"`
}

func newVMUsageJSON(n telemetry.VMNow) *vmUsageJSON {
	return &vmUsageJSON{At: n.At, CPUPercent: n.CPUPercent, MemUsedMB: n.MemUsedBytes >> 20, MemTotalMB: n.MemTotalBytes >> 20,
		DiskUsedMB: n.DiskUsedBytes >> 20, DiskTotalMB: n.DiskTotalBytes >> 20}
}

const (
	vmLatestTimeout     = 2 * time.Second
	vmListLatestTimeout = 5 * time.Second
)

var vmProcessesTimeout = 3 * time.Second

// vmLatest loads the newest usage of those vms the caller may see metrics of; a store error only drops the usage.
func (h *api) vmLatest(r *http.Request, org uuid.UUID, vms []store.VM, timeout time.Duration) map[uuid.UUID]telemetry.VMNow {
	if h.telemetry == nil {
		return nil
	}
	p, _ := auth.PrincipalFrom(r.Context())
	e, sub := h.z.Enforcer(), authz.UserSubject(p.User.ID)
	ids := []uuid.UUID{}
	for _, v := range vms {
		if e.Allowed(sub, org, authz.VMObject(v.ID), store.ActionVMMetrics) {
			ids = append(ids, v.ID)
		}
	}
	if len(ids) == 0 {
		return nil
	}
	ctx, cancel := context.WithTimeout(r.Context(), timeout)
	defer cancel()
	latest, err := h.telemetry.VMLatest(ctx, org, ids)
	if err != nil {
		slog.Warn("vm latest usage", "org", org, "err", err)
		return nil
	}
	return latest
}

func (h *api) usageVM(w http.ResponseWriter, r *http.Request) {
	if h.telemetry == nil {
		writeError(w, http.StatusNotFound, "not found")
		return
	}
	v, ok := h.vmFromPath(w, r, store.ActionVMMetrics)
	if !ok {
		return
	}
	from, to, ok := timeRange(w, r, 6*time.Hour)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), usageTimeout)
	defer cancel()
	u, err := h.telemetry.VMUsage(ctx, v.OrgID, v.ID, from, to)
	if err != nil {
		telemetryError(w, err)
		return
	}
	drops, err := h.telemetry.VMDrops(ctx, v.OrgID, v.ID, from, to)
	if err != nil {
		telemetryError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"t": u.T, "charts": u.Charts,
		"dropped": map[string]int64{"logs": drops["logs"], "series": drops["series"], "body": drops["body"]}})
}

// vmLogQuery parses the shared log filters plus the VM's stream.
func (h *api) vmLogQuery(w http.ResponseWriter, r *http.Request) (telemetry.LogQuery, bool) {
	if h.telemetry == nil {
		writeError(w, http.StatusNotFound, "not found")
		return telemetry.LogQuery{}, false
	}
	v, ok := h.vmFromPath(w, r, store.ActionVMMetrics)
	if !ok {
		return telemetry.LogQuery{}, false
	}
	q, ok := logQuery(w, r)
	if !ok {
		return q, false
	}
	q.Org, q.VM, q.Stream = v.OrgID, v.ID, r.URL.Query().Get("stream")
	switch q.Stream {
	case "", "service", "console", "app", "system", "egress":
	default:
		writeError(w, http.StatusBadRequest, "invalid stream")
		return q, false
	}
	return q, true
}

func (h *api) vmLogs(w http.ResponseWriter, r *http.Request) {
	if q, ok := h.vmLogQuery(w, r); ok {
		h.writeLogs(w, r, q)
	}
}

func (h *api) tailVMLogs(w http.ResponseWriter, r *http.Request) {
	if q, ok := h.vmLogQuery(w, r); ok {
		h.tailLogs(w, r, q, "vm", q.VM, nil)
	}
}

// vmProcesses proxies the live list from the VM's node; cpuPercent is percent of one core, like top.
func (h *api) vmProcesses(w http.ResponseWriter, r *http.Request) {
	v, ok := h.vmFromPath(w, r, store.ActionVMMetrics)
	if !ok {
		return
	}
	if v.State != proto.StateRunning {
		writeError(w, http.StatusConflict, "vm is not running")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), vmProcessesTimeout)
	defer cancel()
	ps, err := h.fleet.Processes(ctx, v.NodeID, v.ID)
	switch {
	case errors.Is(err, fleet.ErrVMNotRunning):
		writeError(w, http.StatusConflict, "vm is not running")
	case errors.Is(err, fleet.ErrNodeOffline):
		writeError(w, http.StatusBadGateway, "node offline")
	case errors.Is(err, context.DeadlineExceeded):
		writeError(w, http.StatusGatewayTimeout, "node did not answer in time")
	case err != nil:
		slog.Warn("vm processes", "vm", v.ID, "err", err)
		writeError(w, http.StatusBadGateway, "process list unavailable")
	default:
		if ps == nil {
			ps = []proto.GuestProcess{}
		}
		writeJSON(w, http.StatusOK, map[string]any{"processes": ps})
	}
}

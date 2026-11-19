package fleet

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"time"

	"github.com/google/uuid"

	"github.com/asterism-dev/observatory/internal/authz"
	"github.com/asterism-dev/observatory/internal/proto"
	"github.com/asterism-dev/observatory/internal/store"
	"github.com/asterism-dev/observatory/internal/version"
)

var (
	ErrUpdateRunning = errors.New("an update run is already active")
	ErrNotUpdatable  = errors.New("node is not updatable")
)

// UpdateRun.State is running, done, failed or cancelled.
type UpdateRun struct {
	ID        uuid.UUID
	Org       uuid.UUID
	StartedBy uuid.UUID
	State     string
	Nodes     []UpdateNode
	StartedAt time.Time
	EndedAt   *time.Time
}

// UpdateNode.Status is pending, updating, done, failed or skipped (not reached after a failure or cancel).
type UpdateNode struct {
	ID     uuid.UUID
	Status string
	Reason string
}

// updateRun is guarded by Fleet.mu.
type updateRun struct {
	UpdateRun
	cancelled bool
}

type updateTimeouts struct{ back, vms, poll time.Duration }

func (f *Fleet) Updatable(n store.Node) bool {
	return f.Online(n.ID) && version.Older(n.Version, version.Version) && n.Hypervisor != nil
}

// StartUpdate updates nodes one at a time in list order; the run outlives ctx and ends only on its own or by CancelUpdate.
func (f *Fleet) StartUpdate(_ context.Context, org, by uuid.UUID, nodes []store.Node) (UpdateRun, error) {
	for _, n := range nodes {
		if n.OrgID != org || !f.Updatable(n) {
			return UpdateRun{}, ErrNotUpdatable
		}
	}
	r := &updateRun{UpdateRun: UpdateRun{ID: uuid.New(), Org: org, StartedBy: by, State: "running", StartedAt: time.Now()}}
	for _, n := range nodes {
		r.Nodes = append(r.Nodes, UpdateNode{ID: n.ID, Status: "pending"})
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if old := f.runs[org]; old != nil && old.State == "running" {
		return UpdateRun{}, ErrUpdateRunning
	}
	f.runs[org] = r
	go f.runUpdate(r, nodes)
	return r.copy(), nil
}

// CurrentUpdate returns the org's active or last run.
func (f *Fleet) CurrentUpdate(org uuid.UUID) (UpdateRun, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	r := f.runs[org]
	if r == nil {
		return UpdateRun{}, false
	}
	return r.copy(), true
}

// CancelUpdate lets the node being updated finish and starts no further one.
func (f *Fleet) CancelUpdate(org uuid.UUID) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	r := f.runs[org]
	if r == nil || r.State != "running" {
		return false
	}
	r.cancelled = true
	return true
}

func (f *Fleet) Updating(node uuid.UUID) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, r := range f.runs {
		if slices.ContainsFunc(r.Nodes, func(n UpdateNode) bool { return n.ID == node && n.Status == "updating" }) {
			return true
		}
	}
	return false
}

// copy needs Fleet.mu held.
func (r *updateRun) copy() UpdateRun {
	out := r.UpdateRun
	out.Nodes = slices.Clone(r.Nodes)
	if r.EndedAt != nil {
		t := *r.EndedAt
		out.EndedAt = &t
	}
	return out
}

func (f *Fleet) runUpdate(r *updateRun, nodes []store.Node) {
	state := "done"
	for i, n := range nodes {
		f.mu.Lock()
		stop := state != "done" || r.cancelled
		if stop {
			r.Nodes[i].Status = "skipped"
			if state == "done" {
				state = "cancelled"
			}
		} else {
			r.Nodes[i].Status = "updating"
		}
		f.mu.Unlock()
		if stop {
			continue
		}
		reason := f.updateNode(n)
		result, res := "done", store.ResultAllowed
		if reason != "" {
			result, res, state = "failed", store.ResultFailed, "failed"
			slog.Warn("node update failed", "node", n.ID, "run", r.ID, "reason", reason)
		}
		f.mu.Lock()
		r.Nodes[i].Status, r.Nodes[i].Reason = result, reason
		f.mu.Unlock()
		err := f.store.Audit(context.Background(), store.AuditEntry{ActorKind: store.ActorUser, ActorID: r.StartedBy.String(), OrgID: &r.Org,
			Action: store.ActionNodeUpdate, Resource: authz.NodeObject(n.ID), Result: res,
			Details: map[string]any{"name": n.Name, "update": result, "reason": reason, "run": r.ID.String()}})
		if err != nil {
			slog.Error("audit node update", "node", n.ID, "err", err)
		}
	}
	f.mu.Lock()
	now := time.Now()
	r.State, r.EndedAt = state, &now
	f.mu.Unlock()
}

// updateNode asks the node to update and waits until it runs the hub's version with its VMs back; it returns why that failed, or "".
func (f *Fleet) updateNode(n store.Node) string {
	ctx := context.Background()
	f.mu.Lock()
	c := f.conns[n.ID]
	delete(f.updateErrs, n.ID)
	t := f.updateTimeouts
	f.mu.Unlock()
	if c == nil {
		return "node offline"
	}
	sent := time.Now()
	select {
	case c.update <- struct{}{}:
	default:
	}
	tick := time.NewTicker(t.poll)
	defer tick.Stop()
	var backAt time.Time
	for ; ; <-tick.C {
		f.mu.Lock()
		report, failed := f.updateErrs[n.ID]
		c := f.conns[n.ID]
		var since time.Time
		if c != nil {
			since = c.since
		}
		f.mu.Unlock()
		if failed {
			return report
		}
		if backAt.IsZero() {
			// The old binary may reconnect while the installer reconfigures networking; only the new version counts.
			if since.After(sent) {
				cur, err := f.store.Node(ctx, n.OrgID, n.ID)
				switch {
				case errors.Is(err, store.ErrNotFound):
					return "node deleted"
				case err != nil:
					slog.Error("node update: load node", "node", n.ID, "err", err)
				case cur.Version == version.Version:
					backAt = time.Now()
					continue
				}
			}
			if time.Since(sent) > t.back {
				return fmt.Sprintf("timed out after %s waiting for the node to run %s", t.back, version.Version)
			}
			continue
		}
		waiting, reason, err := f.vmsBack(ctx, n.ID, c)
		switch {
		case err != nil:
			slog.Error("node update: load vms", "node", n.ID, "err", err)
		case reason != "":
			return reason
		case waiting == "":
			return ""
		}
		if time.Since(backAt) > t.vms {
			return fmt.Sprintf("timed out after %s waiting for vm %s", t.vms, waiting)
		}
	}
}

// vmsBack names a VM not yet settled on c, or the failure of one that reported an error there; a VM counts only once c saw its status.
func (f *Fleet) vmsBack(ctx context.Context, node uuid.UUID, c *conn) (waiting, failure string, _ error) {
	vms, err := f.store.VMsOnNode(ctx, node)
	if err != nil {
		return "", "", err
	}
	f.mu.Lock()
	seen := make([]bool, len(vms))
	for i, v := range vms {
		seen[i] = c != nil && c.seen[v.ID.String()]
	}
	f.mu.Unlock()
	for i, v := range vms {
		switch {
		case !seen[i]:
			waiting = cmp.Or(waiting, v.Name)
		case v.State == proto.StateError:
			return "", fmt.Sprintf("vm %s: %s", v.Name, v.Error), nil
		case !settled(v.Power, v.State):
			waiting = cmp.Or(waiting, v.Name)
		}
	}
	return waiting, "", nil
}

func settled(p proto.Power, s proto.VMState) bool {
	switch p {
	case proto.PowerRunning:
		return s == proto.StateRunning || s == proto.StateSuspended
	case proto.PowerPaused:
		return s == proto.StatePaused
	case proto.PowerHibernated:
		return s == proto.StateHibernated || s == proto.StateStopped
	case proto.PowerStopped:
		return s == proto.StateStopped
	}
	return false
}

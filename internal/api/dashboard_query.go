package api

import (
	"context"
	"errors"
	"net/http"
	"regexp"
	"time"

	"github.com/google/uuid"

	"github.com/asterism-dev/observatory/internal/auth"
	"github.com/asterism-dev/observatory/internal/authz"
	"github.com/asterism-dev/observatory/internal/store"
	"github.com/asterism-dev/observatory/internal/telemetry"
)

var dashboardQueryTimeout = 30 * time.Second

var (
	orgVMMetrics   = authz.Rule{Effect: authz.Allow, Object: authz.ObjVMs + "/*", Action: store.ActionVMMetrics}
	orgNodeMetrics = authz.Rule{Effect: authz.Allow, Object: authz.ObjNodes + "/*", Action: store.ActionNodeMetrics}
	storeURL       = regexp.MustCompile(`[a-zA-Z][a-zA-Z0-9+.-]*://\S+`)
)

// dashboardScope is what the caller may see of the org's metrics; org-wide grants count only without any deny under them.
func (h *api) dashboardScope(r *http.Request, org uuid.UUID) (telemetry.Scope, error) {
	p, _ := auth.PrincipalFrom(r.Context())
	e, sub := h.z.Enforcer(), authz.UserSubject(p.User.ID)
	holds := func(rule authz.Rule) (bool, error) {
		err := e.CanHold(sub, org, []authz.Rule{rule})
		if errors.Is(err, authz.ErrEscalation) {
			return false, nil
		}
		return err == nil, err
	}
	var s telemetry.Scope
	allVMs, err := holds(orgVMMetrics)
	if err != nil {
		return s, err
	}
	if s.Nodes, err = holds(orgNodeMetrics); err != nil {
		return s, err
	}
	if s.OrgWide = allVMs && s.Nodes; s.OrgWide {
		return s, nil
	}
	vms, err := h.store.VMs(r.Context(), org)
	if err != nil {
		return s, err
	}
	for _, v := range vms {
		if e.Allowed(sub, org, authz.VMObject(v.ID), store.ActionVMMetrics) {
			s.VMs = append(s.VMs, v.ID)
		}
	}
	return s, nil
}

// dashboardRequest resolves the org, applies the caller's rate limit and computes their scope; it writes errors itself.
func (h *api) dashboardRequest(w http.ResponseWriter, r *http.Request) (org uuid.UUID, scope telemetry.Scope, ok bool) {
	if h.telemetry == nil {
		writeError(w, http.StatusNotFound, "telemetry is off")
		return org, scope, false
	}
	m, ok := h.orgFromPath(w, r)
	if !ok {
		return org, scope, false
	}
	p, _ := auth.PrincipalFrom(r.Context())
	if !h.dashboardLimit.allow(p.User.ID, h.now()) {
		writeError(w, http.StatusTooManyRequests, "rate limit exceeded")
		return org, scope, false
	}
	scope, err := h.dashboardScope(r, m.Org.ID)
	if err != nil {
		serverError(w, "dashboard scope", err)
		return org, scope, false
	}
	return m.Org.ID, scope, true
}

// dashboardQueryError turns a rejected query into a 400 with the store's text, stripped of URLs that could point at it.
func dashboardQueryError(w http.ResponseWriter, err error) {
	if rej := (*telemetry.RejectedError)(nil); errors.As(err, &rej) {
		writeError(w, http.StatusBadRequest, storeURL.ReplaceAllString(rej.Msg, "<url>"))
		return
	}
	telemetryError(w, err)
}

func (h *api) dashboardQuery(w http.ResponseWriter, r *http.Request) {
	org, scope, ok := h.dashboardRequest(w, r)
	if !ok {
		return
	}
	var body struct {
		Query          string
		From, To, Time *time.Time
		Step           float64
	}
	if !readJSON(w, r, &body) {
		return
	}
	switch {
	case body.Query == "" || len(body.Query) > maxPanelQuery:
		writeError(w, http.StatusBadRequest, "query must be 1–4096 bytes")
		return
	case body.Step < 0:
		writeError(w, http.StatusBadRequest, "invalid step")
		return
	case (body.From == nil) != (body.To == nil):
		writeError(w, http.StatusBadRequest, "from and to go together")
		return
	case body.From != nil && (!body.From.Before(*body.To) || body.To.Sub(*body.From) > maxTelemetrySpan):
		writeError(w, http.StatusBadRequest, "invalid range")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), dashboardQueryTimeout)
	defer cancel()
	if body.From != nil {
		res, err := h.telemetry.RawQuery(ctx, org, scope, body.Query, *body.From, *body.To, time.Duration(body.Step*float64(time.Second)))
		if err != nil {
			dashboardQueryError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, res)
		return
	}
	at := h.now()
	if body.Time != nil {
		at = *body.Time
	}
	series, err := h.telemetry.Instant(ctx, org, scope, body.Query, at)
	if err != nil {
		dashboardQueryError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"series": series})
}

func (h *api) dashboardMetricNames(w http.ResponseWriter, r *http.Request) {
	org, scope, ok := h.dashboardRequest(w, r)
	if !ok {
		return
	}
	match := r.URL.Query().Get("match")
	if len(match) > maxPanelQuery {
		writeError(w, http.StatusBadRequest, "match too long")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), dashboardQueryTimeout)
	defer cancel()
	names, err := h.telemetry.MetricNames(ctx, org, scope, match)
	if err != nil {
		dashboardQueryError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"names": names})
}

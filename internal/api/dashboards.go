package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"

	"github.com/asterism-dev/observatory/internal/auth"
	"github.com/asterism-dev/observatory/internal/authz"
	"github.com/asterism-dev/observatory/internal/platform/textx"
	"github.com/asterism-dev/observatory/internal/store"
)

const (
	maxPanels     = 20
	maxPanelQuery = 4 << 10
)

var (
	panelTypes  = []string{"timeseries", "stat"}
	panelUnits  = []string{"none", "percent", "bytes", "bytes_per_second", "seconds", "requests_per_second"}
	panelWidths = []string{"half", "full"}
)

type panel struct {
	Title string `json:"title"`
	Type  string `json:"type"`
	Query string `json:"query"`
	Unit  string `json:"unit"`
	Width string `json:"width"`
}

type dashboardJSON struct {
	ID        uuid.UUID       `json:"id"`
	Name      string          `json:"name"`
	Scope     store.EnvScope  `json:"scope"`
	OwnerID   *uuid.UUID      `json:"ownerId"`
	Panels    json.RawMessage `json:"panels"`
	CreatedBy uuid.UUID       `json:"createdBy"`
	CreatedAt time.Time       `json:"createdAt"`
	UpdatedAt time.Time       `json:"updatedAt"`
	CanEdit   bool            `json:"canEdit"`
}

type dashboardBody struct {
	Name      string
	Scope     store.EnvScope
	TeamID    *uuid.UUID
	Panels    []panel
	UpdatedAt *time.Time
}

func writeFieldError(w http.ResponseWriter, field, msg string) {
	writeJSON(w, http.StatusBadRequest, map[string]string{"error": field + ": " + msg, "field": field})
}

// validDashboard writes a 400 naming the first bad field and returns the panels re-encoded without unknown keys.
func validDashboard(w http.ResponseWriter, b *dashboardBody) (json.RawMessage, bool) {
	b.Name = strings.TrimSpace(b.Name)
	if n := utf8.RuneCountInString(b.Name); n < 1 || n > 100 || textx.Sanitize(b.Name) != b.Name {
		writeFieldError(w, "name", "must be 1–100 characters without control characters")
		return nil, false
	}
	if len(b.Panels) > maxPanels {
		writeFieldError(w, "panels", fmt.Sprintf("at most %d panels", maxPanels))
		return nil, false
	}
	for i, p := range b.Panels {
		field := fmt.Sprintf("panels[%d].", i)
		switch {
		case utf8.RuneCountInString(p.Title) > 100 || textx.Sanitize(p.Title) != p.Title:
			writeFieldError(w, field+"title", "must be at most 100 characters without control characters")
		case !slices.Contains(panelTypes, p.Type):
			writeFieldError(w, field+"type", "must be one of "+strings.Join(panelTypes, ", "))
		case strings.TrimSpace(p.Query) == "" || len(p.Query) > maxPanelQuery:
			writeFieldError(w, field+"query", "must be non-empty and at most 4 KiB")
		case !slices.Contains(panelUnits, p.Unit):
			writeFieldError(w, field+"unit", "must be one of "+strings.Join(panelUnits, ", "))
		case !slices.Contains(panelWidths, p.Width):
			writeFieldError(w, field+"width", "must be one of "+strings.Join(panelWidths, ", "))
		default:
			continue
		}
		return nil, false
	}
	if b.Panels == nil {
		b.Panels = []panel{}
	}
	raw, err := json.Marshal(b.Panels)
	if err != nil {
		serverError(w, "encode panels", err)
		return nil, false
	}
	return raw, true
}

func dashboardObject(scope store.EnvScope, owner *uuid.UUID) string {
	switch scope {
	case store.EnvScopeTeam:
		return authz.TeamDashboardsObject(*owner)
	case store.EnvScopeUser:
		return authz.UserDashboardsObject(*owner)
	}
	return authz.ObjDashboards
}

func dashboardAudit(r *http.Request, action string, d store.Dashboard) store.AuditEntry {
	e := auditEntry(r, &d.OrgID, action, dashboardObject(d.Scope, d.OwnerID), store.ResultAllowed)
	e.Details = map[string]any{"dashboardId": d.ID, "name": d.Name, "scope": d.Scope, "ownerId": d.OwnerID}
	return e
}

// dashboardAccess answers read and edit for one caller; teams are the caller's team IDs.
type dashboardAccess struct {
	e     *authz.Enforcer
	sub   string
	user  uuid.UUID
	org   uuid.UUID
	teams []uuid.UUID
}

func (h *api) dashboardAccess(r *http.Request, org uuid.UUID) (dashboardAccess, error) {
	p, _ := auth.PrincipalFrom(r.Context())
	teams, err := h.store.UserTeamIDs(r.Context(), org, p.User.ID)
	return dashboardAccess{h.z.Enforcer(), authz.UserSubject(p.User.ID), p.User.ID, org, teams}, err
}

// hidden: another member's private dashboard is never shown, whatever rules a custom role grants.
func (a dashboardAccess) hidden(d store.Dashboard) bool {
	return d.Scope == store.EnvScopeUser && *d.OwnerID != a.user
}

func (a dashboardAccess) canRead(d store.Dashboard) bool {
	return !a.hidden(d) && a.e.Allowed(a.sub, a.org, dashboardObject(d.Scope, d.OwnerID), "telemetry.dashboards.get")
}

// canEdit adds the creator rule: a team dashboard's creator while in the team, an org dashboard's creator always.
func (a dashboardAccess) canEdit(d store.Dashboard) bool {
	if a.hidden(d) {
		return false
	}
	if a.e.Allowed(a.sub, a.org, dashboardObject(d.Scope, d.OwnerID), "telemetry.dashboards.update") {
		return true
	}
	return d.CreatedBy == a.user && a.canRead(d) && (d.Scope != store.EnvScopeTeam || slices.Contains(a.teams, *d.OwnerID))
}

func (a dashboardAccess) json(d store.Dashboard) dashboardJSON {
	return dashboardJSON{d.ID, d.Name, d.Scope, d.OwnerID, d.Panels, d.CreatedBy, d.CreatedAt, d.UpdatedAt, a.canEdit(d)}
}

func (h *api) listDashboards(w http.ResponseWriter, r *http.Request) {
	m, ok := h.orgFromPath(w, r)
	if !ok {
		return
	}
	ds, err := h.store.Dashboards(r.Context(), m.Org.ID)
	if err != nil {
		serverError(w, "dashboards", err)
		return
	}
	a, err := h.dashboardAccess(r, m.Org.ID)
	if err != nil {
		serverError(w, "user teams", err)
		return
	}
	out := []dashboardJSON{}
	for _, d := range ds {
		if a.canRead(d) {
			out = append(out, a.json(d))
		}
	}
	writeJSON(w, http.StatusOK, out)
}

// dashboardFromPath loads {id} and checks read access; another member's private dashboard is 404.
func (h *api) dashboardFromPath(w http.ResponseWriter, r *http.Request) (store.Dashboard, dashboardAccess, bool) {
	m, ok := h.orgFromPath(w, r)
	if !ok {
		return store.Dashboard{}, dashboardAccess{}, false
	}
	id, ok := pathID(w, r, "id")
	if !ok {
		return store.Dashboard{}, dashboardAccess{}, false
	}
	a, err := h.dashboardAccess(r, m.Org.ID)
	if err != nil {
		serverError(w, "user teams", err)
		return store.Dashboard{}, dashboardAccess{}, false
	}
	d, err := h.store.Dashboard(r.Context(), m.Org.ID, id)
	if errors.Is(err, store.ErrNotFound) || (err == nil && a.hidden(d)) {
		writeError(w, http.StatusNotFound, "not found")
		return store.Dashboard{}, dashboardAccess{}, false
	} else if err != nil {
		serverError(w, "dashboard", err)
		return store.Dashboard{}, dashboardAccess{}, false
	}
	return d, a, h.authorize(w, r, m.Org.ID, dashboardObject(d.Scope, d.OwnerID), "telemetry.dashboards.get")
}

func (h *api) dashboardEditable(w http.ResponseWriter, r *http.Request, a dashboardAccess, d store.Dashboard) bool {
	if a.canEdit(d) {
		return true
	}
	h.auditDenied(r, d.OrgID, dashboardObject(d.Scope, d.OwnerID), "telemetry.dashboards.update")
	writeError(w, http.StatusForbidden, "forbidden")
	return false
}

func (h *api) getDashboard(w http.ResponseWriter, r *http.Request) {
	d, a, ok := h.dashboardFromPath(w, r)
	if ok {
		writeJSON(w, http.StatusOK, a.json(d))
	}
}

func (h *api) createDashboard(w http.ResponseWriter, r *http.Request) {
	m, ok := h.orgFromPath(w, r)
	if !ok {
		return
	}
	var body dashboardBody
	if !readJSON(w, r, &body) {
		return
	}
	panels, ok := validDashboard(w, &body)
	if !ok {
		return
	}
	owner, ok := h.envTarget(w, r, m.Org.ID, body.Scope, body.TeamID)
	if !ok || !h.authorize(w, r, m.Org.ID, dashboardObject(body.Scope, owner), "telemetry.dashboards.create") {
		return
	}
	a, err := h.dashboardAccess(r, m.Org.ID)
	if err != nil {
		serverError(w, "user teams", err)
		return
	}
	d := store.Dashboard{OrgID: m.Org.ID, Scope: body.Scope, OwnerID: owner, Name: body.Name, Panels: panels, CreatedBy: a.user}
	if !h.commit(w, r, func(tx *store.Store) error {
		var err error
		if d, err = tx.CreateDashboard(r.Context(), d); err != nil {
			return err
		}
		return tx.Audit(r.Context(), dashboardAudit(r, store.ActionDashboardCreate, d))
	}) {
		return
	}
	writeJSON(w, http.StatusCreated, a.json(d))
}

func (h *api) updateDashboard(w http.ResponseWriter, r *http.Request) {
	old, a, ok := h.dashboardFromPath(w, r)
	if !ok || !h.dashboardEditable(w, r, a, old) {
		return
	}
	var body dashboardBody
	if !readJSON(w, r, &body) {
		return
	}
	if body.UpdatedAt == nil {
		writeFieldError(w, "updatedAt", "required")
		return
	}
	// The store compares against the row as it is now; checking it here too means the rights above were checked on that row.
	if !body.UpdatedAt.Equal(old.UpdatedAt) {
		writeError(w, http.StatusConflict, store.ErrConflict.Error())
		return
	}
	panels, ok := validDashboard(w, &body)
	if !ok {
		return
	}
	owner, ok := h.envTarget(w, r, old.OrgID, body.Scope, body.TeamID)
	if !ok {
		return
	}
	moved := body.Scope != old.Scope || (owner != nil && *owner != *old.OwnerID)
	// Making a dashboard private would take it from everyone else, so only its creator may.
	if moved && body.Scope == store.EnvScopeUser && old.CreatedBy != a.user {
		h.auditDenied(r, old.OrgID, dashboardObject(old.Scope, old.OwnerID), "telemetry.dashboards.update")
		writeError(w, http.StatusForbidden, "forbidden")
		return
	}
	if moved && !h.authorize(w, r, old.OrgID, dashboardObject(body.Scope, owner), "telemetry.dashboards.create") {
		return
	}
	d := old
	d.Scope, d.OwnerID, d.Name, d.Panels = body.Scope, owner, body.Name, panels
	if !h.commit(w, r, func(tx *store.Store) error {
		var err error
		if d, err = tx.UpdateDashboard(r.Context(), d, *body.UpdatedAt); err != nil {
			return err
		}
		entry := dashboardAudit(r, store.ActionDashboardUpdate, d)
		if moved {
			entry.Details["fromScope"], entry.Details["fromOwnerId"] = old.Scope, old.OwnerID
		}
		return tx.Audit(r.Context(), entry)
	}) {
		return
	}
	writeJSON(w, http.StatusOK, a.json(d))
}

func (h *api) deleteDashboard(w http.ResponseWriter, r *http.Request) {
	d, a, ok := h.dashboardFromPath(w, r)
	if !ok || !h.dashboardEditable(w, r, a, d) {
		return
	}
	if !h.commit(w, r, func(tx *store.Store) error {
		if err := tx.DeleteDashboard(r.Context(), d.OrgID, d.ID); err != nil {
			return err
		}
		return tx.Audit(r.Context(), dashboardAudit(r, store.ActionDashboardDelete, d))
	}) {
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

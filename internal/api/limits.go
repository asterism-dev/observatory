package api

import (
	"math"
	"net/http"

	"github.com/google/uuid"

	"github.com/asterism-dev/observatory/internal/authz"
	"github.com/asterism-dev/observatory/internal/store"
)

type limitsJSON struct {
	VCPU    *int `json:"vcpu"`
	RAMMB   *int `json:"ramMb"`
	DiskGB  *int `json:"diskGb"`
	VMCount *int `json:"vmCount"`
}

func (l limitsJSON) store() store.Limits {
	return store.Limits{VCPU: l.VCPU, RAMMB: l.RAMMB, DiskGB: l.DiskGB, VMCount: l.VMCount}
}

func toLimitsJSON(l store.Limits) limitsJSON { return limitsJSON{l.VCPU, l.RAMMB, l.DiskGB, l.VMCount} }

type orgLimitsJSON struct {
	Org     limitsJSON            `json:"org"`
	Teams   map[string]limitsJSON `json:"teams"`
	Members map[string]limitsJSON `json:"members"`
}

func toOrgLimitsJSON(ls map[string]store.Limits) orgLimitsJSON {
	out := orgLimitsJSON{Org: toLimitsJSON(ls[store.ScopeOrg]), Teams: map[string]limitsJSON{}, Members: map[string]limitsJSON{}}
	for scope, l := range ls {
		switch {
		case len(scope) > 5 && scope[:5] == "team:":
			out.Teams[scope[5:]] = toLimitsJSON(l)
		case len(scope) > 5 && scope[:5] == "user:":
			out.Members[scope[5:]] = toLimitsJSON(l)
		}
	}
	return out
}

// readLimits accepts non-negative ints or null; absent fields mean unlimited.
func readLimits(w http.ResponseWriter, r *http.Request) (store.Limits, bool) {
	var body limitsJSON
	if !readJSON(w, r, &body) {
		return store.Limits{}, false
	}
	for _, v := range []*int{body.VCPU, body.RAMMB, body.DiskGB, body.VMCount} {
		if v != nil && (*v < 0 || *v > math.MaxInt32) {
			writeError(w, http.StatusBadRequest, "invalid limit")
			return store.Limits{}, false
		}
	}
	return body.store(), true
}

func limitDetails(l store.Limits) map[string]any {
	return map[string]any{"vcpu": l.VCPU, "ramMb": l.RAMMB, "diskGb": l.DiskGB, "vmCount": l.VMCount}
}

func (h *api) getLimits(w http.ResponseWriter, r *http.Request) {
	m, ok := h.orgFromPath(w, r)
	if !ok || !h.authorize(w, r, m.Org.ID, authz.ObjLimits, "limit:read") {
		return
	}
	ls, err := h.store.Limits(r.Context(), m.Org.ID)
	if err != nil {
		serverError(w, "limits", err)
		return
	}
	writeJSON(w, http.StatusOK, toOrgLimitsJSON(ls))
}

// setScopeLimits handles team and member scopes; the org scope belongs to platform admins only.
func (h *api) setScopeLimits(w http.ResponseWriter, r *http.Request) {
	m, ok := h.orgFromPath(w, r)
	if !ok || !h.authorize(w, r, m.Org.ID, authz.ObjLimits, store.ActionLimitSet) {
		return
	}
	scope := r.PathValue("scope")
	if scope == store.ScopeOrg {
		h.auditDenied(r, m.Org.ID, authz.ObjLimits+"/"+scope, store.ActionLimitSet)
		writeError(w, http.StatusForbidden, "forbidden")
		return
	}
	l, ok := readLimits(w, r)
	if !ok {
		return
	}
	h.commitLimits(w, r, m.Org.ID, scope, l)
}

func (h *api) commitLimits(w http.ResponseWriter, r *http.Request, org uuid.UUID, scope string, l store.Limits) {
	if !h.commit(w, r, func(tx *store.Store) error {
		if err := tx.SetLimits(r.Context(), org, scope, l); err != nil {
			return err
		}
		e := auditEntry(r, &org, store.ActionLimitSet, authz.ObjLimits+"/"+scope, store.ResultAllowed)
		e.Details = limitDetails(l)
		return tx.Audit(r.Context(), e)
	}) {
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

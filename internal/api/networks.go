package api

import (
	"errors"
	"net/http"
	"regexp"

	"github.com/google/uuid"

	"github.com/asterism-dev/observatory/internal/auth"
	"github.com/asterism-dev/observatory/internal/authz"
	"github.com/asterism-dev/observatory/internal/netaddr"
	"github.com/asterism-dev/observatory/internal/store"
)

// Network names become DNS labels.
var networkNamePattern = regexp.MustCompile(`^[a-z][a-z0-9-]{0,30}$`)

type networkJSON struct {
	ID        uuid.UUID `json:"id"`
	Name      string    `json:"name"`
	Subnet    string    `json:"subnet"`
	VMCount   int       `json:"vmCount"`
	CanUse    bool      `json:"canUse"`
	CanDelete bool      `json:"canDelete"`
}

func networkAudit(r *http.Request, action string, n store.Network) store.AuditEntry {
	e := auditEntry(r, &n.OrgID, action, authz.NetworkObject(n.ID), store.ResultAllowed)
	e.Details = map[string]any{"name": n.Name}
	return e
}

func (h *api) listNetworks(w http.ResponseWriter, r *http.Request) {
	m, ok := h.orgFromPath(w, r)
	if !ok || !h.authorize(w, r, m.Org.ID, authz.ObjNetworks, "network.networks.get") {
		return
	}
	ns, err := h.store.Networks(r.Context(), m.Org.ID)
	if err != nil {
		serverError(w, "networks", err)
		return
	}
	p, _ := auth.PrincipalFrom(r.Context())
	e, sub := h.z.Enforcer(), authz.UserSubject(p.User.ID)
	out := []networkJSON{}
	for _, n := range ns {
		obj := authz.NetworkObject(n.ID)
		out = append(out, networkJSON{n.ID, n.Name, n.Subnet.String(), n.VMCount,
			e.Allowed(sub, m.Org.ID, obj, "network.networks.use"), e.Allowed(sub, m.Org.ID, obj, "network.networks.delete")})
	}
	writeJSON(w, http.StatusOK, out)
}

func (h *api) createNetwork(w http.ResponseWriter, r *http.Request) {
	m, ok := h.orgFromPath(w, r)
	if !ok {
		return
	}
	var body struct{ Name, Subnet string }
	if !readJSON(w, r, &body) {
		return
	}
	if !networkNamePattern.MatchString(body.Name) {
		writeError(w, http.StatusBadRequest, "invalid name")
		return
	}
	if !h.authorize(w, r, m.Org.ID, authz.ObjNetworks, "network.networks.create") {
		return
	}
	p, _ := auth.PrincipalFrom(r.Context())
	n := store.Network{OrgID: m.Org.ID, Name: body.Name, CreatedBy: p.User.ID}
	if body.Subnet != "" {
		sn, err := netaddr.ValidateSubnet(body.Subnet)
		if err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		n.Subnet = sn
	}
	if !h.commit(w, r, func(tx *store.Store) error {
		var err error
		if n, err = tx.CreateNetwork(r.Context(), n); err != nil {
			return err
		}
		e := networkAudit(r, store.ActionNetworkCreate, n)
		e.Details["subnet"] = n.Subnet.String()
		return tx.Audit(r.Context(), e)
	}) {
		return
	}
	e, sub, obj := h.z.Enforcer(), authz.UserSubject(p.User.ID), authz.NetworkObject(n.ID)
	writeJSON(w, http.StatusCreated, networkJSON{n.ID, n.Name, n.Subnet.String(), 0,
		e.Allowed(sub, m.Org.ID, obj, "network.networks.use"), e.Allowed(sub, m.Org.ID, obj, "network.networks.delete")})
}

func (h *api) deleteNetwork(w http.ResponseWriter, r *http.Request) {
	m, ok := h.orgFromPath(w, r)
	if !ok {
		return
	}
	id, ok := pathID(w, r, "id")
	if !ok {
		return
	}
	n, err := h.store.Network(r.Context(), m.Org.ID, id)
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "not found")
		return
	}
	if err != nil {
		serverError(w, "network", err)
		return
	}
	if !h.authorize(w, r, n.OrgID, authz.NetworkObject(n.ID), "network.networks.delete") {
		return
	}
	if !h.commit(w, r, func(tx *store.Store) error {
		if err := tx.DeleteNetwork(r.Context(), n.OrgID, n.ID); err != nil {
			return err
		}
		return tx.Audit(r.Context(), networkAudit(r, store.ActionNetworkDelete, n))
	}) {
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

package api

import (
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/google/uuid"

	"github.com/asterism-dev/observatory/internal/auth"
	"github.com/asterism-dev/observatory/internal/authz"
	"github.com/asterism-dev/observatory/internal/netaddr"
	"github.com/asterism-dev/observatory/internal/store"
	"github.com/asterism-dev/observatory/internal/wgkey"
)

type deviceJSON struct {
	ID            uuid.UUID  `json:"id"`
	Name          string     `json:"name"`
	IP            string     `json:"ip"`
	Owner         refJSON    `json:"owner"`
	CreatedAt     time.Time  `json:"createdAt"`
	LastHandshake *time.Time `json:"lastHandshake"`
	CanDelete     bool       `json:"canDelete"`
}

func (h *api) deviceJSON(d store.Device, owner string, canDelete bool) deviceJSON {
	return deviceJSON{d.ID, d.Name, d.IP.String(), refJSON{d.UserID, owner}, d.CreatedAt, d.LastHandshake, canDelete}
}

func deviceAudit(r *http.Request, action string, d store.Device) store.AuditEntry {
	e := auditEntry(r, &d.OrgID, action, authz.UserDevicesObject(d.UserID)+"/"+d.ID.String(), store.ResultAllowed)
	e.Details = map[string]any{"name": d.Name, "ip": d.IP.String()}
	return e
}

func (h *api) listDevices(w http.ResponseWriter, r *http.Request) {
	m, ok := h.orgFromPath(w, r)
	if !ok {
		return
	}
	ds, err := h.store.Devices(r.Context(), m.Org.ID)
	if err != nil {
		serverError(w, "devices", err)
		return
	}
	members, err := h.store.Members(r.Context(), m.Org.ID)
	if err != nil {
		serverError(w, "members", err)
		return
	}
	names := map[uuid.UUID]string{}
	for _, mb := range members {
		names[mb.User.ID] = mb.User.Name
	}
	p, _ := auth.PrincipalFrom(r.Context())
	e, sub := h.z.Enforcer(), authz.UserSubject(p.User.ID)
	out := []deviceJSON{}
	for _, d := range ds {
		obj := authz.UserDevicesObject(d.UserID)
		if e.Allowed(sub, m.Org.ID, obj, "device:list") {
			out = append(out, h.deviceJSON(d, names[d.UserID], e.Allowed(sub, m.Org.ID, obj, store.ActionDeviceDelete)))
		}
	}
	writeJSON(w, http.StatusOK, out)
}

func (h *api) createDevice(w http.ResponseWriter, r *http.Request) {
	m, ok := h.orgFromPath(w, r)
	if !ok {
		return
	}
	var body struct{ Name string }
	if !readJSON(w, r, &body) {
		return
	}
	name, ok := validName(body.Name)
	if !ok {
		writeError(w, http.StatusBadRequest, "invalid name")
		return
	}
	p, _ := auth.PrincipalFrom(r.Context())
	if !h.authorize(w, r, m.Org.ID, authz.UserDevicesObject(p.User.ID), store.ActionDeviceCreate) {
		return
	}
	priv, pub, err := wgkey.Generate()
	if err != nil {
		serverError(w, "device key", err)
		return
	}
	d := store.Device{OrgID: m.Org.ID, UserID: p.User.ID, Name: name, PublicKey: pub}
	var slice string
	if !h.commit(w, r, func(tx *store.Store) error {
		var err error
		if d, err = tx.CreateDevice(r.Context(), d); err != nil {
			return err
		}
		s, err := tx.OrgSlice(r.Context(), d.OrgID)
		if err != nil {
			return err
		}
		slice = s.String()
		e := deviceAudit(r, store.ActionDeviceCreate, d)
		e.Details["key"] = pub[:8]
		return tx.Audit(r.Context(), e)
	}) {
		return
	}
	config := fmt.Sprintf(`[Interface]
PrivateKey = %s
Address = %s/32
DNS = %s
MTU = %d

[Peer]
PublicKey = %s
Endpoint = %s
AllowedIPs = %s, %s/32
PersistentKeepalive = 25
`, priv, d.IP, netaddr.GatewayDNS, netaddr.GuestMTU, h.network.ClientsPublicKey(), h.network.ClientsEndpoint(), slice, netaddr.GatewayDNS)
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusCreated, map[string]any{"device": h.deviceJSON(d, p.User.Name, true), "config": config})
}

func (h *api) deleteDevice(w http.ResponseWriter, r *http.Request) {
	m, ok := h.orgFromPath(w, r)
	if !ok {
		return
	}
	id, ok := pathID(w, r, "id")
	if !ok {
		return
	}
	d, err := h.store.Device(r.Context(), m.Org.ID, id)
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "not found")
		return
	}
	if err != nil {
		serverError(w, "device", err)
		return
	}
	if !h.authorize(w, r, d.OrgID, authz.UserDevicesObject(d.UserID), store.ActionDeviceDelete) {
		return
	}
	if !h.commit(w, r, func(tx *store.Store) error {
		if err := tx.DeleteDevice(r.Context(), d.OrgID, d.ID); err != nil {
			return err
		}
		return tx.Audit(r.Context(), deviceAudit(r, store.ActionDeviceDelete, d))
	}) {
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

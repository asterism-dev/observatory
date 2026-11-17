package api

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/google/uuid"

	"github.com/asterism-dev/observatory/internal/auth"
	"github.com/asterism-dev/observatory/internal/authz"
	"github.com/asterism-dev/observatory/internal/netaddr"
	"github.com/asterism-dev/observatory/internal/store"
)

type orgJSON struct {
	ID       uuid.UUID  `json:"id"`
	Name     string     `json:"name"`
	Personal bool       `json:"personal"`
	Role     store.Role `json:"role"`
}

func toOrgJSON(m store.Membership) orgJSON {
	return orgJSON{m.Org.ID, m.Org.Name, m.Org.Personal, m.Role}
}

// orgFromPath returns the caller's membership in {org}; anything else is 404 so org existence doesn't leak.
func (h *api) orgFromPath(w http.ResponseWriter, r *http.Request) (store.Membership, bool) {
	p, _ := auth.PrincipalFrom(r.Context())
	raw := r.PathValue("org")
	if id, err := uuid.Parse(raw); err == nil && id.String() == raw {
		m, err := h.store.Membership(r.Context(), id, p.User.ID)
		if err == nil {
			return m, true
		}
		if !errors.Is(err, store.ErrNotFound) {
			serverError(w, "membership", err)
			return store.Membership{}, false
		}
	}
	writeError(w, http.StatusNotFound, "not found")
	return store.Membership{}, false
}

// authorize audits and 403s a denial; it doesn't check membership, so resolve org via orgFromPath first.
func (h *api) authorize(w http.ResponseWriter, r *http.Request, org uuid.UUID, obj, act string) bool {
	p, _ := auth.PrincipalFrom(r.Context())
	if h.z.Enforcer().Allowed(authz.UserSubject(p.User.ID), org, obj, act) {
		return true
	}
	h.auditDenied(r, org, obj, act)
	writeError(w, http.StatusForbidden, "forbidden")
	return false
}

func (h *api) auditDenied(r *http.Request, org uuid.UUID, obj, act string) {
	if err := h.store.Audit(r.Context(), auditEntry(r, &org, act, obj, store.ResultDenied)); err != nil {
		slog.Error("audit denied access", "org", org, "action", act, "err", err)
	}
}

// errForbidden rolls back a commit whose permission check depends on rows read inside the transaction.
var errForbidden = errors.New("forbidden")

type invalidPolicyError struct{ err error }

func (e invalidPolicyError) Error() string { return e.err.Error() }

var storeErrorStatus = []struct {
	err    error
	status int
}{
	{store.ErrNotFound, http.StatusNotFound},
	{store.ErrLastOwner, http.StatusConflict},
	{store.ErrPersonalOrg, http.StatusConflict},
	{store.ErrConflict, http.StatusConflict},
	{store.ErrStale, http.StatusConflict},
	{store.ErrNotMember, http.StatusBadRequest},
	{store.ErrInviteInvalid, http.StatusBadRequest},
	{store.ErrAlreadyMember, http.StatusConflict},
	{errForbidden, http.StatusForbidden},
	{store.ErrInvalidParent, http.StatusBadRequest},
	{errLiftsOwnDeny, http.StatusForbidden},
	{authz.ErrDenyNotAllowed, http.StatusForbidden},
	{authz.ErrEscalation, http.StatusForbidden},
	{authz.ErrInvalidPattern, http.StatusBadRequest},
	{store.ErrLimitExceedsOrg, http.StatusBadRequest},
	{store.ErrLastPlatformAdmin, http.StatusConflict},
	{store.ErrLimitExceeded, http.StatusConflict},
	{store.ErrOwnsResources, http.StatusConflict},
	{store.ErrNotEnrolled, http.StatusConflict},
	{store.ErrInvalidKey, http.StatusBadRequest},
	{store.ErrRestartRequired, http.StatusConflict},
	{store.ErrInvalidSize, http.StatusBadRequest},
	{store.ErrOwnerMismatch, http.StatusConflict},
	{store.ErrNoStoragePort, http.StatusConflict},
	{store.ErrSubnetOverlap, http.StatusConflict},
	{store.ErrAddressesExhausted, http.StatusConflict},
	{netaddr.ErrInvalidSubnet, http.StatusBadRequest},
}

// commit runs fn in a transaction that only commits if the resulting policy still builds, then reloads the enforcer.
func (h *api) commit(w http.ResponseWriter, r *http.Request, fn func(tx *store.Store) error) bool {
	ctx := r.Context()
	err := h.store.InTx(ctx, func(tx *store.Store) error {
		if err := fn(tx); err != nil {
			return err
		}
		p, err := tx.PolicySnapshot(ctx)
		if err != nil {
			return err
		}
		if _, err := authz.New(p); err != nil {
			return invalidPolicyError{err}
		}
		return nil
	})
	if err != nil {
		var invalid invalidPolicyError
		if errors.As(err, &invalid) {
			slog.Error("policy validation failed", "err", invalid)
			writeError(w, http.StatusBadRequest, "invalid policy")
			return false
		}
		for _, se := range storeErrorStatus {
			if errors.Is(err, se.err) {
				msg := se.err.Error()
				// These name the caller's own missing permission or bad input, which they need to fix the request.
				if se.err == authz.ErrEscalation || se.err == authz.ErrInvalidPattern || se.err == netaddr.ErrInvalidSubnet {
					msg = err.Error()
				}
				if errors.Is(err, errDiskMissing) {
					msg = errDiskMissing.Error()
				}
				writeError(w, se.status, msg)
				return false
			}
		}
		serverError(w, "commit", err)
		return false
	}
	// Committed: report success so clients don't retry and duplicate; a hang-up must not leave the enforcer stale.
	if err := h.z.Reload(context.WithoutCancel(ctx)); err != nil {
		slog.Error("authz reload after commit", "err", err)
	}
	return true
}

func auditEntry(r *http.Request, org *uuid.UUID, action, resource string, result store.Result) store.AuditEntry {
	p, _ := auth.PrincipalFrom(r.Context())
	ip, ua := auth.RequestMeta(r)
	return store.AuditEntry{ActorKind: store.ActorUser, ActorID: p.User.ID.String(), OrgID: org,
		Action: action, Resource: resource, Result: result, IP: ip, UserAgent: ua}
}

func readJSON(w http.ResponseWriter, r *http.Request, v any) bool {
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(v); err != nil {
		writeError(w, http.StatusBadRequest, "invalid body")
		return false
	}
	return true
}

// validName trims s and accepts 1–64 runes without control characters.
func validName(s string) (string, bool) {
	s = strings.TrimSpace(s)
	n := utf8.RuneCountInString(s)
	return s, n >= 1 && n <= 64 && store.SanitizeText(s) == s
}

func readName(w http.ResponseWriter, r *http.Request) (string, bool) {
	var body struct{ Name string }
	if !readJSON(w, r, &body) {
		return "", false
	}
	name, ok := validName(body.Name)
	if !ok {
		writeError(w, http.StatusBadRequest, "invalid name")
	}
	return name, ok
}

func (h *api) createOrg(w http.ResponseWriter, r *http.Request) {
	p, _ := auth.PrincipalFrom(r.Context())
	name, ok := readName(w, r)
	if !ok {
		return
	}
	var org store.Org
	if !h.commit(w, r, func(tx *store.Store) error {
		var err error
		if org, err = tx.CreateOrg(r.Context(), name, p.User.ID); err != nil {
			return err
		}
		return tx.Audit(r.Context(), auditEntry(r, &org.ID, store.ActionOrgCreate, "org/"+org.ID.String(), store.ResultAllowed))
	}) {
		return
	}
	writeJSON(w, http.StatusCreated, toOrgJSON(store.Membership{Org: org, Role: store.RoleOwner}))
}

func (h *api) getOrg(w http.ResponseWriter, r *http.Request) {
	m, ok := h.orgFromPath(w, r)
	if !ok {
		return
	}
	p, _ := auth.PrincipalFrom(r.Context())
	e, sub := h.z.Enforcer(), authz.UserSubject(p.User.ID)
	perms := []string{}
	for _, c := range authz.Catalog {
		// Objects like org/teams/* are checked literally: allowed only if the caller's rules cover every team.
		if e.Allowed(sub, m.Org.ID, c.Object, c.Action) {
			perms = append(perms, c.Action)
		}
	}
	slices.Sort(perms)
	writeJSON(w, http.StatusOK, struct {
		orgJSON
		Permissions []string `json:"permissions"`
	}{toOrgJSON(m), slices.Compact(perms)})
}

func (h *api) renameOrg(w http.ResponseWriter, r *http.Request) {
	m, ok := h.orgFromPath(w, r)
	if !ok || !h.authorize(w, r, m.Org.ID, authz.ObjSettings, store.ActionOrgUpdate) {
		return
	}
	name, ok := readName(w, r)
	if !ok {
		return
	}
	if !h.commit(w, r, func(tx *store.Store) error {
		if err := tx.RenameOrg(r.Context(), m.Org.ID, name); err != nil {
			return err
		}
		e := auditEntry(r, &m.Org.ID, store.ActionOrgUpdate, "org/"+m.Org.ID.String(), store.ResultAllowed)
		e.Details = map[string]any{"name": name}
		return tx.Audit(r.Context(), e)
	}) {
		return
	}
	m.Org.Name = name
	writeJSON(w, http.StatusOK, toOrgJSON(m))
}

func (h *api) orgAudit(w http.ResponseWriter, r *http.Request) {
	m, ok := h.orgFromPath(w, r)
	if !ok || !h.authorize(w, r, m.Org.ID, authz.ObjAudit, store.ActionAuditRead) {
		return
	}
	f, err := auditFilter(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	f.OrgID = &m.Org.ID
	h.writeAuditPage(w, r, f)
}

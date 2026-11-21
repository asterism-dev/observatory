package api

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"unicode/utf8"

	"github.com/google/uuid"

	"github.com/asterism-dev/observatory/internal/auth"
	"github.com/asterism-dev/observatory/internal/authz"
	"github.com/asterism-dev/observatory/internal/egress"
	"github.com/asterism-dev/observatory/internal/netaddr"
	"github.com/asterism-dev/observatory/internal/platform/textx"
	"github.com/asterism-dev/observatory/internal/store"
)

func pathID(w http.ResponseWriter, r *http.Request, name string) (uuid.UUID, bool) {
	raw := r.PathValue(name)
	id, err := uuid.Parse(raw)
	if err != nil || id.String() != raw {
		writeError(w, http.StatusNotFound, "not found")
		return uuid.Nil, false
	}
	return id, true
}

// privileged roles may see the org's builder VMs.
func privileged(role store.Role) bool { return role == store.RoleOwner || role == store.RoleAdmin }

// orgMember is the caller's membership in an org as the policy has it.
type orgMember struct {
	OrgID uuid.UUID
	Role  store.Role
}

// orgFromPath returns the caller's membership in {org} from the policy; anything else is 404 so org existence doesn't leak.
func (h *api) orgFromPath(w http.ResponseWriter, r *http.Request) (orgMember, bool) {
	p, _ := auth.PrincipalFrom(r.Context())
	raw := r.PathValue("org")
	if id, err := uuid.Parse(raw); err == nil && id.String() == raw {
		if role, ok := h.z.Enforcer().Role(id, p.User.ID); ok {
			return orgMember{id, store.Role(role)}, true
		}
	}
	writeError(w, http.StatusNotFound, "not found")
	return orgMember{}, false
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
	{store.ErrImageExists, http.StatusConflict},
	{store.ErrConflict, http.StatusConflict},
	{store.ErrStale, http.StatusConflict},
	{errForbidden, http.StatusForbidden},
	{errSnapshotBusy, http.StatusConflict},
	{errForkPending, http.StatusConflict},
	{store.ErrLimitExceedsOrg, http.StatusBadRequest},
	{store.ErrLimitExceeded, http.StatusConflict},
	{store.ErrNotEnrolled, http.StatusConflict},
	{store.ErrInvalidKey, http.StatusBadRequest},
	{store.ErrHostsNeedSecret, http.StatusBadRequest},
	{store.ErrValueTooShort, http.StatusBadRequest},
	{egress.ErrInvalidHost, http.StatusBadRequest},
	{store.ErrRestartRequired, http.StatusConflict},
	{store.ErrShapeChanged, http.StatusConflict},
	{store.ErrInvalidSize, http.StatusBadRequest},
	{store.ErrOwnerMismatch, http.StatusConflict},
	{store.ErrNoStoragePort, http.StatusConflict},
	{store.ErrSubnetOverlap, http.StatusConflict},
	{store.ErrAddressesExhausted, http.StatusConflict},
	{netaddr.ErrInvalidSubnet, http.StatusBadRequest},
	{store.ErrMultiAccountNotAllowed, http.StatusConflict},
	{store.ErrInvalid, http.StatusBadRequest},
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
		var ce codedError
		if errors.As(err, &ce) {
			writeJSON(w, ce.status, map[string]string{"error": ce.msg, "code": ce.code})
			return false
		}
		var he httpError
		if errors.As(err, &he) {
			writeError(w, he.status, he.msg)
			return false
		}
		var invalid invalidPolicyError
		if errors.As(err, &invalid) {
			slog.Error("policy validation failed", "err", invalid)
			writeError(w, http.StatusBadRequest, "invalid policy")
			return false
		}
		for _, se := range storeErrorStatus {
			if errors.Is(err, se.err) {
				msg := se.err.Error()
				// These name the caller's bad input, which they need to fix the request.
				if se.err == netaddr.ErrInvalidSubnet || se.err == egress.ErrInvalidHost {
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
	return s, n >= 1 && n <= 64 && textx.Sanitize(s) == s
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

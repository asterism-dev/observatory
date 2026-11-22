package iam

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"unicode/utf8"

	"connectrpc.com/connect"
	"github.com/google/uuid"

	"github.com/asterism-dev/observatory/internal/authz"
	"github.com/asterism-dev/observatory/internal/iam/store"
	"github.com/asterism-dev/observatory/internal/platform/textx"
	"github.com/asterism-dev/observatory/internal/platform/uuidx"
)

// errForbidden rolls back a commit whose permission check depends on rows read inside the transaction.
var errForbidden = errors.New("forbidden")

type invalidPolicyError struct{ err error }

func (e invalidPolicyError) Error() string { return e.err.Error() }

// denyRuleError names the deny rule behind a denial; it unwraps to the sentinel.
type denyRuleError struct {
	err  error
	rule authz.Rule
}

func (e denyRuleError) Error() string {
	return e.err.Error() + ": " + string(e.rule.Effect) + " " + e.rule.Action + "@" + e.rule.Object
}

func (e denyRuleError) Unwrap() error { return e.err }

var errorCodes = []struct {
	err  error
	code connect.Code
}{
	{store.ErrNotFound, connect.CodeNotFound},
	{errForbidden, connect.CodePermissionDenied},
	{authz.ErrDenyNotAllowed, connect.CodePermissionDenied},
	{authz.ErrEscalation, connect.CodePermissionDenied},
	{errLiftsOwnDeny, connect.CodePermissionDenied},
	{store.ErrLastOwner, connect.CodeFailedPrecondition},
	{store.ErrPersonalOrg, connect.CodeFailedPrecondition},
	{store.ErrConflict, connect.CodeFailedPrecondition},
	{store.ErrAlreadyMember, connect.CodeFailedPrecondition},
	{store.ErrLastPlatformAdmin, connect.CodeFailedPrecondition},
	{store.ErrOwnsResources, connect.CodeFailedPrecondition},
	{store.ErrNotMember, connect.CodeInvalidArgument},
	{store.ErrInviteInvalid, connect.CodeInvalidArgument},
	{store.ErrInvalidParent, connect.CodeInvalidArgument},
	{store.ErrInvalidKey, connect.CodeInvalidArgument},
	{authz.ErrInvalidPattern, connect.CodeInvalidArgument},
}

// toConnect maps a store or guard error to its Connect code with today's message; anything unknown is logged and hidden.
func toConnect(what string, err error) error {
	var ce *connect.Error
	if errors.As(err, &ce) {
		return ce
	}
	var invalid invalidPolicyError
	if errors.As(err, &invalid) {
		slog.Error("policy validation failed", "err", invalid)
		return connect.NewError(connect.CodeInvalidArgument, errors.New("invalid policy"))
	}
	for _, m := range errorCodes {
		if errors.Is(err, m.err) {
			msg := m.err.Error()
			// These name the caller's own missing permission or bad input, which they need to fix the request.
			if m.err == authz.ErrEscalation || m.err == authz.ErrInvalidPattern {
				msg = err.Error()
			}
			return connect.NewError(m.code, errors.New(msg))
		}
	}
	return internal(what, err)
}

func internal(what string, err error) error {
	slog.Error(what, "err", err)
	return connect.NewError(connect.CodeInternal, errors.New("internal error"))
}

// commit runs fn in a transaction that only commits if the resulting policy still builds, then reloads the enforcer.
func (s *server) commit(ctx context.Context, fn func(tx *store.Store) error) error {
	err := s.Store.InTx(ctx, func(tx *store.Store) error {
		if err := fn(tx); err != nil {
			return err
		}
		_, err := policyEnforcer(ctx, tx)
		return err
	})
	if err != nil {
		return toConnect("commit", err)
	}
	// Committed: report success so clients don't retry and duplicate; a hang-up must not leave the enforcer stale.
	if err := s.Policy.Reload(context.WithoutCancel(ctx)); err != nil {
		slog.Error("authz reload after commit", "err", err)
	}
	return nil
}

// policyEnforcer builds an enforcer from tx's current policy, uncommitted changes included.
func policyEnforcer(ctx context.Context, tx *store.Store) (*authz.Enforcer, error) {
	p, err := tx.PolicySnapshot(ctx)
	if err != nil {
		return nil, err
	}
	e, err := authz.New(p)
	if err != nil {
		return nil, invalidPolicyError{err}
	}
	return e, nil
}

// lockedPolicy takes LockOrg and returns the caller's role and enforcer as of that lock; guards deciding a write must use these, not request-start state.
func lockedPolicy(ctx context.Context, tx *store.Store, org uuid.UUID) (store.Role, *authz.Enforcer, error) {
	if err := tx.LockOrg(ctx, org); err != nil {
		return "", nil, err
	}
	m, err := tx.Membership(ctx, org, caller(ctx).User)
	if err != nil {
		return "", nil, err
	}
	e, err := policyEnforcer(ctx, tx)
	return m.Role, e, err
}

// auditDenied records a guard denial the handler made after authz let the call through; such denials never reach the interceptor.
func (s *server) auditDenied(ctx context.Context, e store.AuditEntry, err error) {
	e.Result = store.ResultDenied
	if err != nil {
		e.Details = map[string]any{"error": err.Error()}
	}
	if aerr := s.Store.Audit(ctx, e); aerr != nil {
		slog.Error("audit denied access", "action", e.Action, "err", aerr)
	}
}

// validName trims s and accepts 1–64 runes without control characters.
func validName(s string) (string, error) {
	s = strings.TrimSpace(s)
	n := utf8.RuneCountInString(s)
	if n < 1 || n > 64 || textx.Sanitize(s) != s {
		return "", connect.NewError(connect.CodeInvalidArgument, errors.New("invalid name"))
	}
	return s, nil
}

func validRole(r string) (store.Role, error) {
	switch role := store.Role(r); role {
	case store.RoleOwner, store.RoleAdmin, store.RoleMember:
		return role, nil
	}
	return "", connect.NewError(connect.CodeInvalidArgument, errors.New("invalid role"))
}

// errAdminRequired refuses a request without its admin flag, whose zero value would demote or revoke.
var errAdminRequired = connect.NewError(connect.CodeInvalidArgument, errors.New("admin is required"))

// privileged roles can only be granted or taken away by holders of iam.members.setRole (owners).
func privileged(role store.Role) bool { return role == store.RoleOwner || role == store.RoleAdmin }

// parseID reads a canonical UUID field the interceptor did not resolve.
func parseID(name, raw string) (uuid.UUID, error) {
	id, err := uuidx.Canonical(raw)
	if err != nil {
		return uuid.Nil, connect.NewError(connect.CodeInvalidArgument, errors.New(name+": invalid id"))
	}
	return id, nil
}

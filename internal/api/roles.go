package api

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/asterism-dev/observatory/internal/auth"
	"github.com/asterism-dev/observatory/internal/authz"
	"github.com/asterism-dev/observatory/internal/platform/textx"
	"github.com/asterism-dev/observatory/internal/store"
)

var errLiftsOwnDeny = errors.New("only an org owner can lift a deny rule that applies to you")

const maxRoleRules = 200

// denyRuleError names the deny rule behind a P6/P9 denial; it unwraps to the sentinel.
type denyRuleError struct {
	err  error
	rule authz.Rule
}

func (e denyRuleError) Error() string {
	return e.err.Error() + ": " + string(e.rule.Effect) + " " + e.rule.Action + "@" + e.rule.Object
}

func (e denyRuleError) Unwrap() error { return e.err }

type ruleJSON struct {
	Effect authz.Effect `json:"effect"`
	Object string       `json:"object"`
	Action string       `json:"action"`
}

func toRulesJSON(rules []authz.Rule) []ruleJSON {
	out := make([]ruleJSON, 0, len(rules))
	for _, r := range rules {
		out = append(out, ruleJSON(r))
	}
	return out
}

type roleJSON struct {
	ID          uuid.UUID  `json:"id"`
	Name        string     `json:"name"`
	Description string     `json:"description"`
	Rules       []ruleJSON `json:"rules"`
	Parents     []string   `json:"parents"`
}

func toRoleJSON(r authz.CustomRole) roleJSON {
	parents := r.Parents
	if parents == nil {
		parents = []string{}
	}
	return roleJSON{r.ID, r.Name, r.Description, toRulesJSON(r.Rules), parents}
}

type bindingJSON struct {
	ID        uuid.UUID `json:"id"`
	Subject   string    `json:"subject"`
	RoleID    uuid.UUID `json:"roleId"`
	CreatedAt time.Time `json:"createdAt"`
}

func (h *api) catalog(w http.ResponseWriter, r *http.Request) {
	m, ok := h.orgFromPath(w, r)
	if !ok || !h.authorize(w, r, m.OrgID, authz.ObjRoles, "iam.roles.list") {
		return
	}
	type permJSON struct {
		Action string `json:"action"`
		Object string `json:"object"`
	}
	out := make([]permJSON, 0, len(authz.Catalog))
	for _, p := range authz.Catalog {
		out = append(out, permJSON(p))
	}
	writeJSON(w, http.StatusOK, out)
}

func (h *api) listRoles(w http.ResponseWriter, r *http.Request) {
	m, ok := h.orgFromPath(w, r)
	if !ok || !h.authorize(w, r, m.OrgID, authz.ObjRoles, "iam.roles.list") {
		return
	}
	roles, err := h.store.Roles(r.Context(), m.OrgID)
	if err != nil {
		serverError(w, "roles", err)
		return
	}
	out := []any{}
	for _, key := range []string{"owner", "admin", "member"} {
		out = append(out, map[string]any{"key": key, "name": strings.ToUpper(key[:1]) + key[1:], "builtin": true, "rules": toRulesJSON(authz.BuiltinRules(key))})
	}
	for _, cr := range roles {
		out = append(out, toRoleJSON(cr))
	}
	writeJSON(w, http.StatusOK, out)
}

func readRoleBody(w http.ResponseWriter, r *http.Request) (authz.CustomRole, bool) {
	var body struct {
		Name, Description string
		Rules             []ruleJSON
		Parents           []string
	}
	if !readJSON(w, r, &body) {
		return authz.CustomRole{}, false
	}
	name, ok := validName(body.Name)
	if !ok {
		writeError(w, http.StatusBadRequest, "invalid name")
		return authz.CustomRole{}, false
	}
	if textx.Sanitize(body.Description) != body.Description {
		writeError(w, http.StatusBadRequest, "invalid description")
		return authz.CustomRole{}, false
	}
	if len(body.Rules) > maxRoleRules {
		writeError(w, http.StatusBadRequest, "too many rules")
		return authz.CustomRole{}, false
	}
	rules := make([]authz.Rule, 0, len(body.Rules))
	for _, rj := range body.Rules {
		rules = append(rules, authz.Rule(rj))
	}
	return authz.CustomRole{Name: name, Description: body.Description, Rules: rules, Parents: body.Parents}, true
}

// expandRules stores allow wildcards as the concrete catalog actions they match today, so later catalog additions aren't granted silently; deny wildcards stay as written so they keep covering new actions.
func expandRules(rules []authz.Rule) []authz.Rule {
	out := []authz.Rule{}
	for _, r := range rules {
		actions := []string{r.Action}
		if r.Effect == authz.Allow {
			actions = authz.ExpandActions(r.Action)
		}
		for _, a := range actions {
			if x := (authz.Rule{Effect: r.Effect, Object: r.Object, Action: a}); !slices.Contains(out, x) {
				out = append(out, x)
			}
		}
	}
	return out
}

func (h *api) createRole(w http.ResponseWriter, r *http.Request) {
	m, ok := h.orgFromPath(w, r)
	if !ok || !h.authorize(w, r, m.OrgID, authz.ObjRoles, "iam.roles.manage") {
		return
	}
	role, ok := readRoleBody(w, r)
	if !ok {
		return
	}
	p, _ := auth.PrincipalFrom(r.Context())
	actor := authz.UserSubject(p.User.ID)
	var denied error
	if !h.commit(w, r, func(tx *store.Store) error {
		cur, e, err := lockedPolicy(r, tx, m.OrgID)
		if err != nil {
			return err
		}
		if denied = e.CanGrantRole(actor, m.OrgID, privileged(cur.Role), role.Rules, role.Parents); denied != nil {
			return denied
		}
		role.Rules = expandRules(role.Rules)
		if role, err = tx.CreateRole(r.Context(), m.OrgID, role); err != nil {
			return err
		}
		if denied = denyFreeUnlessPrivileged(r, tx, cur, role.ID); denied != nil {
			return denied
		}
		ae := auditEntry(r, &m.OrgID, store.ActionRoleCreate, authz.RoleObject(role.ID), store.ResultAllowed)
		ae.Details = map[string]any{"name": role.Name}
		return tx.Audit(r.Context(), ae)
	}) {
		h.auditGuardDenial(r, m.OrgID, store.ActionRoleCreate, authz.ObjRoles, denied)
		return
	}
	h.fleet.NotifyOrg(m.OrgID)
	writeJSON(w, http.StatusCreated, toRoleJSON(role))
}

func (h *api) updateRole(w http.ResponseWriter, r *http.Request) {
	m, ok := h.orgFromPath(w, r)
	if !ok || !h.authorize(w, r, m.OrgID, authz.ObjRoles, "iam.roles.manage") {
		return
	}
	id, ok := pathID(w, r, "role")
	if !ok {
		return
	}
	role, ok := readRoleBody(w, r)
	if !ok {
		return
	}
	role.ID = id
	p, _ := auth.PrincipalFrom(r.Context())
	actor, obj := authz.UserSubject(p.User.ID), authz.RoleObject(id)
	var denied error
	if !h.commit(w, r, func(tx *store.Store) error {
		cur, e, err := lockedPolicy(r, tx, m.OrgID)
		if err != nil {
			return err
		}
		if denied = e.CanGrantRole(actor, m.OrgID, privileged(cur.Role), role.Rules, role.Parents); denied != nil {
			return denied
		}
		if !privileged(cur.Role) {
			if denied = carriesDeny(e, m.OrgID, id); denied != nil {
				return denied
			}
		}
		role.Rules = expandRules(role.Rules)
		if denied = guardOwnDenies(r.Context(), tx, cur, actor, func() error { return tx.UpdateRole(r.Context(), m.OrgID, role) }); denied != nil {
			return denied
		}
		if denied = denyFreeUnlessPrivileged(r, tx, cur, id); denied != nil {
			return denied
		}
		ae := auditEntry(r, &m.OrgID, store.ActionRoleUpdate, obj, store.ResultAllowed)
		ae.Details = map[string]any{"name": role.Name}
		return tx.Audit(r.Context(), ae)
	}) {
		h.auditGuardDenial(r, m.OrgID, store.ActionRoleUpdate, obj, denied)
		return
	}
	h.fleet.NotifyOrg(m.OrgID)
	w.WriteHeader(http.StatusNoContent)
}

func (h *api) deleteRole(w http.ResponseWriter, r *http.Request) {
	m, ok := h.orgFromPath(w, r)
	if !ok || !h.authorize(w, r, m.OrgID, authz.ObjRoles, "iam.roles.manage") {
		return
	}
	id, ok := pathID(w, r, "role")
	if !ok {
		return
	}
	p, _ := auth.PrincipalFrom(r.Context())
	actor, obj := authz.UserSubject(p.User.ID), authz.RoleObject(id)
	var denied error
	if !h.commit(w, r, func(tx *store.Store) error {
		cur, e, err := lockedPolicy(r, tx, m.OrgID)
		if err != nil {
			return err
		}
		if !privileged(cur.Role) {
			if denied = carriesDeny(e, m.OrgID, id); denied != nil {
				return denied
			}
		}
		if denied = guardOwnDenies(r.Context(), tx, cur, actor, func() error { return tx.DeleteRole(r.Context(), m.OrgID, id) }); denied != nil {
			return denied
		}
		return tx.Audit(r.Context(), auditEntry(r, &m.OrgID, store.ActionRoleDelete, obj, store.ResultAllowed))
	}) {
		h.auditGuardDenial(r, m.OrgID, store.ActionRoleDelete, obj, denied)
		return
	}
	h.fleet.NotifyOrg(m.OrgID)
	w.WriteHeader(http.StatusNoContent)
}

func (h *api) listBindings(w http.ResponseWriter, r *http.Request) {
	m, ok := h.orgFromPath(w, r)
	if !ok || !h.authorize(w, r, m.OrgID, authz.ObjRoles, "iam.roleBindings.manage") {
		return
	}
	bs, err := h.store.Bindings(r.Context(), m.OrgID)
	if err != nil {
		serverError(w, "bindings", err)
		return
	}
	out := make([]bindingJSON, 0, len(bs))
	for _, b := range bs {
		out = append(out, bindingJSON(b))
	}
	writeJSON(w, http.StatusOK, out)
}

func (h *api) bind(w http.ResponseWriter, r *http.Request) {
	m, ok := h.orgFromPath(w, r)
	if !ok || !h.authorize(w, r, m.OrgID, authz.ObjRoles, "iam.roleBindings.manage") {
		return
	}
	var body struct {
		Subject string    `json:"subject"`
		RoleID  uuid.UUID `json:"roleId"`
	}
	if !readJSON(w, r, &body) {
		return
	}
	p, _ := auth.PrincipalFrom(r.Context())
	obj := authz.RoleObject(body.RoleID)
	var (
		b      store.BindingRow
		denied error
	)
	if !h.commit(w, r, func(tx *store.Store) error {
		cur, e, err := lockedPolicy(r, tx, m.OrgID)
		if err != nil {
			return err
		}
		// Bindings only reference custom roles; the guard rejects anything else as an invalid parent.
		if denied = e.CanGrantRole(authz.UserSubject(p.User.ID), m.OrgID, privileged(cur.Role), nil, []string{body.RoleID.String()}); denied != nil {
			return denied
		}
		if !privileged(cur.Role) {
			if denied = carriesDeny(e, m.OrgID, body.RoleID); denied != nil {
				return denied
			}
		}
		if b, err = tx.Bind(r.Context(), m.OrgID, body.Subject, body.RoleID); err != nil {
			return err
		}
		ae := auditEntry(r, &m.OrgID, store.ActionRoleBind, obj, store.ResultAllowed)
		ae.Details = map[string]any{"subject": b.Subject, "binding": b.ID.String()}
		return tx.Audit(r.Context(), ae)
	}) {
		h.auditGuardDenial(r, m.OrgID, store.ActionRoleBind, obj, denied)
		return
	}
	h.fleet.NotifyOrg(m.OrgID)
	writeJSON(w, http.StatusCreated, bindingJSON(b))
}

func (h *api) unbind(w http.ResponseWriter, r *http.Request) {
	m, ok := h.orgFromPath(w, r)
	if !ok || !h.authorize(w, r, m.OrgID, authz.ObjRoles, "iam.roleBindings.manage") {
		return
	}
	id, ok := pathID(w, r, "id")
	if !ok {
		return
	}
	p, _ := auth.PrincipalFrom(r.Context())
	actor := authz.UserSubject(p.User.ID)
	var (
		b      store.BindingRow
		denied error
	)
	if !h.commit(w, r, func(tx *store.Store) error {
		cur, e, err := lockedPolicy(r, tx, m.OrgID)
		if err != nil {
			return err
		}
		denied = guardOwnDenies(r.Context(), tx, cur, actor, func() error {
			var err error
			if b, err = tx.Unbind(r.Context(), m.OrgID, id); err != nil {
				return err
			}
			if !privileged(cur.Role) {
				return carriesDeny(e, m.OrgID, b.RoleID)
			}
			return nil
		})
		if denied != nil {
			return denied
		}
		ae := auditEntry(r, &m.OrgID, store.ActionRoleUnbind, authz.RoleObject(b.RoleID), store.ResultAllowed)
		ae.Details = map[string]any{"subject": b.Subject, "binding": b.ID.String()}
		return tx.Audit(r.Context(), ae)
	}) {
		h.auditGuardDenial(r, m.OrgID, store.ActionRoleUnbind, authz.RoleObject(b.RoleID), denied)
		return
	}
	h.fleet.NotifyOrg(m.OrgID)
	w.WriteHeader(http.StatusNoContent)
}

// carriesDeny fails with ErrDenyNotAllowed if role or its ancestors hold deny rules: only org owners and admins may change or assign those.
func carriesDeny(e *authz.Enforcer, org, role uuid.UUID) error {
	denies, err := e.DenyRulesFor(authz.RoleSubject(role.String()), org)
	if err != nil {
		return err
	}
	if len(denies) > 0 {
		return denyRuleError{authz.ErrDenyNotAllowed, denies[0]}
	}
	return nil
}

// guardOwnDenies runs change in tx and fails with errLiftsOwnDeny if it drops a deny rule applying to the caller; owners are exempt. Take LockOrg first.
func guardOwnDenies(ctx context.Context, tx *store.Store, m orgMember, actor string, change func() error) error {
	if m.Role == store.RoleOwner {
		return change()
	}
	had, err := ownDenies(ctx, tx, m.OrgID, actor)
	if err != nil {
		return err
	}
	if err := change(); err != nil || len(had) == 0 {
		return err
	}
	has, err := ownDenies(ctx, tx, m.OrgID, actor)
	if err != nil {
		return err
	}
	for _, d := range had {
		if !slices.Contains(has, d) {
			return denyRuleError{errLiftsOwnDeny, d}
		}
	}
	return nil
}

// ownDenies returns the deny rules applying to actor under tx's current policy.
func ownDenies(ctx context.Context, tx *store.Store, org uuid.UUID, actor string) ([]authz.Rule, error) {
	e, err := policyEnforcer(ctx, tx)
	if err != nil {
		return nil, err
	}
	return e.DenyRulesFor(actor, org)
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

// lockedPolicy takes LockOrg and returns the caller's membership and enforcer as of that lock; guards deciding a write must use these, not request-start state.
func lockedPolicy(r *http.Request, tx *store.Store, org uuid.UUID) (orgMember, *authz.Enforcer, error) {
	ctx := r.Context()
	if err := tx.LockOrg(ctx, org); err != nil {
		return orgMember{}, nil, err
	}
	p, _ := auth.PrincipalFrom(ctx)
	m, err := tx.Membership(ctx, org, p.User.ID)
	if err != nil {
		return orgMember{}, nil, err
	}
	e, err := policyEnforcer(ctx, tx)
	return orgMember{org, m.Role}, e, err
}

// denyFreeUnlessPrivileged checks role after a change in tx: a deny-carrying parent writes denies too, which only owners and admins may.
func denyFreeUnlessPrivileged(r *http.Request, tx *store.Store, m orgMember, role uuid.UUID) error {
	if privileged(m.Role) {
		return nil
	}
	e, err := policyEnforcer(r.Context(), tx)
	if err != nil {
		return err
	}
	return carriesDeny(e, m.OrgID, role)
}

func isGuardDenial(err error) bool {
	return errors.Is(err, authz.ErrEscalation) || errors.Is(err, authz.ErrDenyNotAllowed) || errors.Is(err, errLiftsOwnDeny)
}

// auditGuardDenial writes a denied audit row naming why, if err is a guard denial.
func (h *api) auditGuardDenial(r *http.Request, org uuid.UUID, act, obj string, err error) {
	if !isGuardDenial(err) {
		return
	}
	e := auditEntry(r, &org, act, obj, store.ResultDenied)
	e.Details = map[string]any{"error": err.Error()}
	if aerr := h.store.Audit(r.Context(), e); aerr != nil {
		slog.Error("audit denied guard", "org", org, "action", act, "err", aerr)
	}
}

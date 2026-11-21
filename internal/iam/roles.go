package iam

import (
	"context"
	"errors"
	"net/http"
	"slices"
	"strings"

	"connectrpc.com/connect"
	"github.com/google/uuid"
	"google.golang.org/protobuf/types/known/timestamppb"

	iamv1 "github.com/asterism-dev/observatory/gen/go/observatory/iam/v1"
	"github.com/asterism-dev/observatory/internal/authz"
	"github.com/asterism-dev/observatory/internal/platform/textx"
	"github.com/asterism-dev/observatory/internal/store"
)

var errLiftsOwnDeny = errors.New("only an org owner can lift a deny rule that applies to you")

const maxRoleRules = 200

func toRoleRules(rules []authz.Rule) []*iamv1.RoleRule {
	out := make([]*iamv1.RoleRule, 0, len(rules))
	for _, r := range rules {
		out = append(out, &iamv1.RoleRule{Effect: string(r.Effect), Object: r.Object, Action: r.Action})
	}
	return out
}

func toRole(r authz.CustomRole) *iamv1.Role {
	return &iamv1.Role{Id: r.ID.String(), Name: r.Name, Description: r.Description, Rules: toRoleRules(r.Rules), Parents: r.Parents}
}

func toRoleBinding(b store.BindingRow) *iamv1.RoleBinding {
	return &iamv1.RoleBinding{Id: b.ID.String(), Subject: b.Subject, RoleId: b.RoleID.String(), CreatedAt: timestamppb.New(b.CreatedAt)}
}

func (s *server) ListCatalog(_ context.Context, req *connect.Request[iamv1.ListCatalogRequest]) (*connect.Response[iamv1.ListCatalogResponse], error) {
	if _, err := parseID("org", req.Msg.Org); err != nil {
		return nil, err
	}
	out := make([]*iamv1.CatalogEntry, 0, len(authz.Catalog))
	for _, p := range authz.Catalog {
		out = append(out, &iamv1.CatalogEntry{Action: p.Action, Object: p.Object})
	}
	return connect.NewResponse(&iamv1.ListCatalogResponse{Entries: out}), nil
}

func (s *server) ListRoles(ctx context.Context, req *connect.Request[iamv1.ListRolesRequest]) (*connect.Response[iamv1.ListRolesResponse], error) {
	org, err := parseID("org", req.Msg.Org)
	if err != nil {
		return nil, err
	}
	roles, err := s.Store.Roles(ctx, org)
	if err != nil {
		return nil, internal("roles", err)
	}
	out := make([]*iamv1.Role, 0, 3+len(roles))
	for _, key := range []string{"owner", "admin", "member"} {
		out = append(out, &iamv1.Role{Key: key, Name: strings.ToUpper(key[:1]) + key[1:], Builtin: true, Rules: toRoleRules(authz.BuiltinRules(key))})
	}
	for _, cr := range roles {
		out = append(out, toRole(cr))
	}
	return connect.NewResponse(&iamv1.ListRolesResponse{Roles: out}), nil
}

func roleBody(name, description string, rules []*iamv1.RoleRule, parents []string) (authz.CustomRole, error) {
	name, err := validName(name)
	if err != nil {
		return authz.CustomRole{}, err
	}
	if textx.Sanitize(description) != description {
		return authz.CustomRole{}, connect.NewError(connect.CodeInvalidArgument, errors.New("invalid description"))
	}
	if len(rules) > maxRoleRules {
		return authz.CustomRole{}, connect.NewError(connect.CodeInvalidArgument, errors.New("too many rules"))
	}
	rs := make([]authz.Rule, 0, len(rules))
	for _, r := range rules {
		rs = append(rs, authz.Rule{Effect: authz.Effect(r.Effect), Object: r.Object, Action: r.Action})
	}
	return authz.CustomRole{Name: name, Description: description, Rules: rs, Parents: parents}, nil
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

func (s *server) CreateRole(ctx context.Context, req *connect.Request[iamv1.CreateRoleRequest]) (*connect.Response[iamv1.CreateRoleResponse], error) {
	org, err := parseID("org", req.Msg.Org)
	if err != nil {
		return nil, err
	}
	role, err := roleBody(req.Msg.Name, req.Msg.Description, req.Msg.Rules, req.Msg.Parents)
	if err != nil {
		return nil, err
	}
	actor := authz.UserSubject(caller(ctx).User)
	var denied error
	if err := s.commit(ctx, func(tx *store.Store) error {
		cur, e, err := lockedPolicy(ctx, tx, org)
		if err != nil {
			return err
		}
		if denied = e.CanGrantRole(actor, org, privileged(cur), role.Rules, role.Parents); denied != nil {
			return denied
		}
		role.Rules = expandRules(role.Rules)
		if role, err = tx.CreateRole(ctx, org, role); err != nil {
			return err
		}
		if denied = denyFreeUnlessPrivileged(ctx, tx, org, cur, role.ID); denied != nil {
			return denied
		}
		ae := auditEntry(ctx, req.Header(), &org, store.ActionRoleCreate, authz.RoleObject(role.ID), store.ResultAllowed)
		ae.Details = map[string]any{"name": role.Name}
		return tx.Audit(ctx, ae)
	}); err != nil {
		s.auditGuardDenial(ctx, req.Header(), org, store.ActionRoleCreate, authz.ObjRoles, denied)
		return nil, err
	}
	s.Notify(org)
	return connect.NewResponse(&iamv1.CreateRoleResponse{Role: toRole(role)}), nil
}

func (s *server) UpdateRole(ctx context.Context, req *connect.Request[iamv1.UpdateRoleRequest]) (*connect.Response[iamv1.UpdateRoleResponse], error) {
	org, err := parseID("org", req.Msg.Org)
	if err != nil {
		return nil, err
	}
	id, err := parseID("role", req.Msg.Role)
	if err != nil {
		return nil, err
	}
	role, err := roleBody(req.Msg.Name, req.Msg.Description, req.Msg.Rules, req.Msg.Parents)
	if err != nil {
		return nil, err
	}
	role.ID = id
	actor, obj := authz.UserSubject(caller(ctx).User), authz.RoleObject(id)
	var denied error
	if err := s.commit(ctx, func(tx *store.Store) error {
		cur, e, err := lockedPolicy(ctx, tx, org)
		if err != nil {
			return err
		}
		if denied = e.CanGrantRole(actor, org, privileged(cur), role.Rules, role.Parents); denied != nil {
			return denied
		}
		if !privileged(cur) {
			if denied = carriesDeny(e, org, id); denied != nil {
				return denied
			}
		}
		role.Rules = expandRules(role.Rules)
		if denied = guardOwnDenies(ctx, tx, org, cur, actor, func() error { return tx.UpdateRole(ctx, org, role) }); denied != nil {
			return denied
		}
		if denied = denyFreeUnlessPrivileged(ctx, tx, org, cur, id); denied != nil {
			return denied
		}
		ae := auditEntry(ctx, req.Header(), &org, store.ActionRoleUpdate, obj, store.ResultAllowed)
		ae.Details = map[string]any{"name": role.Name}
		return tx.Audit(ctx, ae)
	}); err != nil {
		s.auditGuardDenial(ctx, req.Header(), org, store.ActionRoleUpdate, obj, denied)
		return nil, err
	}
	s.Notify(org)
	return connect.NewResponse(&iamv1.UpdateRoleResponse{Role: toRole(role)}), nil
}

func (s *server) DeleteRole(ctx context.Context, req *connect.Request[iamv1.DeleteRoleRequest]) (*connect.Response[iamv1.DeleteRoleResponse], error) {
	org, err := parseID("org", req.Msg.Org)
	if err != nil {
		return nil, err
	}
	id, err := parseID("role", req.Msg.Role)
	if err != nil {
		return nil, err
	}
	actor, obj := authz.UserSubject(caller(ctx).User), authz.RoleObject(id)
	var denied error
	if err := s.commit(ctx, func(tx *store.Store) error {
		cur, e, err := lockedPolicy(ctx, tx, org)
		if err != nil {
			return err
		}
		if !privileged(cur) {
			if denied = carriesDeny(e, org, id); denied != nil {
				return denied
			}
		}
		if denied = guardOwnDenies(ctx, tx, org, cur, actor, func() error { return tx.DeleteRole(ctx, org, id) }); denied != nil {
			return denied
		}
		return tx.Audit(ctx, auditEntry(ctx, req.Header(), &org, store.ActionRoleDelete, obj, store.ResultAllowed))
	}); err != nil {
		s.auditGuardDenial(ctx, req.Header(), org, store.ActionRoleDelete, obj, denied)
		return nil, err
	}
	s.Notify(org)
	return connect.NewResponse(&iamv1.DeleteRoleResponse{}), nil
}

func (s *server) ListRoleBindings(ctx context.Context, req *connect.Request[iamv1.ListRoleBindingsRequest]) (*connect.Response[iamv1.ListRoleBindingsResponse], error) {
	org, err := parseID("org", req.Msg.Org)
	if err != nil {
		return nil, err
	}
	bs, err := s.Store.Bindings(ctx, org)
	if err != nil {
		return nil, internal("bindings", err)
	}
	out := make([]*iamv1.RoleBinding, 0, len(bs))
	for _, b := range bs {
		out = append(out, toRoleBinding(b))
	}
	return connect.NewResponse(&iamv1.ListRoleBindingsResponse{Bindings: out}), nil
}

func (s *server) CreateRoleBinding(ctx context.Context, req *connect.Request[iamv1.CreateRoleBindingRequest]) (*connect.Response[iamv1.CreateRoleBindingResponse], error) {
	org, err := parseID("org", req.Msg.Org)
	if err != nil {
		return nil, err
	}
	roleID, err := parseID("role_id", req.Msg.RoleId)
	if err != nil {
		return nil, err
	}
	actor, obj := authz.UserSubject(caller(ctx).User), authz.RoleObject(roleID)
	var (
		b      store.BindingRow
		denied error
	)
	if err := s.commit(ctx, func(tx *store.Store) error {
		cur, e, err := lockedPolicy(ctx, tx, org)
		if err != nil {
			return err
		}
		// Bindings only reference custom roles; the guard rejects anything else as an invalid parent.
		if denied = e.CanGrantRole(actor, org, privileged(cur), nil, []string{roleID.String()}); denied != nil {
			return denied
		}
		if !privileged(cur) {
			if denied = carriesDeny(e, org, roleID); denied != nil {
				return denied
			}
		}
		if b, err = tx.Bind(ctx, org, req.Msg.Subject, roleID); err != nil {
			return err
		}
		ae := auditEntry(ctx, req.Header(), &org, store.ActionRoleBind, obj, store.ResultAllowed)
		ae.Details = map[string]any{"subject": b.Subject, "binding": b.ID.String()}
		return tx.Audit(ctx, ae)
	}); err != nil {
		s.auditGuardDenial(ctx, req.Header(), org, store.ActionRoleBind, obj, denied)
		return nil, err
	}
	s.Notify(org)
	return connect.NewResponse(&iamv1.CreateRoleBindingResponse{Binding: toRoleBinding(b)}), nil
}

func (s *server) DeleteRoleBinding(ctx context.Context, req *connect.Request[iamv1.DeleteRoleBindingRequest]) (*connect.Response[iamv1.DeleteRoleBindingResponse], error) {
	org, err := parseID("org", req.Msg.Org)
	if err != nil {
		return nil, err
	}
	id, err := parseID("binding", req.Msg.Binding)
	if err != nil {
		return nil, err
	}
	actor := authz.UserSubject(caller(ctx).User)
	var (
		b      store.BindingRow
		denied error
	)
	if err := s.commit(ctx, func(tx *store.Store) error {
		cur, e, err := lockedPolicy(ctx, tx, org)
		if err != nil {
			return err
		}
		denied = guardOwnDenies(ctx, tx, org, cur, actor, func() error {
			var err error
			if b, err = tx.Unbind(ctx, org, id); err != nil {
				return err
			}
			if !privileged(cur) {
				return carriesDeny(e, org, b.RoleID)
			}
			return nil
		})
		if denied != nil {
			return denied
		}
		ae := auditEntry(ctx, req.Header(), &org, store.ActionRoleUnbind, authz.RoleObject(b.RoleID), store.ResultAllowed)
		ae.Details = map[string]any{"subject": b.Subject, "binding": b.ID.String()}
		return tx.Audit(ctx, ae)
	}); err != nil {
		s.auditGuardDenial(ctx, req.Header(), org, store.ActionRoleUnbind, authz.RoleObject(b.RoleID), denied)
		return nil, err
	}
	s.Notify(org)
	return connect.NewResponse(&iamv1.DeleteRoleBindingResponse{}), nil
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
func guardOwnDenies(ctx context.Context, tx *store.Store, org uuid.UUID, role store.Role, actor string, change func() error) error {
	if role == store.RoleOwner {
		return change()
	}
	had, err := ownDenies(ctx, tx, org, actor)
	if err != nil {
		return err
	}
	if err := change(); err != nil || len(had) == 0 {
		return err
	}
	has, err := ownDenies(ctx, tx, org, actor)
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

// denyFreeUnlessPrivileged checks role after a change in tx: a deny-carrying parent writes denies too, which only owners and admins may.
func denyFreeUnlessPrivileged(ctx context.Context, tx *store.Store, org uuid.UUID, cur store.Role, role uuid.UUID) error {
	if privileged(cur) {
		return nil
	}
	e, err := policyEnforcer(ctx, tx)
	if err != nil {
		return err
	}
	return carriesDeny(e, org, role)
}

func isGuardDenial(err error) bool {
	return errors.Is(err, authz.ErrEscalation) || errors.Is(err, authz.ErrDenyNotAllowed) || errors.Is(err, errLiftsOwnDeny)
}

// auditGuardDenial writes a denied event naming why, if err is a guard denial.
func (s *server) auditGuardDenial(ctx context.Context, h http.Header, org uuid.UUID, act, obj string, err error) {
	if isGuardDenial(err) {
		s.auditDenied(ctx, auditEntry(ctx, h, &org, act, obj, store.ResultDenied), err)
	}
}

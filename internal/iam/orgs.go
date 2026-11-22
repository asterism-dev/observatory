package iam

import (
	"context"
	"errors"
	"slices"

	"connectrpc.com/connect"
	"github.com/google/uuid"

	iamv1 "github.com/asterism-dev/observatory/gen/go/observatory/iam/v1"
	"github.com/asterism-dev/observatory/internal/authz"
	"github.com/asterism-dev/observatory/internal/iam/store"
)

// callerRole is the caller's role in org from the current policy; the interceptor already checked membership against it.
func (s *server) callerRole(ctx context.Context, org uuid.UUID) (store.Role, error) {
	role, ok := s.Policy.Enforcer().Role(org, caller(ctx).User)
	if !ok {
		return "", connect.NewError(connect.CodeNotFound, errors.New("not found"))
	}
	return store.Role(role), nil
}

func (s *server) CreateOrg(ctx context.Context, req *connect.Request[iamv1.CreateOrgRequest]) (*connect.Response[iamv1.CreateOrgResponse], error) {
	name, err := validName(req.Msg.Name)
	if err != nil {
		return nil, err
	}
	var org store.Org
	if err := s.commit(ctx, func(tx *store.Store) error {
		var err error
		if org, err = tx.CreateOrg(ctx, name, caller(ctx).User); err != nil {
			return err
		}
		return tx.Audit(ctx, auditEntry(ctx, req.Header(), &org.ID, store.ActionOrgCreate, "org/"+org.ID.String(), store.ResultAllowed))
	}); err != nil {
		return nil, err
	}
	return connect.NewResponse(&iamv1.CreateOrgResponse{Org: toOrgInfo(org, store.RoleOwner)}), nil
}

func (s *server) GetOrg(ctx context.Context, req *connect.Request[iamv1.GetOrgRequest]) (*connect.Response[iamv1.GetOrgResponse], error) {
	id, err := parseID("org", req.Msg.Org)
	if err != nil {
		return nil, err
	}
	e, sub := s.Policy.Enforcer(), authz.UserSubject(caller(ctx).User)
	role, ok := e.Role(id, caller(ctx).User)
	if !ok {
		return nil, connect.NewError(connect.CodeNotFound, errors.New("not found"))
	}
	perms := []string{}
	for _, c := range authz.Catalog {
		// Objects like org/teams/* are checked literally: allowed only if the caller's rules cover every team.
		if e.Allowed(sub, id, c.Object, c.Action) {
			perms = append(perms, c.Action)
		}
	}
	slices.Sort(perms)
	org, err := s.Store.Org(ctx, id)
	if err != nil {
		return nil, toConnect("org", err)
	}
	return connect.NewResponse(&iamv1.GetOrgResponse{Org: toOrgInfo(org, store.Role(role)), Permissions: slices.Compact(perms)}), nil
}

func (s *server) UpdateOrg(ctx context.Context, req *connect.Request[iamv1.UpdateOrgRequest]) (*connect.Response[iamv1.UpdateOrgResponse], error) {
	id, err := parseID("org", req.Msg.Org)
	if err != nil {
		return nil, err
	}
	role, err := s.callerRole(ctx, id)
	if err != nil {
		return nil, err
	}
	name, err := validName(req.Msg.Name)
	if err != nil {
		return nil, err
	}
	if err := s.commit(ctx, func(tx *store.Store) error {
		if err := tx.RenameOrg(ctx, id, name); err != nil {
			return err
		}
		e := auditEntry(ctx, req.Header(), &id, store.ActionOrgUpdate, "org/"+id.String(), store.ResultAllowed)
		e.Details = map[string]any{"name": name}
		return tx.Audit(ctx, e)
	}); err != nil {
		return nil, err
	}
	org, err := s.Store.Org(ctx, id)
	if err != nil {
		return nil, internal("org", err)
	}
	return connect.NewResponse(&iamv1.UpdateOrgResponse{Org: toOrgInfo(org, role)}), nil
}

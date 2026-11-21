package iam

import (
	"context"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/types/known/timestamppb"

	iamv1 "github.com/asterism-dev/observatory/gen/go/observatory/iam/v1"
	"github.com/asterism-dev/observatory/internal/store"
)

func (s *server) ListAllOrgs(ctx context.Context, _ *connect.Request[iamv1.ListAllOrgsRequest]) (*connect.Response[iamv1.ListAllOrgsResponse], error) {
	orgs, err := s.Store.Orgs(ctx)
	if err != nil {
		return nil, internal("orgs", err)
	}
	out := make([]*iamv1.OrgSummary, 0, len(orgs))
	for _, o := range orgs {
		out = append(out, &iamv1.OrgSummary{Id: o.Org.ID.String(), Name: o.Org.Name, Personal: o.Org.Personal, MemberCount: int32(o.MemberCount), CreatedAt: timestamppb.New(o.Org.CreatedAt)})
	}
	return connect.NewResponse(&iamv1.ListAllOrgsResponse{Orgs: out}), nil
}

func (s *server) GetAnyOrg(ctx context.Context, req *connect.Request[iamv1.GetAnyOrgRequest]) (*connect.Response[iamv1.GetAnyOrgResponse], error) {
	id, err := parseID("org_id", req.Msg.OrgId)
	if err != nil {
		return nil, err
	}
	org, err := s.Store.Org(ctx, id)
	if err != nil {
		return nil, toConnect("admin org", err)
	}
	members, err := s.Store.Members(ctx, id)
	if err != nil {
		return nil, internal("admin org members", err)
	}
	ms := make([]*iamv1.Member, 0, len(members))
	for _, m := range members {
		ms = append(ms, &iamv1.Member{Id: m.User.ID.String(), Email: m.User.Email, Name: m.User.Name, Role: string(m.Role), JoinedAt: timestamppb.New(m.JoinedAt)})
	}
	return connect.NewResponse(&iamv1.GetAnyOrgResponse{Org: toOrgInfo(org, ""), Members: ms}), nil
}

func (s *server) ListUsers(ctx context.Context, _ *connect.Request[iamv1.ListUsersRequest]) (*connect.Response[iamv1.ListUsersResponse], error) {
	users, err := s.Store.Users(ctx)
	if err != nil {
		return nil, internal("users", err)
	}
	out := make([]*iamv1.UserInfo, 0, len(users))
	for _, u := range users {
		out = append(out, &iamv1.UserInfo{Id: u.ID.String(), Email: u.Email, Name: u.Name, PlatformAdmin: u.PlatformAdmin, CreatedAt: timestamppb.New(u.CreatedAt)})
	}
	return connect.NewResponse(&iamv1.ListUsersResponse{Users: out}), nil
}

// SetPlatformAdmin audits as iam.platformAdmins.create or .delete, as 0.x did, not as the annotation's update.
func (s *server) SetPlatformAdmin(ctx context.Context, req *connect.Request[iamv1.SetPlatformAdminRequest]) (*connect.Response[iamv1.SetPlatformAdminResponse], error) {
	id, err := parseID("user_id", req.Msg.UserId)
	if err != nil {
		return nil, err
	}
	if req.Msg.Admin == nil {
		return nil, errAdminRequired
	}
	admin := *req.Msg.Admin
	action := store.ActionPlatformRevokeAdmin
	if admin {
		action = store.ActionPlatformGrantAdmin
	}
	if err := s.commit(ctx, func(tx *store.Store) error {
		if err := tx.SetPlatformAdmin(ctx, id, admin); err != nil {
			return err
		}
		return tx.Audit(ctx, auditEntry(ctx, req.Header(), nil, action, "user/"+id.String(), store.ResultAllowed))
	}); err != nil {
		return nil, err
	}
	return connect.NewResponse(&iamv1.SetPlatformAdminResponse{}), nil
}

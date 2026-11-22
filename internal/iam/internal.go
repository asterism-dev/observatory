package iam

import (
	"context"
	"errors"
	"fmt"

	"connectrpc.com/connect"
	"github.com/google/uuid"
	"google.golang.org/protobuf/types/known/timestamppb"

	iamv1 "github.com/asterism-dev/observatory/gen/go/observatory/iam/v1"
	"github.com/asterism-dev/observatory/gen/go/observatory/iam/v1/iamv1connect"
	"github.com/asterism-dev/observatory/internal/iam/store"
	"github.com/asterism-dev/observatory/internal/platform/iamclient"
)

// internalServer serves IAMInternalService to other services; the caller wraps it in servicetoken.Middleware.
type internalServer struct{ Store *store.Store }

var _ iamv1connect.IAMInternalServiceHandler = internalServer{}

func (s internalServer) GetUsers(ctx context.Context, req *connect.Request[iamv1.GetUsersRequest]) (*connect.Response[iamv1.GetUsersResponse], error) {
	ids, err := parseIDs(req.Msg.UserIds)
	if err != nil {
		return nil, err
	}
	us, err := s.Store.UsersByID(ctx, ids)
	if err != nil {
		return nil, internal("users", err)
	}
	out := &iamv1.GetUsersResponse{Users: make([]*iamv1.User, 0, len(us))}
	for _, u := range us {
		out.Users = append(out.Users, &iamv1.User{Id: u.ID.String(), Name: u.Name, Email: u.Email})
	}
	return connect.NewResponse(out), nil
}

func (s internalServer) ListSSHKeys(ctx context.Context, req *connect.Request[iamv1.ListSSHKeysRequest]) (*connect.Response[iamv1.ListSSHKeysResponse], error) {
	ids, err := parseIDs(req.Msg.UserIds)
	if err != nil {
		return nil, err
	}
	keys, err := s.Store.SSHKeysOf(ctx, ids)
	if err != nil {
		return nil, internal("ssh keys", err)
	}
	out := &iamv1.ListSSHKeysResponse{Keys: make([]*iamv1.SSHKey, 0, len(keys))}
	for _, k := range keys {
		out.Keys = append(out.Keys, &iamv1.SSHKey{UserId: k.UserID.String(), PublicKey: k.PublicKey})
	}
	return connect.NewResponse(out), nil
}

func (s internalServer) ListTeams(ctx context.Context, req *connect.Request[iamv1.ListTeamsRequest]) (*connect.Response[iamv1.ListTeamsResponse], error) {
	org, err := uuid.Parse(req.Msg.OrgId)
	if err != nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("invalid org_id"))
	}
	ts, err := s.Store.Teams(ctx, org)
	if err != nil {
		return nil, internal("teams", err)
	}
	out := &iamv1.ListTeamsResponse{Teams: make([]*iamv1.Team, 0, len(ts))}
	for _, t := range ts {
		out.Teams = append(out.Teams, &iamv1.Team{Id: t.ID.String(), Name: t.Name})
	}
	return connect.NewResponse(out), nil
}

func (s internalServer) GetMembership(ctx context.Context, req *connect.Request[iamv1.GetMembershipRequest]) (*connect.Response[iamv1.GetMembershipResponse], error) {
	org, err := uuid.Parse(req.Msg.OrgId)
	if err != nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("invalid org_id"))
	}
	user, err := uuid.Parse(req.Msg.UserId)
	if err != nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("invalid user_id"))
	}
	m, err := s.Store.Membership(ctx, org, user)
	if errors.Is(err, store.ErrNotFound) {
		return connect.NewResponse(&iamv1.GetMembershipResponse{}), nil
	}
	if err != nil {
		return nil, internal("membership", err)
	}
	return connect.NewResponse(&iamv1.GetMembershipResponse{Member: true, Role: string(m.Role)}), nil
}

func (s internalServer) GetSession(ctx context.Context, req *connect.Request[iamv1.GetSessionRequest]) (*connect.Response[iamv1.GetSessionResponse], error) {
	id, err := uuid.Parse(req.Msg.SessionId)
	if err != nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("invalid session_id"))
	}
	user, expires, revoked, err := s.Store.SessionState(ctx, id)
	if errors.Is(err, store.ErrNotFound) {
		return nil, connect.NewError(connect.CodeNotFound, errors.New("no such session"))
	}
	if err != nil {
		return nil, internal("session", err)
	}
	return connect.NewResponse(&iamv1.GetSessionResponse{UserId: user.String(), ExpiresAt: timestamppb.New(expires), Revoked: revoked}), nil
}

func parseIDs(raw []string) ([]uuid.UUID, error) {
	if len(raw) > iamclient.MaxIDs {
		return nil, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("at most %d user_ids", iamclient.MaxIDs))
	}
	ids := make([]uuid.UUID, len(raw))
	for i, r := range raw {
		id, err := uuid.Parse(r)
		if err != nil {
			return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("invalid user_id"))
		}
		ids[i] = id
	}
	return ids, nil
}

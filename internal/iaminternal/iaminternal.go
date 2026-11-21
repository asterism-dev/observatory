// Package iaminternal serves IAMInternalService from the hub's store and holds the lookups its callers share.
package iaminternal

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"

	"connectrpc.com/connect"
	"github.com/google/uuid"

	iamv1 "github.com/asterism-dev/observatory/gen/go/observatory/iam/v1"
	"github.com/asterism-dev/observatory/gen/go/observatory/iam/v1/iamv1connect"
	"github.com/asterism-dev/observatory/internal/store"
)

// MaxIDs caps the user IDs of one GetUsers or ListSSHKeys call.
const MaxIDs = 1000

type Server struct{ Store *store.Store }

var _ iamv1connect.IAMInternalServiceHandler = Server{}

func (s Server) GetUsers(ctx context.Context, req *connect.Request[iamv1.GetUsersRequest]) (*connect.Response[iamv1.GetUsersResponse], error) {
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

func (s Server) ListSSHKeys(ctx context.Context, req *connect.Request[iamv1.ListSSHKeysRequest]) (*connect.Response[iamv1.ListSSHKeysResponse], error) {
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

func (s Server) ListTeams(ctx context.Context, req *connect.Request[iamv1.ListTeamsRequest]) (*connect.Response[iamv1.ListTeamsResponse], error) {
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

func parseIDs(raw []string) ([]uuid.UUID, error) {
	if len(raw) > MaxIDs {
		return nil, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("at most %d user_ids", MaxIDs))
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

func internal(what string, err error) error {
	slog.Error("iam internal", "lookup", what, "err", err)
	return connect.NewError(connect.CodeInternal, errors.New("internal error"))
}

// ListTeams lists org's teams; only ID, OrgID and Name are set.
func ListTeams(ctx context.Context, c iamv1connect.IAMInternalServiceClient, org uuid.UUID) ([]store.Team, error) {
	res, err := c.ListTeams(ctx, connect.NewRequest(&iamv1.ListTeamsRequest{OrgId: org.String()}))
	if err != nil {
		return nil, err
	}
	out := make([]store.Team, 0, len(res.Msg.Teams))
	for _, t := range res.Msg.Teams {
		id, err := uuid.Parse(t.Id)
		if err != nil {
			return nil, fmt.Errorf("team id %q: %w", t.Id, err)
		}
		out = append(out, store.Team{ID: id, OrgID: org, Name: t.Name})
	}
	return out, nil
}

// UserNames maps ids to display names; unknown users are absent.
func UserNames(ctx context.Context, c iamv1connect.IAMInternalServiceClient, ids []uuid.UUID) (map[uuid.UUID]string, error) {
	names := make(map[uuid.UUID]string, len(ids))
	for chunk := range slices.Chunk(ids, MaxIDs) {
		res, err := c.GetUsers(ctx, connect.NewRequest(&iamv1.GetUsersRequest{UserIds: idStrings(chunk)}))
		if err != nil {
			return nil, err
		}
		for _, u := range res.Msg.Users {
			if id, err := uuid.Parse(u.Id); err == nil {
				names[id] = u.Name
			}
		}
	}
	return names, nil
}

// SSHKeys returns the distinct public keys of ids, sorted.
func SSHKeys(ctx context.Context, c iamv1connect.IAMInternalServiceClient, ids []uuid.UUID) ([]string, error) {
	keys := []string{}
	for chunk := range slices.Chunk(ids, MaxIDs) {
		res, err := c.ListSSHKeys(ctx, connect.NewRequest(&iamv1.ListSSHKeysRequest{UserIds: idStrings(chunk)}))
		if err != nil {
			return nil, err
		}
		for _, k := range res.Msg.Keys {
			keys = append(keys, k.PublicKey)
		}
	}
	slices.Sort(keys)
	return slices.Compact(keys), nil
}

func idStrings(ids []uuid.UUID) []string {
	out := make([]string, len(ids))
	for i, id := range ids {
		out[i] = id.String()
	}
	return out
}

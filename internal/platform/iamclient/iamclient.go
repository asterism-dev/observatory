// Package iamclient holds the lookups other services make against iam's IAMInternalService.
package iamclient

import (
	"context"
	"fmt"
	"slices"
	"time"

	"connectrpc.com/connect"
	"github.com/google/uuid"

	iamv1 "github.com/asterism-dev/observatory/gen/go/observatory/iam/v1"
	"github.com/asterism-dev/observatory/gen/go/observatory/iam/v1/iamv1connect"
)

// MaxIDs caps the user IDs of one GetUsers or ListSSHKeys call.
const MaxIDs = 1000

type Team struct {
	ID, OrgID uuid.UUID
	Name      string
}

// ListTeams lists org's teams; only ID, OrgID and Name are set.
func ListTeams(ctx context.Context, c iamv1connect.IAMInternalServiceClient, org uuid.UUID) ([]Team, error) {
	res, err := c.ListTeams(ctx, connect.NewRequest(&iamv1.ListTeamsRequest{OrgId: org.String()}))
	if err != nil {
		return nil, err
	}
	out := make([]Team, 0, len(res.Msg.Teams))
	for _, t := range res.Msg.Teams {
		id, err := uuid.Parse(t.Id)
		if err != nil {
			return nil, fmt.Errorf("team id %q: %w", t.Id, err)
		}
		out = append(out, Team{ID: id, OrgID: org, Name: t.Name})
	}
	return out, nil
}

// IsMember asks iam whether user is a member of org.
func IsMember(ctx context.Context, c iamv1connect.IAMInternalServiceClient, org, user uuid.UUID) (bool, error) {
	res, err := c.GetMembership(ctx, connect.NewRequest(&iamv1.GetMembershipRequest{OrgId: org.String(), UserId: user.String()}))
	if err != nil {
		return false, err
	}
	return res.Msg.Member, nil
}

// SessionActive asks iam whether a session is neither revoked nor expired; an error means iam could not answer, and callers must refuse.
func SessionActive(ctx context.Context, c iamv1connect.IAMInternalServiceClient, id uuid.UUID) (bool, error) {
	res, err := c.GetSession(ctx, connect.NewRequest(&iamv1.GetSessionRequest{SessionId: id.String()}))
	if connect.CodeOf(err) == connect.CodeNotFound {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return !res.Msg.Revoked && res.Msg.ExpiresAt.AsTime().After(time.Now()), nil
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

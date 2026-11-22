package api

import (
	"context"
	"sync/atomic"
	"testing"

	"connectrpc.com/connect"

	iamv1 "github.com/asterism-dev/observatory/gen/go/observatory/iam/v1"
	"github.com/asterism-dev/observatory/gen/go/observatory/iam/v1/iamv1connect"
)

// renamingIAM prefixes every user and team name the wrapped client returns and counts GetUsers calls.
type renamingIAM struct {
	iamv1connect.IAMInternalServiceClient
	getUsers atomic.Int32
}

func (f *renamingIAM) GetUsers(ctx context.Context, req *connect.Request[iamv1.GetUsersRequest]) (*connect.Response[iamv1.GetUsersResponse], error) {
	f.getUsers.Add(1)
	res, err := f.IAMInternalServiceClient.GetUsers(ctx, req)
	if err == nil {
		for _, u := range res.Msg.Users {
			u.Name = "iam " + u.Name
		}
	}
	return res, err
}

func (f *renamingIAM) ListTeams(ctx context.Context, req *connect.Request[iamv1.ListTeamsRequest]) (*connect.Response[iamv1.ListTeamsResponse], error) {
	res, err := f.IAMInternalServiceClient.ListTeams(ctx, req)
	if err == nil {
		for _, t := range res.Msg.Teams {
			t.Name = "iam " + t.Name
		}
	}
	return res, err
}

func TestVMListOwnerNamesViaIAM(t *testing.T) {
	e := newEnv(t)
	_, tok := e.user("alice")
	org := e.createOrg(tok, "Acme")
	team := e.createTeam(org, "core")
	node := e.enrolledNode(tok, org, "n1")
	e.createVM(tok, org, vmBody("u1", userOwner(), node))
	e.createVM(tok, org, vmBody("t1", teamOwner(team), node))

	fake := &renamingIAM{IAMInternalServiceClient: e.api.iam}
	e.api.iam = fake
	names := map[string]string{}
	for _, v := range e.listVMs(tok, org).VMs {
		names[v.Owner.Kind] = v.Owner.Name
	}
	if names["user"] != "iam alice" || names["team"] != "iam core" {
		t.Fatalf("owner names %v", names)
	}
	if n := fake.getUsers.Load(); n != 1 {
		t.Fatalf("GetUsers called %d times for one list", n)
	}
}

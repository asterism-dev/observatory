package iaminternal

import (
	"testing"

	"connectrpc.com/connect"
	"github.com/google/uuid"

	iamv1 "github.com/asterism-dev/observatory/gen/go/observatory/iam/v1"
)

func TestGetUsersLimit(t *testing.T) {
	ids := make([]string, MaxIDs+1)
	for i := range ids {
		ids[i] = uuid.NewString()
	}
	_, err := Server{}.GetUsers(t.Context(), connect.NewRequest(&iamv1.GetUsersRequest{UserIds: ids}))
	if connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Fatalf("GetUsers with %d ids: %v", len(ids), err)
	}
	_, err = Server{}.ListSSHKeys(t.Context(), connect.NewRequest(&iamv1.ListSSHKeysRequest{UserIds: ids}))
	if connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Fatalf("ListSSHKeys with %d ids: %v", len(ids), err)
	}
	_, err = Server{}.GetUsers(t.Context(), connect.NewRequest(&iamv1.GetUsersRequest{UserIds: []string{"nope"}}))
	if connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Fatalf("bad id: %v", err)
	}
}

func TestListTeamsInvalidOrg(t *testing.T) {
	_, err := Server{}.ListTeams(t.Context(), connect.NewRequest(&iamv1.ListTeamsRequest{OrgId: "nope"}))
	if connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Fatalf("ListTeams with a malformed org_id: %v", err)
	}
}

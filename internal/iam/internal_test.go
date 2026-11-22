package iam

import (
	"testing"
	"time"

	"connectrpc.com/connect"
	"github.com/google/uuid"

	iamv1 "github.com/asterism-dev/observatory/gen/go/observatory/iam/v1"
	"github.com/asterism-dev/observatory/internal/iam/store"
	"github.com/asterism-dev/observatory/internal/iam/store/storetest"
	"github.com/asterism-dev/observatory/internal/platform/iamclient"
)

func TestGetUsersLimit(t *testing.T) {
	ids := make([]string, iamclient.MaxIDs+1)
	for i := range ids {
		ids[i] = uuid.NewString()
	}
	_, err := internalServer{}.GetUsers(t.Context(), connect.NewRequest(&iamv1.GetUsersRequest{UserIds: ids}))
	if connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Fatalf("GetUsers with %d ids: %v", len(ids), err)
	}
	_, err = internalServer{}.ListSSHKeys(t.Context(), connect.NewRequest(&iamv1.ListSSHKeysRequest{UserIds: ids}))
	if connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Fatalf("ListSSHKeys with %d ids: %v", len(ids), err)
	}
	_, err = internalServer{}.GetUsers(t.Context(), connect.NewRequest(&iamv1.GetUsersRequest{UserIds: []string{"nope"}}))
	if connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Fatalf("bad id: %v", err)
	}
}

func TestListTeamsInvalidOrg(t *testing.T) {
	_, err := internalServer{}.ListTeams(t.Context(), connect.NewRequest(&iamv1.ListTeamsRequest{OrgId: "nope"}))
	if connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Fatalf("ListTeams with a malformed org_id: %v", err)
	}
}

func TestGetSessionUnknownNotFound(t *testing.T) {
	s := store.New(storetest.NewDB(t))
	srv := internalServer{Store: s}
	_, err := srv.GetSession(t.Context(), connect.NewRequest(&iamv1.GetSessionRequest{SessionId: uuid.NewString()}))
	if connect.CodeOf(err) != connect.CodeNotFound {
		t.Fatalf("unknown session: %v", err)
	}
	u, err := s.Login(t.Context(), store.Identity{Subject: "u", Email: "u@x.io", Name: "u"})
	if err != nil {
		t.Fatal(err)
	}
	_, sess, err := s.CreateSession(t.Context(), u.User.ID, store.SessionCLI, time.Hour, "test", "127.0.0.1")
	if err != nil {
		t.Fatal(err)
	}
	res, err := srv.GetSession(t.Context(), connect.NewRequest(&iamv1.GetSessionRequest{SessionId: sess.ID.String()}))
	if err != nil || res.Msg.UserId != u.User.ID.String() || res.Msg.Revoked || res.Msg.ExpiresAt.AsTime().Sub(sess.ExpiresAt).Abs() > time.Millisecond {
		t.Fatalf("live session: %+v %v", res, err)
	}
	if err := s.RevokeSession(t.Context(), u.User.ID, sess.ID); err != nil {
		t.Fatal(err)
	}
	if res, err := srv.GetSession(t.Context(), connect.NewRequest(&iamv1.GetSessionRequest{SessionId: sess.ID.String()})); err != nil || !res.Msg.Revoked {
		t.Fatalf("revoked session: %+v %v", res, err)
	}
	if active, err := iamclient.SessionActive(t.Context(), srv, sess.ID); err != nil || active {
		t.Fatalf("revoked session active=%v err=%v", active, err)
	}
	if active, err := iamclient.SessionActive(t.Context(), srv, uuid.New()); err != nil || active {
		t.Fatalf("unknown session active=%v err=%v", active, err)
	}
}

func TestGetMembership(t *testing.T) {
	s := store.New(storetest.NewDB(t))
	srv := internalServer{Store: s}
	u, err := s.Login(t.Context(), store.Identity{Subject: "u", Email: "u@x.io", Name: "u"})
	if err != nil {
		t.Fatal(err)
	}
	org, err := s.CreateOrg(t.Context(), "A", u.User.ID)
	if err != nil {
		t.Fatal(err)
	}
	res, err := srv.GetMembership(t.Context(), connect.NewRequest(&iamv1.GetMembershipRequest{OrgId: org.ID.String(), UserId: u.User.ID.String()}))
	if err != nil || !res.Msg.Member || res.Msg.Role != string(store.RoleOwner) {
		t.Fatalf("owner: %+v %v", res, err)
	}
	if member, err := iamclient.IsMember(t.Context(), srv, org.ID, uuid.New()); err != nil || member {
		t.Fatalf("stranger member=%v err=%v", member, err)
	}
	if _, err := srv.GetMembership(t.Context(), connect.NewRequest(&iamv1.GetMembershipRequest{OrgId: "nope", UserId: u.User.ID.String()})); connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Fatalf("bad org: %v", err)
	}
}

func TestLookups(t *testing.T) {
	ctx := t.Context()
	s := store.New(storetest.NewDB(t))
	srv := internalServer{Store: s}
	alice, err := s.Login(ctx, store.Identity{Subject: "alice", Email: "alice@x.io", Name: "alice"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.AddSSHKey(ctx, store.SSHKey{UserID: alice.User.ID, Name: "k", PublicKey: "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIO+6yIP+G69X+7mX0h8gSHrNNG4s+hDeXmzGTGYeH6nA laptop"}); err != nil {
		t.Fatal(err)
	}
	org, err := s.CreateOrg(ctx, "acme", alice.User.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateTeam(ctx, org.ID, "core"); err != nil {
		t.Fatal(err)
	}
	ids := []string{alice.User.ID.String()}
	us, err := srv.GetUsers(ctx, connect.NewRequest(&iamv1.GetUsersRequest{UserIds: ids}))
	if err != nil || len(us.Msg.Users) != 1 || us.Msg.Users[0].Name != "alice" || us.Msg.Users[0].Email != "alice@x.io" {
		t.Fatalf("GetUsers: %v %v", us, err)
	}
	ks, err := srv.ListSSHKeys(ctx, connect.NewRequest(&iamv1.ListSSHKeysRequest{UserIds: ids}))
	if err != nil || len(ks.Msg.Keys) != 1 || ks.Msg.Keys[0].UserId != alice.User.ID.String() {
		t.Fatalf("ListSSHKeys: %v %v", ks, err)
	}
	ts, err := srv.ListTeams(ctx, connect.NewRequest(&iamv1.ListTeamsRequest{OrgId: org.ID.String()}))
	if err != nil || len(ts.Msg.Teams) != 1 || ts.Msg.Teams[0].Name != "core" {
		t.Fatalf("ListTeams: %v %v", ts, err)
	}
}

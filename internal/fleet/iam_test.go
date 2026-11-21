package fleet_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"

	"connectrpc.com/connect"

	iamv1 "github.com/asterism-dev/observatory/gen/go/observatory/iam/v1"
	"github.com/asterism-dev/observatory/gen/go/observatory/iam/v1/iamv1connect"
	"github.com/asterism-dev/observatory/internal/fleet"
)

// fakeIAM answers ListSSHKeys with one key per requested user, named after the user.
type fakeIAM struct {
	iamv1connect.IAMInternalServiceClient
}

func (fakeIAM) ListSSHKeys(_ context.Context, req *connect.Request[iamv1.ListSSHKeysRequest]) (*connect.Response[iamv1.ListSSHKeysResponse], error) {
	out := &iamv1.ListSSHKeysResponse{}
	for _, id := range req.Msg.UserIds {
		out.Keys = append(out.Keys, &iamv1.SSHKey{UserId: id, PublicKey: "key-" + id})
	}
	return connect.NewResponse(out), nil
}

func TestBootKeysViaIAM(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	f, err := fleet.New(ctx, e.s, e.z, e.h, fakeIAM{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(f.Close)
	e.f, e.srv = f, httptest.NewServer(http.HandlerFunc(f.ServeConnect))
	t.Cleanup(e.srv.Close)

	team, err := e.s.CreateTeam(ctx, e.org.ID, "core")
	if err != nil {
		t.Fatal(err)
	}
	if err := e.s.SetTeamMember(ctx, e.org.ID, team.ID, e.owner, false); err != nil {
		t.Fatal(err)
	}
	if err := e.z.Reload(ctx); err != nil {
		t.Fatal(err)
	}
	e.sshKey(e.owner) // A key only in the store must not reach the boot.
	n, cred := e.node(e.org.ID, "n")
	e.vm(e.org.ID, n.ID, "user-vm", &e.owner, nil)
	e.vm(e.org.ID, n.ID, "team-vm", nil, &team.ID)

	want := []string{"key-" + e.owner.String()}
	for _, d := range e.connect(cred).desired().Desired {
		if !slices.Equal(d.Boot.SSHPublicKeys, want) {
			t.Errorf("%s keys = %v, want %v", d.Spec.Name, d.Boot.SSHPublicKeys, want)
		}
	}
}

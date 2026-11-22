package hubserver

import (
	"context"
	"net/http/httptest"
	"testing"

	"connectrpc.com/connect"

	hubv1 "github.com/asterism-dev/observatory/gen/go/observatory/hub/v1"
	"github.com/asterism-dev/observatory/gen/go/observatory/hub/v1/hubv1connect"
	"github.com/asterism-dev/observatory/internal/api"
	"github.com/asterism-dev/observatory/internal/authz"
	"github.com/asterism-dev/observatory/internal/db/dbtest"
	"github.com/asterism-dev/observatory/internal/platform/iamtest"
	"github.com/asterism-dev/observatory/internal/platform/servicetoken"
	"github.com/asterism-dev/observatory/internal/store/storetest"
)

func TestCheckOwnershipRequiresServiceToken(t *testing.T) {
	ctx := context.Background()
	pool := dbtest.New(t)
	s := storetest.New(t, pool)
	iam := iamtest.New(t)
	alice := iam.AddUser("alice")
	org := iam.CreateOrg(alice.ID, "acme")
	team := iam.AddTeam(org, "core")
	if _, err := pool.Exec(ctx, "INSERT INTO disks (org_id, user_id, name, size_gb) VALUES ($1, $2, 'd', 1)", org, alice.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, "INSERT INTO disks (org_id, team_id, name, size_gb, deleted_at) VALUES ($1, $2, 'gone', 1, now())", org, team); err != nil {
		t.Fatal(err)
	}
	z := iam.Policy(t, s.OwnerRefs)
	srv := httptest.NewServer(servicetoken.Middleware(map[string]string{"iam": svcToken})(internalHub(api.ResourceServer{Store: s, Policy: currentPolicy{z.Service}})))
	defer srv.Close()
	c := hubv1connect.NewResourceServiceClient(srv.Client(), srv.URL+"/internal/hub")
	check := func(subject, auth string) (*hubv1.CheckOwnershipResponse, error) {
		res, err := c.CheckOwnership(ctx, withAuth(&hubv1.CheckOwnershipRequest{OrgId: org.String(), Subject: subject}, auth))
		if err != nil {
			return nil, err
		}
		return res.Msg, nil
	}

	canHold := func(auth string) error {
		_, err := c.CanHold(ctx, withAuth(&hubv1.CanHoldRequest{OrgId: org.String(), Actor: authz.UserSubject(alice.ID)}, auth))
		return err
	}
	for _, auth := range []string{"", "Bearer " + otherTok} {
		if _, err := check(authz.UserSubject(alice.ID), auth); connect.CodeOf(err) != connect.CodePermissionDenied {
			t.Errorf("with %q: %v", auth, err)
		}
		if err := canHold(auth); connect.CodeOf(err) != connect.CodePermissionDenied {
			t.Errorf("CanHold with %q: %v", auth, err)
		}
	}
	if err := canHold("Bearer " + svcToken); err != nil {
		t.Errorf("CanHold with the service token: %v", err)
	}
	auth := "Bearer " + svcToken
	if got, err := check(authz.UserSubject(alice.ID), auth); err != nil || !got.OwnsDisks || got.OwnsVms {
		t.Fatalf("alice: %v %v", got, err)
	}
	if got, err := check(authz.TeamSubject(team), auth); err != nil || got.OwnsDisks || got.OwnsVms {
		t.Fatalf("team with only a deleted disk: %v %v", got, err)
	}
	for _, bad := range []string{"", "role:owner", "user:nope", "user:" + "7D444840-9DC0-11D1-B245-5FFDCE74FAD2"} {
		if _, err := check(bad, auth); connect.CodeOf(err) != connect.CodeInvalidArgument {
			t.Errorf("subject %q: %v", bad, err)
		}
	}
}

// currentPolicy serves z at every revision, since this test never moves the policy.
type currentPolicy struct{ *authz.Service }

func (currentPolicy) WaitRevision(context.Context, uint64) error { return nil }

const (
	svcToken = "0123456789abcdef0123456789abcdef"
	otherTok = "fedcba9876543210fedcba9876543210"
)

func withAuth[T any](msg *T, auth string) *connect.Request[T] {
	r := connect.NewRequest(msg)
	r.Header().Set("Authorization", auth)
	return r
}

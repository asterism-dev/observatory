package iamtest

import (
	"context"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"
	"github.com/google/uuid"

	iamv1 "github.com/asterism-dev/observatory/gen/go/observatory/iam/v1"
	coreauthz "github.com/asterism-dev/observatory/internal/authz"
	"github.com/asterism-dev/observatory/internal/platform/authz"
	"github.com/asterism-dev/observatory/internal/platform/catalog"
	"github.com/asterism-dev/observatory/internal/platform/identity"
)

func TestIAMTestPolicyStream(t *testing.T) {
	s := New(t)
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	w, err := authz.NewWatcher(s.URL(), s.ServiceToken(), nil)
	if err != nil {
		t.Fatal(err)
	}
	go w.Run(ctx)
	alice, bob := s.AddUser("alice"), s.AddUser("bob")
	org := s.CreateOrg(alice.ID, "acme")
	s.AddMember(org, bob.ID, "member")
	isMember := func() bool {
		p, _, _ := w.Policy()
		return slices.Contains(p.Memberships, coreauthz.MemberRole{OrgID: org, UserID: bob.ID, Role: "member"})
	}
	joined := s.Revision()
	if err := w.WaitRevision(ctx, joined); err != nil {
		t.Fatal(err)
	}
	if !isMember() {
		t.Fatal("bob's membership missing from the streamed policy")
	}
	s.RemoveMember(org, bob.ID)
	if s.Revision() <= joined {
		t.Fatalf("revision %d after the removal, %d before", s.Revision(), joined)
	}
	if err := w.WaitRevision(ctx, s.Revision()); err != nil {
		t.Fatal(err)
	}
	if isMember() {
		t.Fatal("bob still a member after the removal")
	}
}

func TestIAMTestJWTVerifies(t *testing.T) {
	s := New(t)
	v, err := identity.NewVerifier(s.URL(), Issuer, s.ServiceToken(), nil)
	if err != nil {
		t.Fatal(err)
	}
	root, alice := s.AddUser("root"), s.AddUser("alice")
	for _, c := range []struct {
		user  User
		kind  string
		admin bool
	}{{root, "browser", true}, {alice, "cli", false}} {
		raw, session := s.JWT(c.user.ID, c.kind)
		p, err := v.Verify(t.Context(), raw)
		if err != nil {
			t.Fatal(err)
		}
		if p.User != c.user.ID || p.Session != session || p.Name != c.user.Name || p.SessionKind != c.kind || p.PlatformAdmin != c.admin || !p.SessionExpires.After(time.Now()) {
			t.Fatalf("%s: principal %+v", c.user.Name, p)
		}
	}
}

func TestIAMTestSessionsAndMemberships(t *testing.T) {
	s := New(t)
	c := s.Client()
	alice := s.AddUser("alice")
	_, session := s.JWT(alice.ID, "cli")
	get := func(id uuid.UUID) (*iamv1.GetSessionResponse, error) {
		res, err := c.GetSession(t.Context(), connect.NewRequest(&iamv1.GetSessionRequest{SessionId: id.String()}))
		if err != nil {
			return nil, err
		}
		return res.Msg, nil
	}
	if got, err := get(session); err != nil || got.UserId != alice.ID.String() || got.Revoked || !got.ExpiresAt.AsTime().After(time.Now()) {
		t.Fatalf("live session: %+v %v", got, err)
	}
	s.RevokeSession(session)
	if got, err := get(session); err != nil || !got.Revoked {
		t.Fatalf("revoked session: %+v %v", got, err)
	}
	if _, err := get(uuid.New()); connect.CodeOf(err) != connect.CodeNotFound {
		t.Fatalf("unknown session: %v", err)
	}

	member := func(org uuid.UUID) *iamv1.GetMembershipResponse {
		res, err := c.GetMembership(t.Context(), connect.NewRequest(&iamv1.GetMembershipRequest{OrgId: org.String(), UserId: alice.ID.String()}))
		if err != nil {
			t.Fatal(err)
		}
		return res.Msg
	}
	if got := member(s.PersonalOrg(alice.ID)); !got.Member || got.Role != "owner" {
		t.Fatalf("personal org: %+v", got)
	}
	if got := member(uuid.New()); got.Member {
		t.Fatalf("stranger org: %+v", got)
	}
}

func TestIAMTestRefusesTokenForUnknownUser(t *testing.T) {
	s := New(t)
	tok, _ := s.SessionToken(uuid.New(), "cli")
	req, err := http.NewRequest(http.MethodPost, s.URL()+"/internal/iam/token", strings.NewReader(`{"session_token":"`+tok+`"}`))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+s.ServiceToken())
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode == http.StatusOK {
		t.Fatal("minted a JWT for an unknown user")
	}
}

func TestIAMTestStreamsCatalogs(t *testing.T) {
	s := New(t)
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	w, err := authz.NewWatcher(s.URL(), s.ServiceToken(), nil)
	if err != nil {
		t.Fatal(err)
	}
	go w.Run(ctx)
	release := s.HoldCatalogs()
	digest, err := catalog.Register(ctx, s.CatalogClient(), coreauthz.Catalog{Service: "audit", Actions: []coreauthz.Action{{Name: "audit.events.list", Object: "org/audit"}}})
	if err != nil {
		t.Fatal(err)
	}
	w.Require("audit", digest)
	if err := w.WaitRevision(ctx, s.Revision()); err != nil {
		t.Fatal(err)
	}
	if _, ok := w.Current(); ok {
		t.Fatal("current while the fake holds the catalog")
	}
	release()
	for {
		if _, ok := w.Current(); ok {
			break
		}
		if ctx.Err() != nil {
			t.Fatal("not current after the catalog was released")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

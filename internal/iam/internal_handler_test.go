package iam

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"sync/atomic"
	"testing"
	"time"

	"connectrpc.com/connect"
	"github.com/google/uuid"

	iamv1 "github.com/asterism-dev/observatory/gen/go/observatory/iam/v1"
	"github.com/asterism-dev/observatory/gen/go/observatory/iam/v1/iamv1connect"
	"github.com/asterism-dev/observatory/internal/authz"
	"github.com/asterism-dev/observatory/internal/iam/store"
	"github.com/asterism-dev/observatory/internal/iam/store/storetest"
	platformauthz "github.com/asterism-dev/observatory/internal/platform/authz"
	"github.com/asterism-dev/observatory/internal/platform/servicetoken"
	"github.com/asterism-dev/observatory/internal/seal"
)

const (
	svcToken   = "0123456789abcdef0123456789abcdef"
	otherTok   = "fedcba9876543210fedcba9876543210"
	hubTok     = "hhhhhhhhhhhhhhhhhhhhhhhhhhhhhhhh"
	computeTok = "cccccccccccccccccccccccccccccccc"
	issuerURL = "https://hub.test"
)

// iamEnv serves internalHandler over a policy the test sets, published on every reload of z.
type iamEnv struct {
	s      *store.Store
	z      *authz.Service
	h      http.Handler
	policy atomic.Pointer[authz.Policy]
}

// sessionToken stands in for a user's session token, which is no service credential.
const sessionToken = "obs_0123456789abcdef"

func newIAMEnv(t *testing.T) *iamEnv {
	t.Helper()
	ctx := context.Background()
	e := &iamEnv{s: store.New(storetest.NewDB(t))}
	e.policy.Store(&authz.Policy{})
	z, err := authz.NewService(ctx, func(context.Context) (authz.Policy, error) { return *e.policy.Load(), nil })
	if err != nil {
		t.Fatal(err)
	}
	iss, err := NewIssuer(ctx, e.s, seal.Key{1}, issuerURL)
	if err != nil {
		t.Fatal(err)
	}
	ps := platformauthz.NewPublisher()
	z.OnReload(func(_, p authz.Policy) { ps.Publish(p) })
	e.z, e.h = z, servicetoken.Middleware(map[string]string{"edge": svcToken, "audit": otherTok, "hub": hubTok, "compute": computeTok})(internalHandler(iss, ps, internalServer{Store: e.s}))
	return e
}

func (e *iamEnv) do(method, path, auth string, body any) *httptest.ResponseRecorder {
	var buf bytes.Buffer
	if body != nil {
		_ = json.NewEncoder(&buf).Encode(body)
	}
	r := httptest.NewRequest(method, path, &buf)
	if auth != "" {
		r.Header.Set("Authorization", auth)
	}
	rec := httptest.NewRecorder()
	e.h.ServeHTTP(rec, r)
	return rec
}

func TestTokenEndpointOnlyForEdge(t *testing.T) {
	e := newIAMEnv(t)
	if rec := e.do("POST", "/internal/iam/token", "Bearer "+otherTok, map[string]string{"session_token": sessionToken}); rec.Code != http.StatusForbidden {
		t.Fatalf("audit minting user tokens: %d", rec.Code)
	}
	if rec := e.do("GET", "/internal/iam/jwks", "Bearer "+otherTok, nil); rec.Code != http.StatusOK {
		t.Fatalf("audit reading jwks: %d", rec.Code)
	}
	srv := httptest.NewServer(e.h)
	defer srv.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req := connect.NewRequest(&iamv1.WatchPolicyRequest{})
	req.Header().Set("Authorization", "Bearer "+otherTok)
	stream, err := iamv1connect.NewPolicyServiceClient(srv.Client(), srv.URL+"/internal/iam").WatchPolicy(ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = stream.Close() }()
	if !stream.Receive() {
		t.Fatalf("audit watching policy: %v", stream.Err())
	}
}

func TestInternalRequiresServiceToken(t *testing.T) {
	e := newIAMEnv(t)
	tok := sessionToken
	paths := []struct{ method, path string }{
		{"POST", "/internal/iam/token"},
		{"GET", "/internal/iam/jwks"},
		{"POST", "/internal/iam" + iamv1connect.PolicyServiceWatchPolicyProcedure},
	}
	for _, p := range paths {
		// The session token itself is no service credential.
		for _, auth := range []string{"", "Bearer wrong-wrong-wrong-wrong-wrong-wrong", "Bearer " + tok} {
			if rec := e.do(p.method, p.path, auth, map[string]string{"session_token": tok}); rec.Code != http.StatusForbidden {
				t.Errorf("%s %s with %q: %d", p.method, p.path, auth, rec.Code)
			}
		}
	}

	// Without OBSERVATORY_SERVICE_TOKENS every internal call is refused.
	iss, err := NewIssuer(context.Background(), e.s, seal.Key{1}, issuerURL)
	if err != nil {
		t.Fatal(err)
	}
	closed := servicetoken.Middleware(nil)(internalHandler(iss, platformauthz.NewPublisher(), internalServer{Store: e.s}))
	for _, p := range paths {
		r := httptest.NewRequest(p.method, p.path, nil)
		r.Header.Set("Authorization", "Bearer ")
		rec := httptest.NewRecorder()
		closed.ServeHTTP(rec, r)
		if rec.Code != http.StatusForbidden {
			t.Errorf("no tokens configured, %s: %d", p.path, rec.Code)
		}
	}
}

func TestIAMInternalRequiresServiceToken(t *testing.T) {
	e := newIAMEnv(t)
	ctx := t.Context()
	srv := httptest.NewServer(e.h)
	defer srv.Close()
	c := iamv1connect.NewIAMInternalServiceClient(srv.Client(), srv.URL+"/internal/iam")
	alice, org := uuid.NewString(), uuid.NewString()
	call := func(auth string) []error {
		_, uerr := c.GetUsers(ctx, withAuth(&iamv1.GetUsersRequest{UserIds: []string{alice}}, auth))
		_, kerr := c.ListSSHKeys(ctx, withAuth(&iamv1.ListSSHKeysRequest{UserIds: []string{alice}}, auth))
		_, terr := c.ListTeams(ctx, withAuth(&iamv1.ListTeamsRequest{OrgId: org}, auth))
		_, merr := c.GetMembership(ctx, withAuth(&iamv1.GetMembershipRequest{OrgId: org, UserId: alice}, auth))
		_, serr := c.GetSession(ctx, withAuth(&iamv1.GetSessionRequest{SessionId: uuid.NewString()}, auth))
		return []error{uerr, kerr, terr, merr, serr}
	}
	// The session token itself is no service credential.
	for _, auth := range []string{"", "Bearer wrong-wrong-wrong-wrong-wrong-wrong", "Bearer " + sessionToken} {
		for _, err := range call(auth) {
			if connect.CodeOf(err) != connect.CodePermissionDenied {
				t.Errorf("with %q: %v", auth, err)
			}
		}
	}
	errs := call("Bearer " + hubTok)
	if slices.ContainsFunc(errs[:4], func(err error) bool { return err != nil }) || connect.CodeOf(errs[4]) != connect.CodeNotFound {
		t.Fatalf("with the service token: %v", errs)
	}
}

func TestInternalAllowList(t *testing.T) {
	e := newIAMEnv(t)
	rpc := func(procedure string) string { return "/internal/iam" + procedure }
	paths := []string{
		"/internal/iam/token",
		"/internal/iam/jwks",
		rpc(iamv1connect.PolicyServiceWatchPolicyProcedure),
		rpc(iamv1connect.IAMInternalServiceGetUsersProcedure),
		rpc(iamv1connect.IAMInternalServiceListSSHKeysProcedure),
		rpc(iamv1connect.IAMInternalServiceListTeamsProcedure),
		rpc(iamv1connect.IAMInternalServiceGetMembershipProcedure),
		rpc(iamv1connect.IAMInternalServiceGetSessionProcedure),
	}
	allowed := map[string][]string{"edge": paths[:1], "hub": paths[1:], "audit": paths[1:3]}
	for caller, tok := range map[string]string{"edge": svcToken, "hub": hubTok, "audit": otherTok, "compute": computeTok} {
		for _, p := range paths {
			method := "POST"
			if p == "/internal/iam/jwks" {
				method = "GET"
			}
			rec := e.do(method, p, "Bearer "+tok, nil)
			if want := slices.Contains(allowed[caller], p); want == (rec.Code == http.StatusForbidden) {
				t.Errorf("%s on %s: %d, allowed %v", caller, p, rec.Code, want)
			}
		}
	}
}

func withAuth[T any](msg *T, auth string) *connect.Request[T] {
	r := connect.NewRequest(msg)
	r.Header().Set("Authorization", auth)
	return r
}

func TestWatchPolicySendsSnapshotAndUpdates(t *testing.T) {
	e := newIAMEnv(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	srv := httptest.NewServer(e.h)
	defer srv.Close()
	client := iamv1connect.NewPolicyServiceClient(srv.Client(), srv.URL+"/internal/iam")
	req := connect.NewRequest(&iamv1.WatchPolicyRequest{})
	req.Header().Set("Authorization", "Bearer "+hubTok)
	stream, err := client.WatchPolicy(ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = stream.Close() }()
	if !stream.Receive() {
		t.Fatalf("snapshot: %v", stream.Err())
	}
	first := stream.Msg()
	org, owner, bob := uuid.New(), uuid.New(), uuid.New()
	if hasMember(first, org, bob) {
		t.Fatal("bob is a member before the invite")
	}

	e.policy.Store(&authz.Policy{Memberships: []authz.MemberRole{{OrgID: org, UserID: owner, Role: "owner"}, {OrgID: org, UserID: bob, Role: "member"}}})
	if err := e.z.Reload(ctx); err != nil {
		t.Fatal(err)
	}
	if !stream.Receive() {
		t.Fatalf("update: %v", stream.Err())
	}
	second := stream.Msg()
	if second.Revision <= first.Revision {
		t.Fatalf("revision %d after %d", second.Revision, first.Revision)
	}
	if !hasMember(second, org, bob) {
		t.Fatalf("bob missing from %v", second.Memberships)
	}
}

func hasMember(p *iamv1.Policy, org, user uuid.UUID) bool {
	return slices.ContainsFunc(p.Memberships, func(m *iamv1.MemberRole) bool {
		return m.OrgId == org.String() && m.UserId == user.String() && m.Role == "member"
	})
}

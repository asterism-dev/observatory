package authz

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"connectrpc.com/connect"
	iamv1 "github.com/asterism-dev/observatory/gen/go/observatory/iam/v1"
	"github.com/asterism-dev/observatory/gen/go/observatory/iam/v1/iamv1connect"
	testv1 "github.com/asterism-dev/observatory/gen/go/observatory/test/v1"
	"github.com/asterism-dev/observatory/gen/go/observatory/test/v1/testv1connect"
	"github.com/asterism-dev/observatory/internal/platform/identity"
	"github.com/asterism-dev/observatory/internal/platform/permission"
	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
)

const (
	testIssuer = "https://obs.example"
	testToken  = "service-token-service-token-service-token"
)

type fakePolicy struct {
	iamv1connect.UnimplementedPolicyServiceHandler
	conns   chan chan *iamv1.Policy
	streams chan struct{}
}

// WatchPolicy hands the test a channel per connection; closing it ends the stream.
func (f *fakePolicy) WatchPolicy(ctx context.Context, _ *connect.Request[iamv1.WatchPolicyRequest], stream *connect.ServerStream[iamv1.Policy]) error {
	ch := make(chan *iamv1.Policy)
	select {
	case f.conns <- ch:
	case <-ctx.Done():
		return nil
	}
	for {
		select {
		case <-ctx.Done():
			return nil
		case p, ok := <-ch:
			if !ok {
				return nil
			}
			if err := stream.Send(p); err != nil {
				return err
			}
		}
	}
}

type echoServer struct {
	testv1connect.UnimplementedEchoServiceHandler
}

func (echoServer) Echo(_ context.Context, r *connect.Request[testv1.EchoRequest]) (*connect.Response[testv1.EchoResponse], error) {
	return connect.NewResponse(&testv1.EchoResponse{Text: r.Msg.GetText()}), nil
}
func (echoServer) Peek(context.Context, *connect.Request[testv1.PeekRequest]) (*connect.Response[testv1.PeekResponse], error) {
	return connect.NewResponse(&testv1.PeekResponse{}), nil
}
func (echoServer) GetEcho(ctx context.Context, _ *connect.Request[testv1.GetEchoRequest]) (*connect.Response[testv1.GetEchoResponse], error) {
	if _, ok := identity.FromContext(ctx); !ok {
		return nil, connect.NewError(connect.CodeInternal, errors.New("no principal"))
	}
	return connect.NewResponse(&testv1.GetEchoResponse{}), nil
}
func (echoServer) Ping(context.Context, *connect.Request[testv1.PingRequest]) (*connect.Response[testv1.PingResponse], error) {
	return connect.NewResponse(&testv1.PingResponse{}), nil
}

type fixture struct {
	key    ed25519.PrivateKey
	policy *fakePolicy
	echo   testv1connect.EchoServiceClient
	org    uuid.UUID
	member uuid.UUID
	viewer uuid.UUID
}

func newFixture(t *testing.T) *fixture {
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	jwks := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/internal/iam/jwks" {
			http.NotFound(w, r)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"keys": []map[string]string{{"kty": "OKP", "crv": "Ed25519", "kid": "k1", "x": base64.RawURLEncoding.EncodeToString(pub)}}})
	}))
	t.Cleanup(jwks.Close)
	v, err := identity.NewVerifier(jwks.URL, testIssuer, testToken, jwks.Client())
	if err != nil {
		t.Fatal(err)
	}

	f := &fixture{key: priv, policy: &fakePolicy{conns: make(chan chan *iamv1.Policy)}, org: uuid.New(), member: uuid.New(), viewer: uuid.New()}
	path, h := iamv1connect.NewPolicyServiceHandler(f.policy)
	mux := http.NewServeMux()
	mux.Handle("/internal/iam"+path, http.StripPrefix("/internal/iam", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+testToken {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		h.ServeHTTP(w, r)
	})))
	hub := httptest.NewServer(mux)
	t.Cleanup(hub.Close)
	w, err := NewWatcher(hub.URL+"/", testToken, hub.Client())
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { w.Run(ctx); close(done) }()
	t.Cleanup(func() { cancel(); <-done })

	svcMux := http.NewServeMux()
	svcMux.Handle(testv1connect.NewEchoServiceHandler(echoServer{}, connect.WithInterceptors(NewInterceptor(v, w))))
	svc := httptest.NewServer(svcMux)
	t.Cleanup(svc.Close)
	f.echo = testv1connect.NewEchoServiceClient(svc.Client(), svc.URL)
	return f
}

// connect waits for the watcher's stream and returns the channel that feeds it.
func (f *fixture) connect(t *testing.T) chan *iamv1.Policy {
	t.Helper()
	select {
	case ch := <-f.policy.conns:
		return ch
	case <-time.After(5 * time.Second):
		t.Fatal("watcher did not connect")
		return nil
	}
}

// snapshot makes every given user an org member; only f.member also gets a role granting test.echoes.get.
func (f *fixture) snapshot(rev uint64, members ...uuid.UUID) *iamv1.Policy {
	role := uuid.NewString()
	p := &iamv1.Policy{Revision: rev, Roles: []*iamv1.CustomRole{{
		Id: role, OrgId: f.org.String(), Name: "echo reader",
		Rules: []*iamv1.Rule{{Effect: "allow", Object: "org/echoes/*", Action: "test.echoes.get"}},
	}}}
	for _, m := range members {
		p.Memberships = append(p.Memberships, &iamv1.MemberRole{OrgId: f.org.String(), UserId: m.String(), Role: "member"})
		if m == f.member {
			p.Bindings = append(p.Bindings, &iamv1.Binding{OrgId: f.org.String(), Subject: "user:" + m.String(), RoleId: role})
		}
	}
	return p
}

func (f *fixture) token(t *testing.T, user uuid.UUID, admin bool) string {
	t.Helper()
	now := time.Now()
	tok := jwt.NewWithClaims(jwt.SigningMethodEdDSA, jwt.MapClaims{
		"iss": testIssuer, "aud": []string{"observatory"}, "sub": user.String(), "sid": uuid.NewString(), "pa": admin,
		"iat": now.Unix(), "nbf": now.Unix(), "exp": now.Add(5 * time.Minute).Unix(),
	})
	tok.Header["kid"] = "k1"
	s, err := tok.SignedString(f.key)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func withToken[T any](tok string, msg *T) *connect.Request[T] {
	r := connect.NewRequest(msg)
	r.Header().Set("Authorization", "Bearer "+tok)
	return r
}

func (f *fixture) getEcho(t *testing.T, user uuid.UUID, org, echo string) error {
	_, err := f.echo.GetEcho(context.Background(), withToken(f.token(t, user, false), &testv1.GetEchoRequest{Org: org, Echo: echo}))
	return err
}

func wantCode(t *testing.T, err error, want connect.Code) {
	t.Helper()
	if want == 0 {
		if err != nil {
			t.Fatalf("err = %v, want nil", err)
		}
		return
	}
	if got := connect.CodeOf(err); err == nil || got != want {
		t.Fatalf("err = %v (code %v), want %v", err, got, want)
	}
}

// eventually retries until the call returns want; the watcher applies snapshots asynchronously.
func eventually(t *testing.T, want connect.Code, call func() error) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		err := call()
		if (want == 0 && err == nil) || (err != nil && connect.CodeOf(err) == want) {
			return
		}
		if time.Now().After(deadline) {
			wantCode(t, err, want)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestNoSnapshotIsUnavailable(t *testing.T) {
	f := newFixture(t)
	wantCode(t, f.getEcho(t, f.member, f.org.String(), uuid.NewString()), connect.CodeUnavailable)
}

func TestInterceptor(t *testing.T) {
	f := newFixture(t)
	f.connect(t) <- f.snapshot(1, f.member, f.viewer)
	echo := uuid.NewString()
	eventually(t, 0, func() error { return f.getEcho(t, f.member, f.org.String(), echo) })

	t.Run("non-member is not found", func(t *testing.T) {
		wantCode(t, f.getEcho(t, uuid.New(), f.org.String(), echo), connect.CodeNotFound)
	})
	t.Run("member without permission is denied", func(t *testing.T) {
		wantCode(t, f.getEcho(t, f.viewer, f.org.String(), echo), connect.CodePermissionDenied)
	})
	t.Run("non-canonical placeholder is invalid", func(t *testing.T) {
		wantCode(t, f.getEcho(t, f.member, f.org.String(), "ABC"), connect.CodeInvalidArgument)
		wantCode(t, f.getEcho(t, f.member, f.org.String(), "{"+uuid.NewString()+"}"), connect.CodeInvalidArgument)
	})
	t.Run("non-canonical org is invalid", func(t *testing.T) {
		wantCode(t, f.getEcho(t, f.member, "", echo), connect.CodeInvalidArgument)
		wantCode(t, f.getEcho(t, f.member, "urn:uuid:"+f.org.String(), echo), connect.CodeInvalidArgument)
	})
	t.Run("missing or bad token is unauthenticated", func(t *testing.T) {
		_, err := f.echo.GetEcho(context.Background(), connect.NewRequest(&testv1.GetEchoRequest{Org: f.org.String(), Echo: echo}))
		wantCode(t, err, connect.CodeUnauthenticated)
		_, err = f.echo.GetEcho(context.Background(), withToken("garbage", &testv1.GetEchoRequest{Org: f.org.String(), Echo: echo}))
		wantCode(t, err, connect.CodeUnauthenticated)
	})
	t.Run("unannotated method is denied", func(t *testing.T) {
		_, err := f.echo.Ping(context.Background(), withToken(f.token(t, f.member, true), &testv1.PingRequest{}))
		wantCode(t, err, connect.CodeUnimplemented)
	})
	t.Run("unspecified kind is denied", func(t *testing.T) {
		_, err := f.echo.Peek(context.Background(), withToken(f.token(t, f.member, true), &testv1.PeekRequest{}))
		wantCode(t, err, connect.CodeUnimplemented)
	})
	t.Run("platform scope only for platform admins", func(t *testing.T) {
		_, err := f.echo.Echo(context.Background(), withToken(f.token(t, f.member, false), &testv1.EchoRequest{Text: "hi"}))
		wantCode(t, err, connect.CodePermissionDenied)
		res, err := f.echo.Echo(context.Background(), withToken(f.token(t, f.member, true), &testv1.EchoRequest{Text: "hi"}))
		wantCode(t, err, 0)
		if res.Msg.GetText() != "hi" {
			t.Fatalf("text = %q", res.Msg.GetText())
		}
	})
}

func TestMemberRemovedViaStream(t *testing.T) {
	f := newFixture(t)
	stream := f.connect(t)
	stream <- f.snapshot(1, f.member)
	echo := uuid.NewString()
	eventually(t, 0, func() error { return f.getEcho(t, f.member, f.org.String(), echo) })
	stream <- f.snapshot(2)
	eventually(t, connect.CodeNotFound, func() error { return f.getEcho(t, f.member, f.org.String(), echo) })
}

func TestStreamLossKeepsSnapshotAndReconnects(t *testing.T) {
	f := newFixture(t)
	stream := f.connect(t)
	stream <- f.snapshot(1, f.member)
	echo := uuid.NewString()
	eventually(t, 0, func() error { return f.getEcho(t, f.member, f.org.String(), echo) })
	close(stream)
	stream = f.connect(t)
	wantCode(t, f.getEcho(t, f.member, f.org.String(), echo), 0)
	stream <- f.snapshot(2)
	eventually(t, connect.CodeNotFound, func() error { return f.getEcho(t, f.member, f.org.String(), echo) })
}

func TestInvalidSnapshotKeepsPrevious(t *testing.T) {
	f := newFixture(t)
	stream := f.connect(t)
	stream <- f.snapshot(1, f.member)
	echo := uuid.NewString()
	eventually(t, 0, func() error { return f.getEcho(t, f.member, f.org.String(), echo) })
	bad := f.snapshot(2)
	bad.Memberships = append(bad.Memberships, &iamv1.MemberRole{OrgId: "nope", UserId: f.member.String(), Role: "admin"})
	stream <- bad
	// A valid snapshot after the bad one proves the bad one was processed and skipped.
	stream <- f.snapshot(3, f.member, f.viewer)
	eventually(t, connect.CodePermissionDenied, func() error { return f.getEcho(t, f.viewer, f.org.String(), echo) })
	wantCode(t, f.getEcho(t, f.member, f.org.String(), echo), 0)
}

func TestPlatformScopeUserObject(t *testing.T) {
	user := uuid.New()
	perm := permission.Permission{Name: "test.echoes.get", Object: "user:{echo}/echoes"}
	for _, tc := range []struct {
		name  string
		p     identity.Principal
		echo  string
		allow bool
	}{
		{"own scope", identity.Principal{User: user}, user.String(), true},
		{"other user's scope", identity.Principal{User: user}, uuid.NewString(), false},
		{"platform admin", identity.Principal{User: user, PlatformAdmin: true}, uuid.NewString(), true},
	} {
		err := platformScope(tc.p, perm, (&testv1.GetEchoRequest{Echo: tc.echo}).ProtoReflect())
		if (err == nil) != tc.allow {
			t.Errorf("%s: err = %v, allow = %v", tc.name, err, tc.allow)
		}
	}
	err := platformScope(identity.Principal{User: user}, permission.Permission{Object: "org/echoes"}, (&testv1.EchoRequest{}).ProtoReflect())
	wantCode(t, err, connect.CodePermissionDenied)
}

func TestResolveObjectRejectsBadAnnotations(t *testing.T) {
	m := (&testv1.GetEchoRequest{Echo: uuid.NewString()}).ProtoReflect()
	for _, pattern := range []string{"org/echoes/{missing}", "org/echoes/{echo", "org/echoes/echo}", "org/{{echo}}"} {
		if _, err := resolveObject(pattern, m); connect.CodeOf(err) != connect.CodeUnimplemented {
			t.Errorf("resolveObject(%q) err = %v, want Unimplemented", pattern, err)
		}
	}
}

func TestNewWatcherRequiresURLAndToken(t *testing.T) {
	for _, args := range [][2]string{{"", testToken}, {"https://hub", ""}} {
		if _, err := NewWatcher(args[0], args[1], nil); err == nil {
			t.Errorf("NewWatcher(%q, %q) succeeded", args[0], args[1])
		}
	}
}

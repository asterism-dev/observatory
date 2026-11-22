package hubserver

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"connectrpc.com/connect"
	"github.com/google/uuid"

	testv1 "github.com/asterism-dev/observatory/gen/go/observatory/test/v1"
	"github.com/asterism-dev/observatory/gen/go/observatory/test/v1/testv1connect"
	"github.com/asterism-dev/observatory/internal/authz"
	"github.com/asterism-dev/observatory/internal/edge"
	platformauthz "github.com/asterism-dev/observatory/internal/platform/authz"
	"github.com/asterism-dev/observatory/internal/platform/iamtest"
	"github.com/asterism-dev/observatory/internal/platform/identity"
)

type e2eEcho struct {
	testv1connect.UnimplementedEchoServiceHandler
	seen chan http.Header
}

func (e e2eEcho) GetEcho(_ context.Context, r *connect.Request[testv1.GetEchoRequest]) (*connect.Response[testv1.GetEchoResponse], error) {
	select {
	case e.seen <- r.Header().Clone():
	default:
	}
	return connect.NewResponse(&testv1.GetEchoResponse{}), nil
}

// TestIdentityEndToEnd runs iam's identity routes (the fake), the real edge and a service built on the platform libraries.
func TestIdentityEndToEnd(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	iam := iamtest.New(t)
	edgeTok := strings.Repeat("t", 32)
	v, err := identity.NewVerifier(iam.URL(), iamtest.Issuer, iam.ServiceToken(), nil)
	if err != nil {
		t.Fatal(err)
	}
	w, err := platformauthz.NewWatcher(iam.URL(), iam.ServiceToken(), nil)
	if err != nil {
		t.Fatal(err)
	}
	watchDone := make(chan struct{})
	go func() { w.Run(ctx); close(watchDone) }()
	defer func() { cancel(); <-watchDone }()
	echo := e2eEcho{seen: make(chan http.Header, 16)}
	svcMux := http.NewServeMux()
	path, h := testv1connect.NewEchoServiceHandler(echo, connect.WithInterceptors(platformauthz.NewInterceptor(v, w)))
	svcMux.Handle("/api/echo/v1"+path, http.StripPrefix("/api/echo/v1", h))
	svc := httptest.NewServer(svcMux)
	defer svc.Close()

	newEdge := func() *httptest.Server {
		h, err := edge.New(edge.Config{HubURL: iam.URL(), IAMURL: iam.URL(), Token: edgeTok, ServiceToken: iam.ServiceToken(), Services: map[string]string{"echo": svc.URL}}, fstest.MapFS{"index.html": {Data: []byte("app")}})
		if err != nil {
			t.Fatal(err)
		}
		srv := httptest.NewServer(h)
		t.Cleanup(srv.Close)
		return srv
	}

	alice, bob := iam.AddUser("alice"), iam.AddUser("bob")
	org := iam.CreateOrg(alice.ID, "acme")
	iam.AddMember(org, bob.ID, "member")
	iam.Bind(org, authz.UserSubject(bob.ID), authz.CustomRole{Name: "echo reader", Rules: []authz.Rule{{Effect: authz.Allow, Object: "org/echoes/*", Action: "test.echoes.get"}}})
	session, sess := iam.SessionToken(bob.ID, "cli")

	getEcho := func(e *httptest.Server) error {
		req := connect.NewRequest(&testv1.GetEchoRequest{Org: org.String(), Echo: uuid.NewString()})
		req.Header().Set("Authorization", "Bearer "+session)
		_, err := testv1connect.NewEchoServiceClient(e.Client(), e.URL+"/api/echo/v1").GetEcho(ctx, req)
		return err
	}
	until := func(want connect.Code, e *httptest.Server) {
		t.Helper()
		deadline := time.Now().Add(5 * time.Second)
		for {
			err := getEcho(e)
			if (want == 0 && err == nil) || (err != nil && connect.CodeOf(err) == want) {
				return
			}
			if time.Now().After(deadline) {
				t.Fatalf("got %v, want code %v", err, want)
			}
			time.Sleep(20 * time.Millisecond)
		}
	}

	front := newEdge()
	until(0, front)
	hdr := <-echo.seen
	if hdr.Get(edge.HeaderEdge) != "" || hdr.Get(edge.HeaderClientIP) != "127.0.0.1" || strings.Contains(hdr.Get("Authorization"), session) {
		t.Fatalf("service saw edge %q, client ip %q, or the session token", hdr.Get(edge.HeaderEdge), hdr.Get(edge.HeaderClientIP))
	}

	p, err := v.Verify(ctx, strings.TrimPrefix(hdr.Get("Authorization"), "Bearer "))
	if err != nil {
		t.Fatal(err)
	}
	if p.Name != "bob" || p.SessionKind != "cli" || p.Session != sess || time.Until(p.SessionExpires) < 50*time.Minute {
		t.Fatalf("service saw name %q, session kind %q, session %s expiring %v", p.Name, p.SessionKind, p.Session, p.SessionExpires)
	}

	iam.RemoveMember(org, bob.ID)
	until(connect.CodeNotFound, front)

	iam.RevokeSession(sess)
	// A fresh edge has no cached JWT, so the revocation shows at once.
	if err := getEcho(newEdge()); connect.CodeOf(err) != connect.CodeUnauthenticated {
		t.Fatalf("revoked session: %v", err)
	}
}

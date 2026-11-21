package hubserver

import (
	"context"
	"encoding/base64"
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
	"github.com/asterism-dev/observatory/internal/db/dbtest"
	"github.com/asterism-dev/observatory/internal/edge"
	platformauthz "github.com/asterism-dev/observatory/internal/platform/authz"
	"github.com/asterism-dev/observatory/internal/platform/identity"
	"github.com/asterism-dev/observatory/internal/seal"
	"github.com/asterism-dev/observatory/internal/store"
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

// TestIdentityEndToEnd runs the hub's identity routes, the real edge and a service built on the platform libraries.
func TestIdentityEndToEnd(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	edgeSvcTok, echoTok, edgeTok := strings.Repeat("e", 32), strings.Repeat("c", 32), strings.Repeat("t", 32)
	env := map[string]string{
		"OBSERVATORY_DATABASE_URL":       "postgres://unused",
		"OBSERVATORY_PUBLIC_URL":         "https://obs.example/",
		"OBSERVATORY_OIDC_ISSUER":        "https://idp.example",
		"OBSERVATORY_OIDC_WEB_CLIENT_ID": "web",
		"OBSERVATORY_OIDC_CLI_CLIENT_ID": "cli",
		"OBSERVATORY_SECRETS_KEY":        base64.StdEncoding.EncodeToString(make([]byte, 32)),
		"OBSERVATORY_GATEWAY_TOKEN":      strings.Repeat("g", 32),
		"OBSERVATORY_STORAGE_TOKEN":      strings.Repeat("s", 32),
		"OBSERVATORY_EDGE_TOKEN":         edgeTok,
		"OBSERVATORY_NATS_URL":           "nats://unused",
		"OBSERVATORY_NATS_TOKEN":         strings.Repeat("n", 32),
		"OBSERVATORY_SERVICE_TOKENS":     "edge=" + edgeSvcTok + ",echo=" + echoTok,
	}
	cfg, err := loadConfig(func(k string) string { return env[k] })
	if err != nil {
		t.Fatal(err)
	}
	s := store.New(dbtest.New(t)).WithMasterKey(seal.Key{1})
	z, err := authz.NewService(ctx, s.PolicySnapshot)
	if err != nil {
		t.Fatal(err)
	}
	hubMux := http.NewServeMux()
	if err := mountIdentity(ctx, hubMux, s, z, cfg); err != nil {
		t.Fatal(err)
	}
	hub := httptest.NewServer(hubMux)
	defer hub.Close()

	v, err := identity.NewVerifier(hub.URL, env["OBSERVATORY_PUBLIC_URL"], echoTok, nil)
	if err != nil {
		t.Fatal(err)
	}
	w, err := platformauthz.NewWatcher(hub.URL, echoTok, nil)
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
		h, err := edge.New(edge.Config{HubURL: hub.URL, Token: edgeTok, ServiceToken: edgeSvcTok, Services: map[string]string{"echo": svc.URL}}, fstest.MapFS{"index.html": {Data: []byte("app")}})
		if err != nil {
			t.Fatal(err)
		}
		srv := httptest.NewServer(h)
		t.Cleanup(srv.Close)
		return srv
	}

	alice, err := s.Login(ctx, store.Identity{Subject: "alice", Email: "alice@x.io", Name: "alice"})
	if err != nil {
		t.Fatal(err)
	}
	bob, err := s.Login(ctx, store.Identity{Subject: "bob", Email: "bob@x.io", Name: "bob"})
	if err != nil {
		t.Fatal(err)
	}
	org, err := s.CreateOrg(ctx, "acme", alice.User.ID)
	if err != nil {
		t.Fatal(err)
	}
	invite, _, err := s.CreateInvite(ctx, org.ID, store.RoleMember, alice.User.ID, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.AcceptInvite(ctx, invite, bob.User.ID); err != nil {
		t.Fatal(err)
	}
	role, err := s.CreateRole(ctx, org.ID, authz.CustomRole{Name: "echo reader", Rules: []authz.Rule{{Effect: authz.Allow, Object: "org/echoes/*", Action: "test.echoes.get"}}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Bind(ctx, org.ID, "user:"+bob.User.ID.String(), role.ID); err != nil {
		t.Fatal(err)
	}
	session, sess, err := s.CreateSession(ctx, bob.User.ID, store.SessionCLI, time.Hour, "test", "127.0.0.1")
	if err != nil {
		t.Fatal(err)
	}
	if err := z.Reload(ctx); err != nil {
		t.Fatal(err)
	}

	getEcho := func(e *httptest.Server) error {
		req := connect.NewRequest(&testv1.GetEchoRequest{Org: org.ID.String(), Echo: uuid.NewString()})
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

	if err := s.RemoveMember(ctx, org.ID, bob.User.ID); err != nil {
		t.Fatal(err)
	}
	if err := z.Reload(ctx); err != nil {
		t.Fatal(err)
	}
	until(connect.CodeNotFound, front)

	if err := s.RevokeSession(ctx, bob.User.ID, sess.ID); err != nil {
		t.Fatal(err)
	}
	// A fresh edge has no cached JWT, so the revocation shows at once.
	if err := getEcho(newEdge()); connect.CodeOf(err) != connect.CodeUnauthenticated {
		t.Fatalf("revoked session: %v", err)
	}
}

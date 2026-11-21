package auditlog

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync/atomic"
	"testing"
	"testing/fstest"
	"time"

	"connectrpc.com/connect"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/nats-io/nats-server/v2/server"
	"github.com/nats-io/nats.go"
	"google.golang.org/protobuf/proto"

	auditv1 "github.com/asterism-dev/observatory/gen/go/observatory/audit/v1"
	eventsv1 "github.com/asterism-dev/observatory/gen/go/observatory/events/v1"
	testv1 "github.com/asterism-dev/observatory/gen/go/observatory/test/v1"
	"github.com/asterism-dev/observatory/gen/go/observatory/test/v1/testv1connect"
	"github.com/asterism-dev/observatory/internal/db/dbtest"
	"github.com/asterism-dev/observatory/internal/platform/identity"
	"github.com/asterism-dev/observatory/internal/platform/outbox"
	"github.com/asterism-dev/observatory/internal/platform/pgsvc"
)

const natsToken = "0123456789abcdef0123456789abcdef"

// fakeSource serves whatever data-event set the test stores in it.
type fakeSource struct {
	enabled atomic.Pointer[[]*auditv1.DataEvents]
}

func (f *fakeSource) set(es ...*auditv1.DataEvents) { f.enabled.Store(&es) }

func (f *fakeSource) GetDataEvents(context.Context, *connect.Request[auditv1.GetDataEventsRequest]) (*connect.Response[auditv1.GetDataEventsResponse], error) {
	es := f.enabled.Load()
	if es == nil {
		return nil, connect.NewError(connect.CodeUnavailable, errors.New("not set"))
	}
	return connect.NewResponse(&auditv1.GetDataEventsResponse{Enabled: *es}), nil
}

type echoServer struct {
	testv1connect.UnimplementedEchoServiceHandler
}

// Echo fails with the code named by its text, if any.
func (echoServer) Echo(_ context.Context, r *connect.Request[testv1.EchoRequest]) (*connect.Response[testv1.EchoResponse], error) {
	if r.Msg.GetText() == "notfound" {
		return nil, connect.NewError(connect.CodeNotFound, errors.New("no such echo"))
	}
	return connect.NewResponse(&testv1.EchoResponse{}), nil
}

func (echoServer) GetEcho(context.Context, *connect.Request[testv1.GetEchoRequest]) (*connect.Response[testv1.GetEchoResponse], error) {
	return connect.NewResponse(&testv1.GetEchoResponse{}), nil
}

// fakeAuthz stands in for the authz interceptor: it verifies every caller and denies when asked to.
type fakeAuthz struct{ user uuid.UUID }

func (f fakeAuthz) WrapUnary(next connect.UnaryFunc) connect.UnaryFunc {
	return func(ctx context.Context, req connect.AnyRequest) (connect.AnyResponse, error) {
		if req.Header().Get("X-Anonymous") != "" {
			return nil, connect.NewError(connect.CodeUnauthenticated, errors.New("missing identity"))
		}
		inner := identity.WithVerified(ctx, identity.Principal{User: f.user}, "203.0.113.9")
		if req.Header().Get("X-Deny") != "" {
			identity.MarkDenied(ctx)
			return nil, connect.NewError(connect.CodePermissionDenied, errors.New("permission denied"))
		}
		return next(inner, req)
	}
}

func (fakeAuthz) WrapStreamingClient(next connect.StreamingClientFunc) connect.StreamingClientFunc {
	return next
}

func (fakeAuthz) WrapStreamingHandler(next connect.StreamingHandlerFunc) connect.StreamingHandlerFunc {
	return next
}

type env struct {
	pool *pgxpool.Pool
	echo testv1connect.EchoServiceClient
	src  *fakeSource
	set  *Settings
	user uuid.UUID
}

func newEnv(t *testing.T) *env {
	pool, err := pgsvc.Open(context.Background(), dbtest.Empty(t), "svc", outbox.Migrations(fstest.MapFS{}, 1))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	e := &env{pool: pool, src: &fakeSource{}, user: uuid.New()}
	e.src.set()
	e.set = NewSettingsFrom(e.src)
	mux := http.NewServeMux()
	mux.Handle(testv1connect.NewEchoServiceHandler(echoServer{},
		connect.WithInterceptors(NewInterceptor("test", e.set, pool), fakeAuthz{user: e.user})))
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	e.echo = testv1connect.NewEchoServiceClient(srv.Client(), srv.URL)
	return e
}

// events returns the outbox in write order; recording is synchronous, so it is complete once the call returned.
func (e *env) events(t *testing.T) []*eventsv1.Event {
	t.Helper()
	rows, err := e.pool.Query(context.Background(), "SELECT payload FROM outbox ORDER BY id")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []*eventsv1.Event
	for rows.Next() {
		var b []byte
		if err := rows.Scan(&b); err != nil {
			t.Fatal(err)
		}
		ev := &eventsv1.Event{}
		if err := proto.Unmarshal(b, ev); err != nil {
			t.Fatal(err)
		}
		out = append(out, ev)
	}
	return out
}

func call[T any](t *testing.T, msg *T, headers ...string) *connect.Request[T] {
	t.Helper()
	r := connect.NewRequest(msg)
	for _, h := range headers {
		r.Header().Set(h, "1")
	}
	return r
}

func TestInterceptorRecords(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	org := uuid.NewString()
	read := func(headers ...string) error {
		_, err := e.echo.GetEcho(ctx, call(t, &testv1.GetEchoRequest{Org: org, Echo: uuid.NewString()}, headers...))
		return err
	}

	if _, err := e.echo.Echo(ctx, call(t, &testv1.EchoRequest{Text: "hi"})); err != nil {
		t.Fatal(err)
	}
	if err := read(); err != nil {
		t.Fatal(err)
	}
	if _, err := e.echo.Echo(ctx, call(t, &testv1.EchoRequest{Text: "notfound"})); connect.CodeOf(err) != connect.CodeNotFound {
		t.Fatalf("handler not found: %v", err)
	}
	if err := read("X-Deny"); connect.CodeOf(err) != connect.CodePermissionDenied {
		t.Fatalf("denied read: %v", err)
	}
	if err := read("X-Anonymous"); connect.CodeOf(err) != connect.CodeUnauthenticated {
		t.Fatalf("anonymous read: %v", err)
	}
	if _, err := e.echo.Echo(ctx, call(t, &testv1.EchoRequest{}, "X-Anonymous")); connect.CodeOf(err) != connect.CodeUnauthenticated {
		t.Fatalf("anonymous write: %v", err)
	}
	e.src.set(&auditv1.DataEvents{Org: org, Service: "test"})
	if err := e.set.load(ctx); err != nil {
		t.Fatal(err)
	}
	if err := read(); err != nil {
		t.Fatal(err)
	}

	type row struct {
		typ, org string
		result   eventsv1.Result
	}
	want := []row{
		{"test.echoes.create", "", eventsv1.Result_RESULT_ALLOWED},
		// A handler's not found is a failure, not a denial.
		{"test.echoes.create", "", eventsv1.Result_RESULT_FAILED},
		{"test.echoes.get", org, eventsv1.Result_RESULT_DENIED},
		{"test.echoes.get", org, eventsv1.Result_RESULT_ALLOWED},
	}
	got := e.events(t)
	if len(got) != len(want) {
		t.Fatalf("recorded %v, want %v", got, want)
	}
	for i, ev := range got {
		if (row{ev.GetType(), ev.GetOrg(), ev.GetResult()}) != want[i] || ev.GetActor().GetId() != e.user.String() || ev.GetClientIp() != "203.0.113.9" || !ev.GetAudit() {
			t.Errorf("event %d = %v, want %v", i, ev, want[i])
		}
	}
}

func TestInterceptorSanitizesUserAgent(t *testing.T) {
	e := newEnv(t)
	req := call(t, &testv1.EchoRequest{Text: "hi"})
	req.Header().Set("User-Agent", "x\xff")
	if _, err := e.echo.Echo(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	got := e.events(t)
	if len(got) != 1 || got[0].GetUserAgent() != "x" {
		t.Fatalf("recorded %v, want one event with user agent \"x\"", got)
	}
}

func startNATS(t *testing.T, dir string, port int) *server.Server {
	t.Helper()
	s, err := server.NewServer(&server.Options{Host: "127.0.0.1", Port: port, JetStream: true, StoreDir: dir, Authorization: natsToken, NoSigs: true, NoLog: true})
	if err != nil {
		t.Fatal(err)
	}
	go s.Start()
	if !s.ReadyForConnections(10 * time.Second) {
		t.Fatal("nats not ready")
	}
	return s
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestSettingsReloadAfterReconnect(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := l.Addr().(*net.TCPAddr).Port
	l.Close()
	dir := t.TempDir()
	ns := startNATS(t, dir, port)
	var ownHandler atomic.Bool
	nc, err := nats.Connect("nats://127.0.0.1:"+strconv.Itoa(port), nats.Token(natsToken), nats.MaxReconnects(-1), nats.ReconnectWait(50*time.Millisecond),
		nats.ReconnectHandler(func(*nats.Conn) { ownHandler.Store(true) }))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(nc.Close)

	org := uuid.NewString()
	src := &fakeSource{}
	src.set(&auditv1.DataEvents{Org: org, Service: "compute"})
	s := NewSettingsFrom(src)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { s.Run(ctx, nc); close(done) }()
	t.Cleanup(func() { cancel(); <-done })
	waitFor(t, "first load", func() bool { return s.DataEvents(org, "compute") })

	// A change while the bus is down never reaches the subscription; only the reconnect can pick it up.
	ns.Shutdown()
	ns.WaitForShutdown()
	src.set()
	ns = startNATS(t, dir, port)
	t.Cleanup(ns.Shutdown)
	waitFor(t, "reload after reconnect", func() bool { return !s.DataEvents(org, "compute") })
	// Run must not replace the connection's own reconnect handler.
	waitFor(t, "the connection's reconnect handler", ownHandler.Load)
}

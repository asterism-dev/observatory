package audit

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	"connectrpc.com/connect"
	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/nats-io/nats.go/jetstream"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/structpb"
	"google.golang.org/protobuf/types/known/timestamppb"

	auditv1 "github.com/asterism-dev/observatory/gen/go/observatory/audit/v1"
	"github.com/asterism-dev/observatory/gen/go/observatory/audit/v1/auditv1connect"
	eventsv1 "github.com/asterism-dev/observatory/gen/go/observatory/events/v1"
	iamv1 "github.com/asterism-dev/observatory/gen/go/observatory/iam/v1"
	"github.com/asterism-dev/observatory/gen/go/observatory/iam/v1/iamv1connect"
	"github.com/asterism-dev/observatory/internal/db/dbtest"
	"github.com/asterism-dev/observatory/internal/platform/bus"
	"github.com/asterism-dev/observatory/internal/platform/pgsvc"
)

const (
	testIssuer = "https://obs.example"
	testToken  = "audit-service-token-audit-service-token"
	computeToken = "compute-service-token-compute-service-token"
)

func openStore(t *testing.T, dbURL string) *store {
	t.Helper()
	m, err := migrations()
	if err != nil {
		t.Fatal(err)
	}
	pool, err := pgsvc.Open(context.Background(), dbURL, "audit", m)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	return &store{pool: pool}
}

func startBus(t *testing.T) (url, token string, js jetstream.JetStream) {
	t.Helper()
	url, token, stop, err := bus.Embedded(context.Background(), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(stop)
	js, closeConn, err := bus.Connect(context.Background(), url, token)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(closeConn)
	if err := bus.EnsureStream(context.Background(), js); err != nil {
		t.Fatal(err)
	}
	return url, token, js
}

func newEvent(org string, at time.Time) *eventsv1.Event {
	id, _ := uuid.NewV7()
	details, _ := structpb.NewStruct(map[string]any{"name": "acme"})
	return &eventsv1.Event{
		Id: id.String(), Time: timestamppb.New(at.Truncate(time.Microsecond)), Type: "iam.members.delete", Org: org,
		Actor:    &eventsv1.Actor{Kind: eventsv1.ActorKind_ACTOR_KIND_USER, Id: uuid.NewString()},
		Resource: "org/members", Result: eventsv1.Result_RESULT_ALLOWED, ClientIp: "203.0.113.7",
		UserAgent: "test", RequestId: "req-1", Details: details, Audit: true,
	}
}

func publish(t *testing.T, js jetstream.JetStream, e *eventsv1.Event) {
	t.Helper()
	b, err := proto.Marshal(e)
	if err != nil {
		t.Fatal(err)
	}
	scope := e.GetOrg()
	if scope == "" {
		scope = "platform"
	}
	// A fresh message id each time, so JetStream's own de-duplication cannot hide a redelivery.
	if _, err := js.Publish(context.Background(), "events."+scope+"."+e.GetType(), b, jetstream.WithMsgID(uuid.NewString())); err != nil {
		t.Fatal(err)
	}
}

func count(t *testing.T, pool *pgxpool.Pool, id string) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(context.Background(), "SELECT count(*) FROM events WHERE id = $1", id).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
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

func startConsumer(t *testing.T, js jetstream.JetStream, s *store) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- consume(ctx, js, s) }()
	t.Cleanup(func() {
		cancel()
		if err := <-done; err != nil {
			t.Error(err)
		}
	})
}

func ackedAll(t *testing.T, js jetstream.JetStream) {
	t.Helper()
	ctx := context.Background()
	waitFor(t, "consumer", func() bool { _, err := js.Consumer(ctx, bus.Stream, consumerName); return err == nil })
	cons, err := js.Consumer(ctx, bus.Stream, consumerName)
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, "every delivery acked", func() bool {
		info, err := cons.Info(ctx)
		return err == nil && info.NumAckPending == 0 && info.NumPending == 0
	})
}

func TestConsumerStoresAuditEventsOnce(t *testing.T) {
	s := openStore(t, dbtest.Empty(t))
	_, _, js := startBus(t)
	startConsumer(t, js, s)

	org := uuid.NewString()
	dup := newEvent(org, time.Now())
	publish(t, js, dup)
	publish(t, js, dup)
	quiet := newEvent(org, time.Now())
	quiet.Audit = false
	publish(t, js, quiet)
	platform := newEvent("", time.Now())
	publish(t, js, platform)

	// Delivery is in stream order, so the last event stored means every earlier one was handled.
	waitFor(t, "platform event", func() bool { return count(t, s.pool, platform.GetId()) == 1 })
	// A redelivery must be acked, not retried forever against the primary key.
	ackedAll(t, js)
	if n := count(t, s.pool, dup.GetId()); n != 1 {
		t.Fatalf("duplicate delivery stored %d rows, want 1", n)
	}
	if n := count(t, s.pool, quiet.GetId()); n != 0 {
		t.Fatalf("non-audit event stored %d rows", n)
	}

	got, _, err := s.list(context.Background(), listQuery{org: org, pageSize: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || !proto.Equal(got[0], dup) {
		t.Fatalf("stored %v, want %v", got, dup)
	}
}

func TestConsumerRetriesTransientErrors(t *testing.T) {
	ctx := context.Background()
	s := openStore(t, dbtest.Empty(t))
	_, _, js := startBus(t)
	for _, stmt := range []string{
		"CREATE FUNCTION fail_insert() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'database down'; END $$",
		"CREATE TRIGGER fail_insert BEFORE INSERT ON events FOR EACH ROW EXECUTE FUNCTION fail_insert()",
	} {
		if _, err := s.pool.Exec(ctx, stmt); err != nil {
			t.Fatal(err)
		}
	}
	startConsumer(t, js, s)
	e := newEvent(uuid.NewString(), time.Now())
	publish(t, js, e)

	waitFor(t, "consumer", func() bool { _, err := js.Consumer(ctx, bus.Stream, consumerName); return err == nil })
	cons, err := js.Consumer(ctx, bus.Stream, consumerName)
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, "a redelivery", func() bool {
		info, err := cons.Info(ctx)
		return err == nil && info.NumRedelivered > 0
	})
	if _, err := s.pool.Exec(ctx, "DROP TRIGGER fail_insert ON events"); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "event stored after the failure cleared", func() bool { return count(t, s.pool, e.GetId()) == 1 })
}

func TestConsumerDropsUnstorableEvents(t *testing.T) {
	s := openStore(t, dbtest.Empty(t))
	_, _, js := startBus(t)
	startConsumer(t, js, s)
	bad := newEvent(uuid.NewString(), time.Now())
	// Postgres text cannot hold NUL: a data exception no redelivery fixes.
	bad.Resource = "org/\x00members"
	publish(t, js, bad)
	next := newEvent(bad.GetOrg(), time.Now())
	publish(t, js, next)

	waitFor(t, "next event", func() bool { return count(t, s.pool, next.GetId()) == 1 })
	ackedAll(t, js)
	if n := count(t, s.pool, bad.GetId()); n != 0 {
		t.Fatalf("unstorable event stored %d rows", n)
	}
}

func TestConsumeFailsWhenConsumerCloses(t *testing.T) {
	s := openStore(t, dbtest.Empty(t))
	_, _, js := startBus(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- consume(ctx, js, s) }()
	waitFor(t, "consumer", func() bool { _, err := js.Consumer(ctx, bus.Stream, consumerName); return err == nil })
	if err := js.DeleteConsumer(ctx, bus.Stream, consumerName); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("consume returned nil after its consumer was deleted")
		}
	case <-time.After(15 * time.Second):
		t.Fatal("consume kept running after its consumer was deleted")
	}
}

func TestEventsAreAppendOnly(t *testing.T) {
	ctx := context.Background()
	u, err := url.Parse(dbtest.Empty(t))
	if err != nil {
		t.Fatal(err)
	}
	q := u.Query()
	// One connection, so the DELETE after retention runs on the connection that had the setting.
	q.Set("pool_max_conns", "1")
	u.RawQuery = q.Encode()
	s := openStore(t, u.String())
	old := newEvent(uuid.NewString(), time.Now().Add(-400*24*time.Hour))
	fresh := newEvent(old.GetOrg(), time.Now())
	for _, e := range []*eventsv1.Event{old, fresh} {
		if err := s.insert(ctx, e); err != nil {
			t.Fatal(err)
		}
	}
	for _, stmt := range []string{"UPDATE events SET resource = 'x'", "DELETE FROM events", "TRUNCATE events"} {
		if _, err := s.pool.Exec(ctx, stmt); err == nil {
			t.Errorf("%s succeeded", stmt)
		}
	}

	n, err := s.deleteBefore(ctx, time.Now().Add(-365*24*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 || count(t, s.pool, old.GetId()) != 0 || count(t, s.pool, fresh.GetId()) != 1 {
		t.Fatalf("retention deleted %d rows; old left %d, fresh left %d", n, count(t, s.pool, old.GetId()), count(t, s.pool, fresh.GetId()))
	}
	// The retention setting is transaction-local and must not leak to later statements on the connection.
	if _, err := s.pool.Exec(ctx, "DELETE FROM events"); err == nil {
		t.Fatal("DELETE succeeded after retention")
	}
}

type staticPolicy struct {
	iamv1connect.UnimplementedPolicyServiceHandler
	policy *iamv1.Policy
}

func (p staticPolicy) WatchPolicy(ctx context.Context, _ *connect.Request[iamv1.WatchPolicyRequest], stream *connect.ServerStream[iamv1.Policy]) error {
	if err := stream.Send(p.policy); err != nil {
		return err
	}
	<-ctx.Done()
	return nil
}

type fixture struct {
	key                     ed25519.PrivateKey
	client                  auditv1connect.AuditServiceClient
	store                   *store
	baseURL                 string
	natsURL, natsToken      string
	orgA, orgB              string
	admin, member, adminB   uuid.UUID
	platformAdmin, outsider uuid.UUID
}

// newFixture runs the real service against a fake hub: admin is admin of orgA, member a plain member of orgA, adminB admin of orgB.
func newFixture(t *testing.T) *fixture {
	f := &fixture{orgA: uuid.NewString(), orgB: uuid.NewString(), admin: uuid.New(), member: uuid.New(), adminB: uuid.New(), platformAdmin: uuid.New(), outsider: uuid.New()}
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	f.key = priv
	policy := &iamv1.Policy{Revision: 1, Memberships: []*iamv1.MemberRole{
		{OrgId: f.orgA, UserId: f.admin.String(), Role: "admin"},
		{OrgId: f.orgA, UserId: f.member.String(), Role: "member"},
		{OrgId: f.orgB, UserId: f.adminB.String(), Role: "admin"},
	}}
	mux := http.NewServeMux()
	mux.HandleFunc("/internal/iam/jwks", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"keys": []map[string]string{{"kty": "OKP", "crv": "Ed25519", "kid": "k1", "x": base64.RawURLEncoding.EncodeToString(pub)}}})
	})
	path, h := iamv1connect.NewPolicyServiceHandler(staticPolicy{policy: policy})
	mux.Handle("/internal/iam"+path, http.StripPrefix("/internal/iam", h))
	hub := httptest.NewServer(mux)
	t.Cleanup(hub.Close)

	dbURL := dbtest.Empty(t)
	f.store = openStore(t, dbURL)
	natsURL, natsToken, _ := startBus(t)
	f.natsURL, f.natsToken = natsURL, natsToken
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	cfg := Config{DatabaseURL: dbURL, HubURL: hub.URL, Issuer: testIssuer, ServiceToken: testToken, NATSURL: natsURL, NATSToken: natsToken,
		ServiceTokens: map[string]string{"compute": computeToken}}
	go func() { done <- Run(ctx, cfg, ln) }()
	t.Cleanup(func() {
		cancel()
		if err := <-done; err != nil {
			t.Error(err)
		}
	})
	f.baseURL = "http://" + ln.Addr().String()
	f.client = auditv1connect.NewAuditServiceClient(http.DefaultClient, f.baseURL+"/api/audit/v1")
	return f
}

func (f *fixture) token(t *testing.T, user uuid.UUID, platformAdmin bool) string {
	t.Helper()
	now := time.Now()
	tok := jwt.NewWithClaims(jwt.SigningMethodEdDSA, jwt.MapClaims{
		"iss": testIssuer, "aud": []string{"observatory"}, "sub": user.String(), "sid": uuid.NewString(), "pa": platformAdmin,
		"iat": now.Unix(), "nbf": now.Unix(), "exp": now.Add(5 * time.Minute).Unix(),
	})
	tok.Header["kid"] = "k1"
	s, err := tok.SignedString(f.key)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func (f *fixture) list(t *testing.T, user uuid.UUID, req *auditv1.ListEventsRequest) (*auditv1.ListEventsResponse, error) {
	t.Helper()
	r := connect.NewRequest(req)
	r.Header().Set("Authorization", "Bearer "+f.token(t, user, false))
	var resp *connect.Response[auditv1.ListEventsResponse]
	var err error
	// The policy snapshot arrives asynchronously after start.
	waitFor(t, "policy snapshot", func() bool {
		resp, err = f.client.ListEvents(context.Background(), r)
		return connect.CodeOf(err) != connect.CodeUnavailable
	})
	if err != nil {
		return nil, err
	}
	return resp.Msg, nil
}

func (f *fixture) listAll(t *testing.T, user uuid.UUID, platformAdmin bool, req *auditv1.ListAllEventsRequest) (*auditv1.ListAllEventsResponse, error) {
	t.Helper()
	r := connect.NewRequest(req)
	r.Header().Set("Authorization", "Bearer "+f.token(t, user, platformAdmin))
	resp, err := f.client.ListAllEvents(context.Background(), r)
	if err != nil {
		return nil, err
	}
	return resp.Msg, nil
}

// seed stores n events for org at one-minute steps back from base, newest first.
func (f *fixture) seed(t *testing.T, org string, n int, base time.Time) []*eventsv1.Event {
	t.Helper()
	var es []*eventsv1.Event
	for i := range n {
		e := newEvent(org, base.Add(-time.Duration(i)*time.Minute))
		if err := f.store.insert(context.Background(), e); err != nil {
			t.Fatal(err)
		}
		es = append(es, e)
	}
	return es
}

var seededType = &auditv1.EventFilter{Type: "iam.members.delete"}

func ids(es []*eventsv1.Event) []string {
	var out []string
	for _, e := range es {
		out = append(out, e.GetId())
	}
	return out
}

func wantIDs(t *testing.T, got, want []*eventsv1.Event) {
	t.Helper()
	g, w := ids(got), ids(want)
	if len(g) != len(w) {
		t.Fatalf("got %v, want %v", g, w)
	}
	for i := range g {
		if g[i] != w[i] {
			t.Fatalf("got %v, want %v", g, w)
		}
	}
}

func wantCode(t *testing.T, err error, want connect.Code) {
	t.Helper()
	if connect.CodeOf(err) != want {
		t.Fatalf("err = %v, want %v", err, want)
	}
}

func TestListEventsPaginates(t *testing.T) {
	f := newFixture(t)
	base := time.Now().Truncate(time.Second)
	seeded := f.seed(t, f.orgA, 5, base)
	// Same time as the newest: the id breaks the tie, so no row is skipped or repeated across pages.
	tie := newEvent(f.orgA, base)
	if err := f.store.insert(context.Background(), tie); err != nil {
		t.Fatal(err)
	}
	want := append([]*eventsv1.Event{tie}, seeded...)

	var got []*eventsv1.Event
	token := ""
	for page := 0; ; page++ {
		resp, err := f.list(t, f.admin, &auditv1.ListEventsRequest{Org: f.orgA, PageSize: 2, PageToken: token})
		if err != nil {
			t.Fatal(err)
		}
		if len(resp.GetEvents()) > 2 {
			t.Fatalf("page %d has %d events", page, len(resp.GetEvents()))
		}
		got = append(got, resp.GetEvents()...)
		token = resp.GetNextPageToken()
		if token == "" {
			break
		}
		if page > 5 {
			t.Fatal("pagination does not end")
		}
	}
	wantIDs(t, got, want)

	resp, err := f.list(t, f.admin, &auditv1.ListEventsRequest{Org: f.orgA})
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.GetEvents()) != 6 || resp.GetNextPageToken() != "" {
		t.Fatalf("default page: %d events, token %q", len(resp.GetEvents()), resp.GetNextPageToken())
	}
	if _, err := f.list(t, f.admin, &auditv1.ListEventsRequest{Org: f.orgA, PageToken: "garbage"}); connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Fatalf("bad page token: %v", err)
	}
}

func TestListEventsFilters(t *testing.T) {
	f := newFixture(t)
	base := time.Now().Truncate(time.Second)
	var es []*eventsv1.Event
	for i := range 4 {
		es = append(es, newEvent(f.orgA, base.Add(-time.Duration(i)*time.Minute)))
	}
	es[1].Type = "iam.members.setRole"
	es[2].Result = eventsv1.Result_RESULT_DENIED
	for _, e := range es {
		if err := f.store.insert(context.Background(), e); err != nil {
			t.Fatal(err)
		}
	}
	for name, tc := range map[string]struct {
		filter *auditv1.EventFilter
		want   []*eventsv1.Event
	}{
		"actor":  {&auditv1.EventFilter{ActorId: es[3].GetActor().GetId()}, es[3:4]},
		"type":   {&auditv1.EventFilter{Type: "iam.members.setRole"}, es[1:2]},
		"result": {&auditv1.EventFilter{Result: eventsv1.Result_RESULT_DENIED}, es[2:3]},
		// from inclusive, to exclusive.
		"range": {&auditv1.EventFilter{From: es[2].GetTime(), To: es[0].GetTime()}, es[1:3]},
	} {
		t.Run(name, func(t *testing.T) {
			resp, err := f.list(t, f.admin, &auditv1.ListEventsRequest{Org: f.orgA, Filter: tc.filter})
			if err != nil {
				t.Fatal(err)
			}
			wantIDs(t, resp.GetEvents(), tc.want)
		})
	}
}

func TestListEventsAuthorization(t *testing.T) {
	f := newFixture(t)
	now := time.Now()
	a := f.seed(t, f.orgA, 1, now)
	b := f.seed(t, f.orgB, 1, now)

	_, err := f.list(t, f.member, &auditv1.ListEventsRequest{Org: f.orgA})
	wantCode(t, err, connect.CodePermissionDenied)
	_, err = f.list(t, f.outsider, &auditv1.ListEventsRequest{Org: f.orgA})
	wantCode(t, err, connect.CodeNotFound)
	_, err = f.list(t, f.adminB, &auditv1.ListEventsRequest{Org: f.orgA})
	wantCode(t, err, connect.CodeNotFound)

	for _, tc := range []struct {
		user uuid.UUID
		org  string
		want []*eventsv1.Event
	}{{f.admin, f.orgA, a}, {f.adminB, f.orgB, b}} {
		// The denials above are audited too; the type filter keeps only the seeded events.
		resp, err := f.list(t, tc.user, &auditv1.ListEventsRequest{Org: tc.org, Filter: seededType})
		if err != nil {
			t.Fatal(err)
		}
		wantIDs(t, resp.GetEvents(), tc.want)
	}
}

func TestListAllEventsPlatformAdminOnly(t *testing.T) {
	f := newFixture(t)
	now := time.Now()
	a := f.seed(t, f.orgA, 1, now)
	b := f.seed(t, f.orgB, 1, now.Add(-time.Minute))
	p := f.seed(t, "", 1, now.Add(-2*time.Minute))
	// Waits for the policy snapshot, which ListAllEvents does not depend on but the interceptor does.
	if _, err := f.list(t, f.admin, &auditv1.ListEventsRequest{Org: f.orgA}); err != nil {
		t.Fatal(err)
	}

	_, err := f.listAll(t, f.admin, false, &auditv1.ListAllEventsRequest{})
	wantCode(t, err, connect.CodePermissionDenied)

	resp, err := f.listAll(t, f.platformAdmin, true, &auditv1.ListAllEventsRequest{Filter: seededType})
	if err != nil {
		t.Fatal(err)
	}
	wantIDs(t, resp.GetEvents(), []*eventsv1.Event{a[0], b[0], p[0]})

	resp, err = f.listAll(t, f.platformAdmin, true, &auditv1.ListAllEventsRequest{FilterOrg: f.orgB})
	if err != nil {
		t.Fatal(err)
	}
	wantIDs(t, resp.GetEvents(), b)
	_, err = f.listAll(t, f.platformAdmin, true, &auditv1.ListAllEventsRequest{FilterOrg: "nope"})
	wantCode(t, err, connect.CodeInvalidArgument)
}

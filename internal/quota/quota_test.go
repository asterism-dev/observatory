package quota

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"connectrpc.com/connect"
	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
	"google.golang.org/protobuf/proto"

	auditv1 "github.com/asterism-dev/observatory/gen/go/observatory/audit/v1"
	"github.com/asterism-dev/observatory/gen/go/observatory/audit/v1/auditv1connect"
	eventsv1 "github.com/asterism-dev/observatory/gen/go/observatory/events/v1"
	iamv1 "github.com/asterism-dev/observatory/gen/go/observatory/iam/v1"
	"github.com/asterism-dev/observatory/gen/go/observatory/iam/v1/iamv1connect"
	quotav1 "github.com/asterism-dev/observatory/gen/go/observatory/quota/v1"
	"github.com/asterism-dev/observatory/gen/go/observatory/quota/v1/quotav1connect"
	coreauthz "github.com/asterism-dev/observatory/internal/authz"
	"github.com/asterism-dev/observatory/internal/db/dbtest"
	"github.com/asterism-dev/observatory/internal/platform/authz"
	"github.com/asterism-dev/observatory/internal/platform/bus"
	"github.com/asterism-dev/observatory/internal/platform/catalog"
	"github.com/asterism-dev/observatory/internal/platform/clientip"
	"github.com/asterism-dev/observatory/internal/platform/pgsvc"
	"github.com/asterism-dev/observatory/internal/platform/servicetoken"
)

const (
	testIssuer = "https://obs.example"
	testToken  = "quota-service-token-quota-service-token"
	hubToken   = "hub-service-token-hub-service-token-hub"
)

func i32(n int32) *int32 { return &n }

func openStore(t *testing.T, dbURL string) *store {
	t.Helper()
	m, err := migrations()
	if err != nil {
		t.Fatal(err)
	}
	pool, err := pgsvc.Open(context.Background(), dbURL, "quota", m)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	return &store{pool: pool}
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

func wantCode(t *testing.T, err error, want connect.Code) {
	t.Helper()
	if connect.CodeOf(err) != want {
		t.Fatalf("err = %v, want %v", err, want)
	}
}

// fakeAudit serves the data events the test sets; none until then.
type fakeAudit struct {
	auditv1connect.UnimplementedAuditInternalServiceHandler
	enabled atomic.Pointer[[]*auditv1.DataEvents]
}

func (f *fakeAudit) set(es ...*auditv1.DataEvents) { f.enabled.Store(&es) }

func (f *fakeAudit) GetDataEvents(context.Context, *connect.Request[auditv1.GetDataEventsRequest]) (*connect.Response[auditv1.GetDataEventsResponse], error) {
	var es []*auditv1.DataEvents
	if p := f.enabled.Load(); p != nil {
		es = *p
	}
	return connect.NewResponse(&auditv1.GetDataEventsResponse{Enabled: es}), nil
}

// fakeIAM serves the signing key, the policy stream and RegisterCatalog; a held catalog stays out of the stream until releaseCatalog.
type fakeIAM struct {
	pub *authz.Publisher

	mu         sync.Mutex
	pol        coreauthz.Policy
	hold       bool
	registered []*iamv1.Catalog
	auths      []string
}

func (f *fakeIAM) RegisterCatalog(_ context.Context, req *connect.Request[iamv1.RegisterCatalogRequest]) (*connect.Response[iamv1.RegisterCatalogResponse], error) {
	c, err := catalog.FromProto(req.Msg.GetCatalog())
	if err != nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, err)
	}
	if err := catalog.Validate(c, catalog.AllowedPrefixes("quota")); err != nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.registered = append(f.registered, req.Msg.GetCatalog())
	f.auths = append(f.auths, req.Header().Get("Authorization"))
	if !f.hold {
		f.pol.Catalogs = []coreauthz.Catalog{c}
		f.pub.Publish(f.pol)
	}
	return connect.NewResponse(&iamv1.RegisterCatalogResponse{Digest: catalog.Digest(c)}), nil
}

func (f *fakeIAM) releaseCatalog(t *testing.T) {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	c, err := catalog.FromProto(f.registered[len(f.registered)-1])
	if err != nil {
		t.Fatal(err)
	}
	f.hold = false
	f.pol.Catalogs = []coreauthz.Catalog{c}
	f.pub.Publish(f.pol)
}

type fixture struct {
	key                     ed25519.PrivateKey
	client                  quotav1connect.QuotaServiceClient
	store                   *store
	iam                     *fakeIAM
	baseURL                 string
	audit                   *fakeAudit
	natsURL, natsToken      string
	orgA, orgB              uuid.UUID
	teamA, teamB            uuid.UUID
	admin, member, adminB   uuid.UUID
	platformAdmin, outsider uuid.UUID
}

func newFixture(t *testing.T) *fixture { return startFixture(t, false) }

// startFixture runs the real service against a fake iam: admin and member are in orgA with team teamA, adminB is admin of orgB with teamB.
func startFixture(t *testing.T, holdCatalog bool) *fixture {
	f := &fixture{orgA: uuid.New(), orgB: uuid.New(), teamA: uuid.New(), teamB: uuid.New(), admin: uuid.New(), member: uuid.New(), adminB: uuid.New(), platformAdmin: uuid.New(), outsider: uuid.New()}
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	f.key = priv
	f.iam = &fakeIAM{pub: authz.NewPublisher(), hold: holdCatalog, pol: coreauthz.Policy{
		Memberships: []coreauthz.MemberRole{
			{OrgID: f.orgA, UserID: f.admin, Role: "admin"},
			{OrgID: f.orgA, UserID: f.member, Role: "member"},
			{OrgID: f.orgB, UserID: f.adminB, Role: "admin"},
		},
		Teams: []coreauthz.TeamRef{{OrgID: f.orgA, ID: f.teamA}, {OrgID: f.orgB, ID: f.teamB}},
	}}
	f.iam.pub.Publish(f.iam.pol)
	mux := http.NewServeMux()
	mux.HandleFunc("/internal/iam/jwks", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"keys": []map[string]string{{"kty": "OKP", "crv": "Ed25519", "kid": "k1", "x": base64.RawURLEncoding.EncodeToString(pub)}}})
	})
	mux.Handle(f.iam.pub.Handler("/internal/iam"))
	path, h := iamv1connect.NewCatalogInternalServiceHandler(f.iam)
	mux.Handle("/internal/iam"+path, http.StripPrefix("/internal/iam", h))
	iam := httptest.NewServer(mux)
	t.Cleanup(iam.Close)
	f.audit = &fakeAudit{}
	apath, ah := auditv1connect.NewAuditInternalServiceHandler(f.audit)
	amux := http.NewServeMux()
	amux.Handle("/internal/audit"+apath, http.StripPrefix("/internal/audit", servicetoken.Middleware(map[string]string{"quota": testToken})(ah)))
	auditSrv := httptest.NewServer(amux)
	t.Cleanup(auditSrv.Close)

	dbURL := dbtest.Empty(t)
	f.store = openStore(t, dbURL)
	natsURL, natsToken, stop, err := bus.Embedded(context.Background(), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(stop)
	f.natsURL, f.natsToken = natsURL, natsToken
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	cfg := Config{DatabaseURL: dbURL, IAMURL: iam.URL, AuditURL: auditSrv.URL, Issuer: testIssuer, ServiceToken: testToken, NATSURL: natsURL, NATSToken: natsToken,
		ServiceTokens: map[string]string{"hub": hubToken}}
	go func() { done <- Run(ctx, cfg, ln) }()
	t.Cleanup(func() {
		cancel()
		if err := <-done; err != nil {
			t.Error(err)
		}
	})
	f.baseURL = "http://" + ln.Addr().String()
	f.client = quotav1connect.NewQuotaServiceClient(http.DefaultClient, f.baseURL+"/api/quota/v1")
	if !holdCatalog {
		f.ready(t)
	}
	return f
}

// ready waits until the service answers with its catalog in the policy.
func (f *fixture) ready(t *testing.T) {
	t.Helper()
	waitFor(t, "policy snapshot", func() bool {
		_, err := f.client.GetLimits(context.Background(), as(t, f, f.member, false, &quotav1.GetLimitsRequest{Org: f.orgA.String()}))
		return connect.CodeOf(err) != connect.CodeUnavailable
	})
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

func as[T any](t *testing.T, f *fixture, user uuid.UUID, platformAdmin bool, msg *T) *connect.Request[T] {
	t.Helper()
	r := connect.NewRequest(msg)
	r.Header().Set("Authorization", "Bearer "+f.token(t, user, platformAdmin))
	r.Header().Set(clientip.HeaderClientIP, "198.51.100.4")
	return r
}

func (f *fixture) setScope(t *testing.T, user uuid.UUID, scope string, l *quotav1.Limits) error {
	t.Helper()
	_, err := f.client.UpdateLimits(context.Background(), as(t, f, user, false, &quotav1.UpdateLimitsRequest{Org: f.orgA.String(), Scope: scope, Limits: l}))
	return err
}

func (f *fixture) setOrg(t *testing.T, l *quotav1.Limits) error {
	t.Helper()
	_, err := f.client.UpdateOrgLimits(context.Background(), as(t, f, f.platformAdmin, true, &quotav1.UpdateOrgLimitsRequest{OrgId: f.orgA.String(), Limits: l}))
	return err
}

func (f *fixture) rest(t *testing.T, method, path string, user uuid.UUID, platformAdmin bool, body string) (int, string) {
	t.Helper()
	req, err := http.NewRequest(method, f.baseURL+"/api/quota/v1"+path, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+f.token(t, user, platformAdmin))
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

// events returns quota's outbox in write order; the interceptor writes before the call returns.
func (f *fixture) events(t *testing.T, typ string) []*eventsv1.Event {
	t.Helper()
	rows, err := f.store.pool.Query(context.Background(), "SELECT payload FROM outbox ORDER BY id")
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
		e := &eventsv1.Event{}
		if err := proto.Unmarshal(b, e); err != nil {
			t.Fatal(err)
		}
		if e.GetType() == typ {
			out = append(out, e)
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

func TestGetLimitsShape(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	if err := f.setOrg(t, &quotav1.Limits{Vcpu: i32(10), DiskGb: i32(100), ImageStorageGb: i32(20)}); err != nil {
		t.Fatal(err)
	}
	if err := f.setScope(t, f.admin, "team:"+f.teamA.String(), &quotav1.Limits{Vcpu: i32(4)}); err != nil {
		t.Fatal(err)
	}
	if err := f.setScope(t, f.admin, "user:"+f.member.String(), &quotav1.Limits{Vcpu: i32(2)}); err != nil {
		t.Fatal(err)
	}
	resp, err := f.client.GetLimits(ctx, as(t, f, f.member, false, &quotav1.GetLimitsRequest{Org: f.orgA.String()}))
	if err != nil {
		t.Fatal(err)
	}
	got := resp.Msg.GetLimits()
	if o := got.GetOrg(); o.GetVcpu() != 10 || o.GetDiskGb() != 100 || o.GetImageStorageGb() != 20 || o.RamMb != nil || o.VmCount != nil || o.SnapshotCount != nil {
		t.Fatalf("org = %v", o)
	}
	if len(got.GetTeams()) != 1 || got.GetTeams()[f.teamA.String()].GetVcpu() != 4 || len(got.GetMembers()) != 1 || got.GetMembers()[f.member.String()].GetVcpu() != 2 {
		t.Fatalf("teams %v, members %v", got.GetTeams(), got.GetMembers())
	}

	code, body := f.rest(t, http.MethodGet, "/orgs/"+f.orgA.String()+"/limits", f.member, false, "")
	if code != 200 || !strings.HasPrefix(body, `{"org":{`) || !strings.Contains(body, `"vcpu":10`) || !strings.Contains(body, `"`+f.teamA.String()+`":{"vcpu":4}`) {
		t.Fatalf("REST get: %d %s", code, body)
	}
	if code, body := f.rest(t, http.MethodPut, "/orgs/"+f.orgA.String()+"/limits", f.admin, false, `{"scope":"team:`+f.teamA.String()+`","limits":{"vcpu":3,"ramMb":null}}`); code != 200 {
		t.Fatalf("REST put: %d %s", code, body)
	}
	if code, body := f.rest(t, http.MethodGet, "/orgs/"+f.orgA.String()+"/limits?org="+f.orgB.String(), f.member, false, ""); code != 400 {
		t.Fatalf("REST query: %d %s", code, body)
	}

	// An empty org has an all-unlimited org scope and no teams or members.
	resp, err = f.client.GetLimits(ctx, as(t, f, f.adminB, false, &quotav1.GetLimitsRequest{Org: f.orgB.String()}))
	if err != nil {
		t.Fatal(err)
	}
	if l := resp.Msg.GetLimits(); l.GetOrg() == nil || l.GetOrg().Vcpu != nil || len(l.GetTeams())+len(l.GetMembers()) != 0 {
		t.Fatalf("empty org = %v", resp.Msg)
	}
}

func TestUpdateLimitsStacking(t *testing.T) {
	f := newFixture(t)
	team, user := "team:"+f.teamA.String(), "user:"+f.member.String()
	if err := f.setOrg(t, &quotav1.Limits{Vcpu: i32(10), DiskGb: i32(100)}); err != nil {
		t.Fatal(err)
	}
	stacking := func(err error) {
		t.Helper()
		wantCode(t, err, connect.CodeInvalidArgument)
		if !strings.Contains(err.Error(), "limit exceeds org limit") {
			t.Fatalf("message: %v", err)
		}
	}
	stacking(f.setScope(t, f.admin, team, &quotav1.Limits{Vcpu: i32(11)}))
	stacking(f.setScope(t, f.admin, user, &quotav1.Limits{DiskGb: i32(101)}))
	// RAM is unlimited at org level, so any team RAM is fine.
	if err := f.setScope(t, f.admin, team, &quotav1.Limits{Vcpu: i32(10), RamMb: i32(1 << 20)}); err != nil {
		t.Fatalf("team at org: %v", err)
	}
	if err := f.setScope(t, f.admin, user, &quotav1.Limits{}); err != nil {
		t.Fatalf("all null: %v", err)
	}
	if err := f.setScope(t, f.admin, user, nil); err != nil {
		t.Fatalf("no limits: %v", err)
	}
	stacking(f.setOrg(t, &quotav1.Limits{Vcpu: i32(9), DiskGb: i32(100)}))
	if err := f.setOrg(t, &quotav1.Limits{DiskGb: i32(100)}); err != nil {
		t.Fatalf("org unlimited vcpu: %v", err)
	}
	stacking(f.setOrg(t, &quotav1.Limits{Vcpu: i32(10), RamMb: i32(5)}))

	ls, err := f.store.limits(context.Background(), f.orgA)
	if err != nil || ls["org"].VCPU != nil || *ls["org"].DiskGB != 100 || *ls[team].VCPU != 10 || ls[user].VCPU != nil {
		t.Fatalf("limits: %+v %v", ls, err)
	}
}

func TestSetLimitsConcurrentOrgLowerVsTeamRaise(t *testing.T) {
	ctx := context.Background()
	s := openStore(t, dbtest.Empty(t))
	org, team := uuid.New(), "team:"+uuid.NewString()
	known := func(scope string) bool { return scope == team }
	if err := s.SetLimits(ctx, org, scopeOrg, Limits{VCPU: i32(10)}, nil, nil); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	wg.Go(func() { _ = s.SetLimits(ctx, org, scopeOrg, Limits{VCPU: i32(5)}, nil, nil) })
	wg.Go(func() { _ = s.SetLimits(ctx, org, team, Limits{VCPU: i32(8)}, known, nil) })
	wg.Wait()
	ls, err := s.limits(ctx, org)
	if err != nil {
		t.Fatal(err)
	}
	if tl, ok := ls[team]; ok && tl.VCPU != nil && *tl.VCPU > *ls[scopeOrg].VCPU {
		t.Fatalf("team above org: %+v", ls)
	}
}

func TestSetLimitsErrors(t *testing.T) {
	ctx := context.Background()
	s := openStore(t, dbtest.Empty(t))
	org, team := uuid.New(), uuid.New()
	known := func(scope string) bool { return scope == "team:"+team.String() }
	if err := s.SetLimits(ctx, org, scopeOrg, Limits{VCPU: i32(10)}, nil, nil); err != nil {
		t.Fatal(err)
	}
	if err := s.SetLimits(ctx, org, "team:"+team.String(), Limits{VCPU: i32(11)}, known, nil); !errors.Is(err, errLimitExceedsOrg) {
		t.Fatalf("team above org: %v", err)
	}
	for _, scope := range []string{"team:" + uuid.NewString(), "team:nope", "bogus", "", "TEAM:" + team.String(), "team:" + strings.ToUpper(team.String())} {
		if err := s.SetLimits(ctx, org, scope, Limits{}, known, nil); !errors.Is(err, errNotFound) {
			t.Fatalf("scope %q: %v", scope, err)
		}
	}
	if err := s.SetLimits(ctx, org, "team:"+team.String(), Limits{}, nil, nil); !errors.Is(err, errNotFound) {
		t.Fatalf("nil known: %v", err)
	}
}

func TestUpdateLimitsOrgScopeDenied(t *testing.T) {
	f := newFixture(t)
	wantCode(t, f.setScope(t, f.admin, "org", &quotav1.Limits{Vcpu: i32(1)}), connect.CodePermissionDenied)
	_, err := f.client.UpdateOrgLimits(context.Background(), as(t, f, f.admin, false, &quotav1.UpdateOrgLimitsRequest{OrgId: f.orgA.String(), Limits: &quotav1.Limits{Vcpu: i32(1)}}))
	wantCode(t, err, connect.CodePermissionDenied)
	es := f.events(t, "quota.limits.update")
	if len(es) != 2 {
		t.Fatalf("recorded %v, want two denials", es)
	}
	for _, e := range es {
		if e.GetResult() != eventsv1.Result_RESULT_DENIED || e.GetActor().GetId() != f.admin.String() {
			t.Fatalf("event %v, want denied by admin", e)
		}
	}
	if es[0].GetOrg() != f.orgA.String() {
		t.Fatalf("org scope denial in org %q", es[0].GetOrg())
	}
	if ls, err := f.store.limits(context.Background(), f.orgA); err != nil || len(ls) != 0 {
		t.Fatalf("limits stored: %v %v", ls, err)
	}
}

func TestUpdateLimitsUnknownScope404(t *testing.T) {
	f := newFixture(t)
	for _, scope := range []string{"team:" + f.teamB.String(), "team:" + uuid.NewString(), "user:" + f.adminB.String(), "user:" + uuid.NewString(),
		"team:nope", "bogus", "team:" + strings.ToUpper(f.teamA.String()), "team:" + uuid.Nil.String()} {
		if err := f.setScope(t, f.admin, scope, &quotav1.Limits{Vcpu: i32(1)}); connect.CodeOf(err) != connect.CodeNotFound {
			t.Fatalf("%s: %v", scope, err)
		}
	}
	_, err := f.client.UpdateLimits(context.Background(), as(t, f, f.adminB, false, &quotav1.UpdateLimitsRequest{Org: f.orgA.String(), Scope: "team:" + f.teamA.String()}))
	wantCode(t, err, connect.CodeNotFound)
	if ls, err := f.store.limits(context.Background(), f.orgB); err != nil || len(ls) != 0 {
		t.Fatalf("foreign org touched: %v %v", ls, err)
	}
}

func TestUpdateLimitsInvalidValue(t *testing.T) {
	f := newFixture(t)
	team := "team:" + f.teamA.String()
	for _, l := range []*quotav1.Limits{{Vcpu: i32(-1)}, {ImageStorageGb: i32(-5)}} {
		err := f.setScope(t, f.admin, team, l)
		wantCode(t, err, connect.CodeInvalidArgument)
		if !strings.Contains(err.Error(), "invalid limit") {
			t.Fatalf("message: %v", err)
		}
	}
	_, err := f.client.UpdateOrgLimits(context.Background(), as(t, f, f.platformAdmin, true, &quotav1.UpdateOrgLimitsRequest{OrgId: f.orgA.String(), Limits: &quotav1.Limits{RamMb: i32(-1)}}))
	wantCode(t, err, connect.CodeInvalidArgument)
	for _, body := range []string{`{"scope":"` + team + `","limits":{"vcpu":-1}}`, `{"scope":"` + team + `","limits":{"vcpu":1.5}}`,
		`{"scope":"` + team + `","limits":{"vcpu":99999999999}}`, `{"scope":"` + team + `","limits":nope}`, `nope`} {
		if code, resp := f.rest(t, http.MethodPut, "/orgs/"+f.orgA.String()+"/limits", f.admin, false, body); code != 400 {
			t.Fatalf("%s: %d %s", body, code, resp)
		}
	}
	if code, resp := f.rest(t, http.MethodPut, "/admin/orgs/"+f.orgA.String()+"/limits", f.platformAdmin, true, `{"vcpu":"x"}`); code != 400 {
		t.Fatalf("admin bad body: %d %s", code, resp)
	}
	if ls, err := f.store.limits(context.Background(), f.orgA); err != nil || len(ls) != 0 {
		t.Fatalf("invalid limits stored: %v %v", ls, err)
	}
	if err := f.setScope(t, f.admin, team, &quotav1.Limits{Vcpu: i32(0), RamMb: i32(2147483647)}); err != nil {
		t.Fatalf("bounds: %v", err)
	}
}

func TestMemberCannotUpdate(t *testing.T) {
	f := newFixture(t)
	wantCode(t, f.setScope(t, f.member, "team:"+f.teamA.String(), &quotav1.Limits{Vcpu: i32(1)}), connect.CodePermissionDenied)
	if code, _ := f.rest(t, http.MethodPut, "/orgs/"+f.orgA.String()+"/limits", f.member, false, `{"scope":"team:`+f.teamA.String()+`","limits":{"vcpu":1}}`); code != 403 {
		t.Fatalf("REST member put: %d", code)
	}
	wantCode(t, f.setScope(t, f.outsider, "team:"+f.teamA.String(), &quotav1.Limits{Vcpu: i32(1)}), connect.CodeNotFound)
	_, err := f.client.GetLimits(context.Background(), as(t, f, f.outsider, false, &quotav1.GetLimitsRequest{Org: f.orgA.String()}))
	wantCode(t, err, connect.CodeNotFound)
	es := f.events(t, "quota.limits.update")
	if len(es) < 1 || es[0].GetResult() != eventsv1.Result_RESULT_DENIED || es[0].GetActor().GetId() != f.member.String() {
		t.Fatalf("denied audit: %v", es)
	}
}

func TestOrgLimitsPlatformOnly(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	_, err := f.client.GetOrgLimits(ctx, as(t, f, f.admin, false, &quotav1.GetOrgLimitsRequest{OrgId: f.orgA.String()}))
	wantCode(t, err, connect.CodePermissionDenied)
	if err := f.setOrg(t, &quotav1.Limits{Vcpu: i32(1)}); err != nil {
		t.Fatal(err)
	}
	resp, err := f.client.GetOrgLimits(ctx, as(t, f, f.platformAdmin, true, &quotav1.GetOrgLimitsRequest{OrgId: f.orgA.String()}))
	if err != nil || resp.Msg.GetLimits().GetOrg().GetVcpu() != 1 {
		t.Fatalf("get: %v %v", resp, err)
	}
	if code, body := f.rest(t, http.MethodPut, "/admin/orgs/"+f.orgA.String()+"/limits", f.platformAdmin, true, `{"vcpu":2}`); code != 200 {
		t.Fatalf("REST admin put: %d %s", code, body)
	}
	if code, body := f.rest(t, http.MethodGet, "/admin/orgs/"+f.orgA.String()+"/limits", f.platformAdmin, true, ""); code != 200 || !strings.Contains(body, `"vcpu":2`) {
		t.Fatalf("REST admin get: %d %s", code, body)
	}
	_, err = f.client.UpdateOrgLimits(ctx, as(t, f, f.platformAdmin, true, &quotav1.UpdateOrgLimitsRequest{OrgId: uuid.NewString(), Limits: &quotav1.Limits{}}))
	wantCode(t, err, connect.CodeNotFound)
	_, err = f.client.GetOrgLimits(ctx, as(t, f, f.platformAdmin, true, &quotav1.GetOrgLimitsRequest{OrgId: uuid.NewString()}))
	wantCode(t, err, connect.CodeNotFound)
	_, err = f.client.GetOrgLimits(ctx, as(t, f, f.platformAdmin, true, &quotav1.GetOrgLimitsRequest{OrgId: strings.ToUpper(f.orgA.String())}))
	wantCode(t, err, connect.CodeInvalidArgument)
}

func TestLimitSetAuditDetails(t *testing.T) {
	f := newFixture(t)
	team := "team:" + f.teamA.String()
	if err := f.setOrg(t, &quotav1.Limits{Vcpu: i32(10)}); err != nil {
		t.Fatal(err)
	}
	if err := f.setScope(t, f.admin, team, &quotav1.Limits{Vcpu: i32(4)}); err != nil {
		t.Fatal(err)
	}
	es := f.events(t, "quota.limits.update")
	if len(es) != 2 {
		t.Fatalf("recorded %v, want one event per write", es)
	}
	for i, want := range []struct {
		actor    uuid.UUID
		resource string
		vcpu     float64
	}{{f.platformAdmin, "org/limits/org", 10}, {f.admin, "org/limits/" + team, 4}} {
		e := es[i]
		d := e.GetDetails().AsMap()
		if e.GetResult() != eventsv1.Result_RESULT_ALLOWED || e.GetOrg() != f.orgA.String() || e.GetActor().GetId() != want.actor.String() ||
			e.GetResource() != want.resource || !e.GetAudit() || len(d) != 6 || d["vcpu"] != want.vcpu || e.GetClientIp() != "198.51.100.4" {
			t.Fatalf("event %d = %v", i, e)
		}
		for _, k := range []string{"ramMb", "diskGb", "vmCount", "snapshotCount", "imageStorageGb"} {
			if v, ok := d[k]; !ok || v != nil {
				t.Fatalf("%s = %v, want explicit null", k, v)
			}
		}
	}
}

func TestRejectedWriteRecordsNothing(t *testing.T) {
	f := newFixture(t)
	if err := f.setOrg(t, &quotav1.Limits{Vcpu: i32(10)}); err != nil {
		t.Fatal(err)
	}
	wantCode(t, f.setScope(t, f.admin, "team:"+f.teamA.String(), &quotav1.Limits{Vcpu: i32(11)}), connect.CodeInvalidArgument)
	if es := f.events(t, "quota.limits.update"); len(es) != 1 {
		t.Fatalf("recorded %v, want only the platform write", es)
	}
}

func TestSetLimitsRollsBackWithItsAuditEvent(t *testing.T) {
	ctx := context.Background()
	s := openStore(t, dbtest.Empty(t))
	org := uuid.New()
	if err := s.SetLimits(ctx, org, scopeOrg, Limits{VCPU: i32(1)}, nil, &eventsv1.Event{Type: "not a type"}); err == nil {
		t.Fatal("unwritable audit event accepted")
	}
	if ls, err := s.limits(ctx, org); err != nil || len(ls) != 0 {
		t.Fatalf("limits stored without their audit event: %v %v", ls, err)
	}
}

func TestRESTPathOrgWins(t *testing.T) {
	f := newFixture(t)
	body := `{"org":"` + f.orgB.String() + `","scope":"team:` + f.teamB.String() + `","limits":{"vcpu":1}}`
	// adminB may write orgB but is no member of the path's orgA.
	if code, resp := f.rest(t, http.MethodPut, "/orgs/"+f.orgA.String()+"/limits", f.adminB, false, body); code != 404 {
		t.Fatalf("body org: %d %s", code, resp)
	}
	if ls, err := f.store.limits(context.Background(), f.orgB); err != nil || len(ls) != 0 {
		t.Fatalf("orgB written: %v %v", ls, err)
	}
}

func TestReadsRecordedWithDataEvents(t *testing.T) {
	f := newFixture(t)
	read := func() {
		t.Helper()
		if _, err := f.client.GetLimits(context.Background(), as(t, f, f.member, false, &quotav1.GetLimitsRequest{Org: f.orgA.String()})); err != nil {
			t.Fatal(err)
		}
	}
	read()
	if es := f.events(t, "quota.limits.get"); len(es) != 0 {
		t.Fatalf("read recorded without data events: %v", es)
	}
	f.audit.set(&auditv1.DataEvents{Org: f.orgA.String(), Service: "quota"})
	js, closeConn, err := bus.Connect(context.Background(), f.natsURL, f.natsToken)
	if err != nil {
		t.Fatal(err)
	}
	defer closeConn()
	if err := js.Conn().Publish("events."+f.orgA.String()+".audit.settings.changed", nil); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "a recorded read", func() bool {
		read()
		return len(f.events(t, "quota.limits.get")) > 0
	})
	if e := f.events(t, "quota.limits.get")[0]; e.GetOrg() != f.orgA.String() || e.GetActor().GetId() != f.member.String() || e.GetResult() != eventsv1.Result_RESULT_ALLOWED {
		t.Fatalf("read event %v", e)
	}
}

func TestQuotaRegistersAtStart(t *testing.T) {
	f := newFixture(t)
	want, err := Catalog()
	if err != nil {
		t.Fatal(err)
	}
	f.iam.mu.Lock()
	defer f.iam.mu.Unlock()
	if len(f.iam.registered) != 1 || f.iam.auths[0] != "Bearer "+testToken {
		t.Fatalf("%d registrations, auth %v", len(f.iam.registered), f.iam.auths)
	}
	got, err := catalog.FromProto(f.iam.registered[0])
	if err != nil {
		t.Fatal(err)
	}
	if got.Service != "quota" || catalog.Digest(got) != catalog.Digest(want) {
		t.Fatalf("registered %+v, want quota.Catalog()", got)
	}
}

func TestQuotaUnavailableUntilCatalog(t *testing.T) {
	f := startFixture(t, true)
	waitFor(t, "the catalog registration", func() bool {
		f.iam.mu.Lock()
		defer f.iam.mu.Unlock()
		return len(f.iam.registered) == 1
	})
	time.Sleep(200 * time.Millisecond)
	_, err := f.client.GetLimits(context.Background(), as(t, f, f.member, false, &quotav1.GetLimitsRequest{Org: f.orgA.String()}))
	wantCode(t, err, connect.CodeUnavailable)
	f.iam.releaseCatalog(t)
	f.ready(t)
}

func TestRunRefusesShortServiceToken(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	err = Run(context.Background(), Config{IAMURL: "http://iam", Issuer: testIssuer, ServiceToken: "short"}, ln)
	if err == nil || !strings.Contains(err.Error(), "service token") {
		t.Fatalf("short service token: %v", err)
	}
}

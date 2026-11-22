package quota

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"connectrpc.com/connect"
	"github.com/google/uuid"
	"google.golang.org/protobuf/proto"

	quotav1 "github.com/asterism-dev/observatory/gen/go/observatory/quota/v1"
	"github.com/asterism-dev/observatory/gen/go/observatory/quota/v1/quotav1connect"
	"github.com/asterism-dev/observatory/internal/db/dbtest"
	"github.com/asterism-dev/observatory/internal/platform/quotaclient"
	"github.com/asterism-dev/observatory/internal/platform/quotatest"
	"github.com/asterism-dev/observatory/internal/platform/servicetoken"
	hubstore "github.com/asterism-dev/observatory/internal/store"
)

func newStore(t *testing.T) *store {
	t.Helper()
	return openStore(t, dbtest.Empty(t))
}

func allKnown(string) bool { return true }

func setLimits(t *testing.T, s *store, org uuid.UUID, scope string, l Limits) {
	t.Helper()
	if err := s.SetLimits(context.Background(), org, scope, l, allKnown, nil); err != nil {
		t.Fatal(err)
	}
}

func allocate(t *testing.T, s *store, org uuid.UUID, resource, owner, parent string, vcpu, snaps int32, deleted bool) {
	t.Helper()
	var p *string
	if parent != "" {
		p = &parent
	}
	_, err := s.pool.Exec(context.Background(), `INSERT INTO allocations (resource, org, owner, parent, vcpu, snapshot_count, last_event_id, deleted)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8)`, resource, org, owner, p, vcpu, snaps, uuid.Must(uuid.NewV7()), deleted)
	if err != nil {
		t.Fatal(err)
	}
}

func wantLimitErr(t *testing.T, err error, scope, name string, limit, got int) {
	t.Helper()
	var le *limitError
	if !errors.As(err, &le) {
		t.Fatalf("err = %v, want a limit error", err)
	}
	if le.Scope != scope || le.Name != name || le.Limit != limit || le.Got != got {
		t.Fatalf("limit error = %+v, want %s %s %d %d", le, scope, name, limit, got)
	}
}

func TestReserveChecksOrgThenOwner(t *testing.T) {
	s, ctx, org := newStore(t), context.Background(), uuid.New()
	owner := "user:" + uuid.NewString()
	setLimits(t, s, org, scopeOrg, Limits{VCPU: i32(4), RAMMB: i32(100)})
	setLimits(t, s, org, owner, Limits{VCPU: i32(2), RAMMB: i32(50)})

	_, _, err := s.Reserve(ctx, org, owner, Amounts{VCPU: i32(5)})
	wantLimitErr(t, err, scopeOrg, "vCPU", 4, 5)
	_, _, err = s.Reserve(ctx, org, owner, Amounts{VCPU: i32(3)})
	wantLimitErr(t, err, owner, "vCPU", 2, 3)
	_, _, err = s.Reserve(ctx, org, owner, Amounts{VCPU: i32(3), RAMMB: i32(60)})
	wantLimitErr(t, err, owner, "vCPU", 2, 3)
	_, _, err = s.Reserve(ctx, org, owner, Amounts{RAMMB: i32(60)})
	wantLimitErr(t, err, owner, "RAM", 50, 60)
	if _, _, err := s.Reserve(ctx, org, "team:"+uuid.NewString(), Amounts{VCPU: i32(3)}); err != nil {
		t.Fatalf("other owner within the org limit: %v", err)
	}
}

func TestReserveOnlyRequestedDimensions(t *testing.T) {
	s, ctx, org := newStore(t), context.Background(), uuid.New()
	owner := "user:" + uuid.NewString()
	setLimits(t, s, org, scopeOrg, Limits{VCPU: i32(1)})
	allocate(t, s, org, "vm/"+uuid.NewString(), owner, "", 5, 0, false)
	if _, _, err := s.Reserve(ctx, org, owner, Amounts{SnapshotCount: i32(1)}); err != nil {
		t.Fatalf("snapshot-only reserve: %v", err)
	}
	_, _, err := s.Reserve(ctx, org, owner, Amounts{VCPU: i32(0)})
	wantLimitErr(t, err, scopeOrg, "vCPU", 1, 5)
}

func TestReserveCountsHoldsAndAllocations(t *testing.T) {
	s, ctx, org := newStore(t), context.Background(), uuid.New()
	owner := "user:" + uuid.NewString()
	setLimits(t, s, org, owner, Limits{VCPU: i32(4)})
	allocate(t, s, org, "vm/"+uuid.NewString(), owner, "", 2, 0, false)
	allocate(t, s, org, "vm/"+uuid.NewString(), owner, "", 100, 0, true)
	allocate(t, s, org, "vm/"+uuid.NewString(), "user:"+uuid.NewString(), "", 100, 0, false)
	allocate(t, s, uuid.New(), "vm/"+uuid.NewString(), owner, "", 100, 0, false)
	if _, _, err := s.Reserve(ctx, org, owner, Amounts{VCPU: i32(1)}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.Reserve(ctx, org, owner, Amounts{VCPU: i32(1)}); err != nil {
		t.Fatal(err)
	}
	_, _, err := s.Reserve(ctx, org, owner, Amounts{VCPU: i32(1)})
	wantLimitErr(t, err, owner, "vCPU", 4, 5)
}

func TestExpiredHoldIgnored(t *testing.T) {
	s, ctx, org := newStore(t), context.Background(), uuid.New()
	now := time.Now()
	s.now = func() time.Time { return now }
	setLimits(t, s, org, scopeOrg, Limits{VCPU: i32(1)})
	_, exp, err := s.Reserve(ctx, org, "user:"+uuid.NewString(), Amounts{VCPU: i32(1)})
	if err != nil {
		t.Fatal(err)
	}
	if !exp.Equal(now.Add(5 * time.Minute)) {
		t.Fatalf("expires = %v, want now + 5 min", exp)
	}
	_, _, err = s.Reserve(ctx, org, "user:"+uuid.NewString(), Amounts{VCPU: i32(1)})
	wantLimitErr(t, err, scopeOrg, "vCPU", 1, 2)
	now = now.Add(5*time.Minute + time.Second)
	if _, _, err := s.Reserve(ctx, org, "user:"+uuid.NewString(), Amounts{VCPU: i32(1)}); err != nil {
		t.Fatalf("expired hold still counted: %v", err)
	}
}

func TestCancelFreesHold(t *testing.T) {
	s, ctx, org := newStore(t), context.Background(), uuid.New()
	owner := "team:" + uuid.NewString()
	setLimits(t, s, org, scopeOrg, Limits{VMCount: i32(1)})
	id, _, err := s.Reserve(ctx, org, owner, Amounts{VMCount: i32(1)})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.Reserve(ctx, org, owner, Amounts{VMCount: i32(1)}); err == nil {
		t.Fatal("second hold fit a limit of 1")
	}
	for range 2 {
		if err := s.Cancel(ctx, []uuid.UUID{id, uuid.New()}); err != nil {
			t.Fatal(err)
		}
	}
	if _, _, err := s.Reserve(ctx, org, owner, Amounts{VMCount: i32(1)}); err != nil {
		t.Fatalf("after cancel: %v", err)
	}
}

func TestReleaseDropsChildren(t *testing.T) {
	s, ctx, org := newStore(t), context.Background(), uuid.New()
	owner := "user:" + uuid.NewString()
	vmX, vmY := "vm/"+uuid.NewString(), "vm/"+uuid.NewString()
	setLimits(t, s, org, scopeOrg, Limits{SnapshotCount: i32(2), VCPU: i32(4)})
	allocate(t, s, org, vmX, owner, "", 2, 0, false)
	allocate(t, s, org, vmY, owner, "", 2, 0, false)
	allocate(t, s, org, "snapshot/"+uuid.NewString(), owner, vmX, 0, 1, false)
	allocate(t, s, org, "snapshot/"+uuid.NewString(), owner, vmY, 0, 1, false)
	_, _, err := s.Reserve(ctx, org, owner, Amounts{SnapshotCount: i32(1)})
	wantLimitErr(t, err, scopeOrg, "snapshot count", 2, 3)

	unknown := "disk/" + uuid.NewString()
	for range 2 {
		if err := s.Release(ctx, org, []string{vmX, unknown}); err != nil {
			t.Fatal(err)
		}
	}
	if _, _, err := s.Reserve(ctx, org, owner, Amounts{SnapshotCount: i32(1), VCPU: i32(2)}); err != nil {
		t.Fatalf("after release: %v", err)
	}
	var live, tombs int
	if err := s.pool.QueryRow(ctx, "SELECT count(*) FILTER (WHERE NOT deleted), count(*) FILTER (WHERE deleted) FROM allocations").Scan(&live, &tombs); err != nil {
		t.Fatal(err)
	}
	if live != 2 || tombs != 3 {
		t.Fatalf("live = %d, tombstones = %d; want 2 and 3 (vm/x, its snapshot, the unknown disk)", live, tombs)
	}
}

func TestReleaseLeavesOtherOrgs(t *testing.T) {
	s, ctx, org := newStore(t), context.Background(), uuid.New()
	vm := "vm/" + uuid.NewString()
	allocate(t, s, org, vm, "user:"+uuid.NewString(), "", 2, 0, false)
	if err := s.Release(ctx, uuid.New(), []string{vm}); err != nil {
		t.Fatal(err)
	}
	var deleted bool
	if err := s.pool.QueryRow(ctx, "SELECT deleted FROM allocations WHERE resource = $1", vm).Scan(&deleted); err != nil {
		t.Fatal(err)
	}
	if deleted {
		t.Fatal("another org's release tombstoned the resource")
	}
}

func TestReserveSerializesPerOrg(t *testing.T) {
	s, org := newStore(t), uuid.New()
	setLimits(t, s, org, scopeOrg, Limits{VCPU: i32(3)})
	var ok, exceeded atomic.Int32
	var wg sync.WaitGroup
	for range 10 {
		wg.Go(func() {
			_, _, err := s.Reserve(context.Background(), org, "user:"+uuid.NewString(), Amounts{VCPU: i32(1)})
			var le *limitError
			switch {
			case err == nil:
				ok.Add(1)
			case errors.As(err, &le):
				exceeded.Add(1)
			default:
				t.Error(err)
			}
		})
	}
	wg.Wait()
	if ok.Load() != 3 || exceeded.Load() != 7 {
		t.Fatalf("succeeded = %d, exceeded = %d; want 3 and 7", ok.Load(), exceeded.Load())
	}
}

func (f *fixture) internalClient(token string) quotav1connect.QuotaInternalServiceClient {
	var opts []connect.ClientOption
	if token != "" {
		opts = append(opts, connect.WithInterceptors(servicetoken.Bearer(token)))
	}
	return quotav1connect.NewQuotaInternalServiceClient(http.DefaultClient, f.baseURL+"/internal/quota", opts...)
}

func TestLimitExceededDetail(t *testing.T) {
	f := newFixture(t)
	ctx, owner := context.Background(), "user:"+f.member.String()
	setLimits(t, f.store, f.orgA, owner, Limits{VCPU: i32(4)})
	c := f.internalClient(hubToken)
	if _, err := c.Reserve(ctx, connect.NewRequest(&quotav1.ReserveRequest{OrgId: f.orgA.String(), Owner: owner, Amounts: &quotav1.Amounts{Vcpu: i32(2)}})); err != nil {
		t.Fatal(err)
	}
	_, err := c.Reserve(ctx, connect.NewRequest(&quotav1.ReserveRequest{OrgId: f.orgA.String(), Owner: owner, Amounts: &quotav1.Amounts{Vcpu: i32(4)}}))
	wantCode(t, err, connect.CodeResourceExhausted)
	d, ok := quotaclient.LimitExceeded(err)
	if !ok {
		t.Fatalf("err = %v, want a LimitExceeded detail", err)
	}
	// Task 4's hub adapter converts the same way.
	got := (&hubstore.LimitError{Scope: d.GetScope(), Name: d.GetName(), Unit: d.GetUnit(), Limit: int(d.GetLimit()), Got: int(d.GetGot())}).Error()
	if want := "the owner's vCPU limit of 4 would be exceeded (6)"; got != want {
		t.Fatalf("message = %q, want %q", got, want)
	}
	if _, ok := quotaclient.LimitExceeded(errors.New("x")); ok {
		t.Fatal("plain error read as LimitExceeded")
	}
}

func TestInternalRequiresServiceToken(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	for name, token := range map[string]string{"none": "", "wrong": "not-a-token", "not allowed": otherToken} {
		c := f.internalClient(token)
		_, err := c.GetLimits(ctx, connect.NewRequest(&quotav1.QuotaInternalServiceGetLimitsRequest{OrgId: f.orgA.String()}))
		if connect.CodeOf(err) != connect.CodePermissionDenied {
			t.Errorf("%s: GetLimits err = %v, want PermissionDenied", name, err)
		}
		_, err = c.Reserve(ctx, connect.NewRequest(&quotav1.ReserveRequest{OrgId: f.orgA.String(), Owner: "user:" + f.member.String()}))
		if connect.CodeOf(err) != connect.CodePermissionDenied {
			t.Errorf("%s: Reserve err = %v, want PermissionDenied", name, err)
		}
	}
	c := f.internalClient(hubToken)
	for _, call := range []func() error{
		func() error {
			_, err := c.GetLimits(ctx, connect.NewRequest(&quotav1.QuotaInternalServiceGetLimitsRequest{OrgId: f.orgA.String()}))
			return err
		},
		func() error {
			_, err := c.Reserve(ctx, connect.NewRequest(&quotav1.ReserveRequest{OrgId: f.orgA.String(), Owner: "user:" + f.member.String()}))
			return err
		},
		func() error {
			_, err := c.Cancel(ctx, connect.NewRequest(&quotav1.CancelRequest{ReservationIds: []string{uuid.NewString()}}))
			return err
		},
		func() error {
			_, err := c.Release(ctx, connect.NewRequest(&quotav1.ReleaseRequest{OrgId: f.orgA.String(), Resources: []string{"vm/" + uuid.NewString()}}))
			return err
		},
	} {
		if err := call(); err != nil {
			t.Errorf("hub call: %v", err)
		}
	}
	res, err := http.Post(f.baseURL+"/internal/quota/x", "application/json", http.NoBody)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != http.StatusForbidden {
		t.Fatalf("unknown path without token = %d, want 403", res.StatusCode)
	}
}

func TestInternalBodyCapped(t *testing.T) {
	f := newFixture(t)
	ids := make([]string, maxBody/30)
	for i := range ids {
		ids[i] = uuid.NewString()
	}
	_, err := f.internalClient(hubToken).Cancel(context.Background(), connect.NewRequest(&quotav1.CancelRequest{ReservationIds: ids}))
	if connect.CodeOf(err) != connect.CodeResourceExhausted {
		t.Fatalf("oversized body err = %v, want ResourceExhausted", err)
	}
}

type step struct {
	name    string
	call    func(ctx context.Context, c quotav1connect.QuotaInternalServiceClient, held []string) (string, error)
	code    connect.Code
	exceeds *quotav1.LimitExceeded
}

func reserve(owner string, a *quotav1.Amounts, org string) func(context.Context, quotav1connect.QuotaInternalServiceClient, []string) (string, error) {
	return func(ctx context.Context, c quotav1connect.QuotaInternalServiceClient, _ []string) (string, error) {
		res, err := c.Reserve(ctx, connect.NewRequest(&quotav1.ReserveRequest{OrgId: org, Owner: owner, Amounts: a}))
		if err != nil {
			return "", err
		}
		return res.Msg.GetReservationId(), nil
	}
}

func TestFakeMatchesService(t *testing.T) {
	f := newFixture(t)
	org, user, team := f.orgA, "user:"+f.member.String(), "team:"+f.teamA.String()
	limits := map[string]*quotav1.Limits{
		scopeOrg: {Vcpu: i32(3), SnapshotCount: i32(5)},
		user:     {Vcpu: i32(2), SnapshotCount: i32(1)},
	}
	fake := quotatest.NewFake()
	for scope, l := range limits {
		fake.SetLimits(org, scope, l)
		dl, err := fromProto(l)
		if err != nil {
			t.Fatal(err)
		}
		setLimits(t, f.store, org, scope, dl)
	}
	steps := []step{
		{name: "fits", call: reserve(user, &quotav1.Amounts{Vcpu: i32(2)}, org.String())},
		{name: "owner limit", call: reserve(user, &quotav1.Amounts{Vcpu: i32(1)}, org.String()), code: connect.CodeResourceExhausted,
			exceeds: &quotav1.LimitExceeded{Scope: user, Name: "vCPU", Limit: 2, Got: 3}},
		{name: "team fits", call: reserve(team, &quotav1.Amounts{Vcpu: i32(1)}, org.String())},
		{name: "org limit first", call: reserve(user, &quotav1.Amounts{Vcpu: i32(1), RamMb: i32(1)}, org.String()), code: connect.CodeResourceExhausted,
			exceeds: &quotav1.LimitExceeded{Scope: scopeOrg, Name: "vCPU", Limit: 3, Got: 4}},
		{name: "unchecked dimension", call: reserve(user, &quotav1.Amounts{SnapshotCount: i32(1), DiskGb: i32(1000)}, org.String())},
		{name: "snapshot owner limit", call: reserve(user, &quotav1.Amounts{SnapshotCount: i32(1)}, org.String()), code: connect.CodeResourceExhausted,
			exceeds: &quotav1.LimitExceeded{Scope: user, Name: "snapshot count", Limit: 1, Got: 2}},
		{name: "other org unlimited", call: reserve(user, &quotav1.Amounts{Vcpu: i32(100)}, f.orgB.String())},
		{name: "bad owner", call: reserve("user:nope", &quotav1.Amounts{Vcpu: i32(1)}, org.String()), code: connect.CodeInvalidArgument},
		{name: "org owner", call: reserve(scopeOrg, &quotav1.Amounts{Vcpu: i32(1)}, org.String()), code: connect.CodeInvalidArgument},
		{name: "negative", call: reserve(team, &quotav1.Amounts{Vcpu: i32(-1)}, org.String()), code: connect.CodeInvalidArgument},
		{name: "bad org", call: reserve(team, nil, ""), code: connect.CodeInvalidArgument},
		{name: "cancel first", call: func(ctx context.Context, c quotav1connect.QuotaInternalServiceClient, held []string) (string, error) {
			_, err := c.Cancel(ctx, connect.NewRequest(&quotav1.CancelRequest{ReservationIds: held[:1]}))
			return "", err
		}},
		{name: "fits after cancel", call: reserve(user, &quotav1.Amounts{Vcpu: i32(2)}, org.String())},
		{name: "bad cancel id", call: func(ctx context.Context, c quotav1connect.QuotaInternalServiceClient, _ []string) (string, error) {
			_, err := c.Cancel(ctx, connect.NewRequest(&quotav1.CancelRequest{ReservationIds: []string{"nope"}}))
			return "", err
		}, code: connect.CodeInvalidArgument},
		{name: "bad resource", call: func(ctx context.Context, c quotav1connect.QuotaInternalServiceClient, _ []string) (string, error) {
			_, err := c.Release(ctx, connect.NewRequest(&quotav1.ReleaseRequest{OrgId: org.String(), Resources: []string{"node/" + uuid.NewString()}}))
			return "", err
		}, code: connect.CodeInvalidArgument},
	}
	path, h := quotav1connect.NewQuotaInternalServiceHandler(fake)
	mux := http.NewServeMux()
	mux.Handle(path, h)
	fakeSrv := httptest.NewServer(mux)
	t.Cleanup(fakeSrv.Close)
	clients := map[string]quotav1connect.QuotaInternalServiceClient{
		"service": f.internalClient(hubToken),
		"fake":    quotav1connect.NewQuotaInternalServiceClient(http.DefaultClient, fakeSrv.URL),
	}
	ctx := context.Background()
	for name, c := range clients {
		var held []string
		for _, st := range steps {
			id, err := st.call(ctx, c, held)
			if id != "" {
				held = append(held, id)
			}
			if code := connect.CodeOf(err); err != nil && code != st.code || err == nil && st.code != 0 {
				t.Fatalf("%s %s: err = %v, want code %v", name, st.name, err, st.code)
			}
			if st.exceeds != nil {
				d, ok := quotaclient.LimitExceeded(err)
				if !ok || !proto.Equal(d, st.exceeds) {
					t.Fatalf("%s %s: detail = %v, want %v", name, st.name, d, st.exceeds)
				}
			}
		}
		res, err := c.GetLimits(ctx, connect.NewRequest(&quotav1.QuotaInternalServiceGetLimitsRequest{OrgId: org.String()}))
		if err != nil {
			t.Fatalf("%s GetLimits: %v", name, err)
		}
		want := &quotav1.OrgLimits{Org: limits[scopeOrg], Members: map[string]*quotav1.Limits{f.member.String(): limits[user]}}
		if !proto.Equal(res.Msg.GetLimits(), want) {
			t.Fatalf("%s GetLimits = %v, want %v", name, res.Msg.GetLimits(), want)
		}
	}
	if got := fake.Holds(); got != 4 {
		t.Fatalf("fake holds = %d, want 4", got)
	}
}

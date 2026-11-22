package quota

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"connectrpc.com/connect"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/nats-io/nats.go/jetstream"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	eventsv1 "github.com/asterism-dev/observatory/gen/go/observatory/events/v1"
	iamv1 "github.com/asterism-dev/observatory/gen/go/observatory/iam/v1"
	"github.com/asterism-dev/observatory/gen/go/observatory/iam/v1/iamv1connect"
	coreauthz "github.com/asterism-dev/observatory/internal/authz"
	"github.com/asterism-dev/observatory/internal/platform/bus"
	"github.com/asterism-dev/observatory/internal/platform/iamtest"
	lifecycle "github.com/asterism-dev/observatory/internal/platform/usage"
)

type consumerEnv struct {
	t    *testing.T
	s    *store
	js   jetstream.JetStream
	iam  *iamtest.Server
	org  uuid.UUID
	stop func() error
}

func newConsumerEnv(t *testing.T) *consumerEnv {
	t.Helper()
	return newConsumerEnvWith(t, nil)
}

// flakyIAM answers GetMembership through fail first, falling back to the real fake when fail returns nil.
type flakyIAM struct {
	iamv1connect.IAMInternalServiceClient
	fail func(context.Context) error
}

func (f flakyIAM) GetMembership(ctx context.Context, r *connect.Request[iamv1.GetMembershipRequest]) (*connect.Response[iamv1.GetMembershipResponse], error) {
	if err := f.fail(ctx); err != nil {
		return nil, err
	}
	return f.IAMInternalServiceClient.GetMembership(ctx, r)
}

func newConsumerEnvWith(t *testing.T, fail func(context.Context) error) *consumerEnv {
	t.Helper()
	ctx := context.Background()
	url, token, stop, err := bus.Embedded(ctx, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(stop)
	js, closeConn, err := bus.Connect(ctx, url, token)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(closeConn)
	if err := bus.EnsureStream(ctx, js); err != nil {
		t.Fatal(err)
	}
	e := &consumerEnv{t: t, s: newStore(t), js: js, iam: iamtest.New(t)}
	e.org = e.iam.CreateOrg(e.iam.AddUser("owner").ID, "acme")
	cctx, cancel := context.WithCancel(ctx)
	done := make(chan error, 1)
	iam := e.iam.Client()
	if fail != nil {
		iam = flakyIAM{IAMInternalServiceClient: iam, fail: fail}
	}
	go func() { done <- consumeAll(cctx, js, e.s, iam) }()
	var once sync.Once
	var stopErr error
	e.stop = func() error {
		once.Do(func() {
			cancel()
			stopErr = <-done
		})
		return stopErr
	}
	t.Cleanup(func() {
		if err := e.stop(); err != nil {
			t.Error(err)
		}
	})
	return e
}

func (e *consumerEnv) publish(ev *eventsv1.Event) {
	e.t.Helper()
	if ev.Id == "" {
		ev.Id = uuid.Must(uuid.NewV7()).String()
	}
	ev.Time = timestamppb.Now()
	e.publishRaw("events."+ev.GetOrg()+"."+ev.GetType(), ev)
}

func (e *consumerEnv) publishRaw(subject string, ev *eventsv1.Event) {
	e.t.Helper()
	b, err := proto.Marshal(ev)
	if err != nil {
		e.t.Fatal(err)
	}
	if _, err := e.js.Publish(context.Background(), subject, b); err != nil {
		e.t.Fatal(err)
	}
}

func (e *consumerEnv) lifecycle(typ string, l lifecycle.Lifecycle) *eventsv1.Event {
	return lifecycle.Event(e.org, typ, "org/vms/x", l)
}

// settle waits until an event published now has been applied; the consumer is strictly ordered, so all earlier ones have too.
func (e *consumerEnv) settle() {
	e.t.Helper()
	marker := "disk/" + uuid.NewString()
	other := uuid.New()
	e.publish(lifecycle.Event(other, lifecycle.DiskCreated, "org/disks/x", lifecycle.Lifecycle{Resource: marker, Owner: "user:" + uuid.NewString(), Amounts: lifecycle.Amounts{DiskGB: 1}}))
	waitFor(e.t, "marker event", func() bool {
		var n int
		_ = e.s.pool.QueryRow(context.Background(), "SELECT count(*) FROM allocations WHERE org = $1 AND resource = $2", other, marker).Scan(&n)
		return n == 1
	})
}

func (e *consumerEnv) usage(owner string) [5]int64 {
	e.t.Helper()
	var u [5]int64
	err := pgx.BeginFunc(context.Background(), e.s.pool, func(tx pgx.Tx) (err error) {
		u, err = usage(context.Background(), tx, e.org, owner, e.s.now())
		return err
	})
	if err != nil {
		e.t.Fatal(err)
	}
	return u
}

func (e *consumerEnv) wantUsage(owner string, want [5]int64) {
	e.t.Helper()
	e.settle()
	if got := e.usage(owner); got != want {
		e.t.Fatalf("usage(%q) = %v, want %v", owner, got, want)
	}
}

func (e *consumerEnv) scopes() map[string]bool {
	e.t.Helper()
	ls, err := e.s.limits(context.Background(), e.org)
	if err != nil {
		e.t.Fatal(err)
	}
	out := map[string]bool{}
	for s := range ls {
		out[s] = true
	}
	return out
}

func newOwner() string { return "user:" + uuid.NewString() }

func TestCreatedSetsAllocation(t *testing.T) {
	e := newConsumerEnv(t)
	o := newOwner()
	e.publish(e.lifecycle(lifecycle.InstanceCreated, lifecycle.Lifecycle{Resource: "vm/" + uuid.NewString(), Owner: o, Amounts: lifecycle.Amounts{VCPU: 2, RAMMB: 1024, DiskGB: 20, VMCount: 1}}))
	e.wantUsage(o, [5]int64{2, 1024, 20, 1, 0})
	e.wantUsage("", [5]int64{2, 1024, 20, 1, 0})
	e.wantUsage(newOwner(), [5]int64{})
}

func TestResizedSetsAbsolute(t *testing.T) {
	e := newConsumerEnv(t)
	o, disk := newOwner(), "disk/"+uuid.NewString()
	e.publish(e.lifecycle(lifecycle.DiskCreated, lifecycle.Lifecycle{Resource: disk, Owner: o, Amounts: lifecycle.Amounts{DiskGB: 10}}))
	e.publish(e.lifecycle(lifecycle.DiskResized, lifecycle.Lifecycle{Resource: disk, Owner: o, Amounts: lifecycle.Amounts{DiskGB: 20}}))
	e.wantUsage(o, [5]int64{0, 0, 20, 0, 0})
	e.publish(e.lifecycle(lifecycle.DiskDeleted, lifecycle.Lifecycle{Resource: disk}))
	e.wantUsage(o, [5]int64{})
}

func TestDeletedTombstonesWithChildren(t *testing.T) {
	e := newConsumerEnv(t)
	o, vm := newOwner(), "vm/"+uuid.NewString()
	e.publish(e.lifecycle(lifecycle.InstanceCreated, lifecycle.Lifecycle{Resource: vm, Owner: o, Amounts: lifecycle.Amounts{VCPU: 2, VMCount: 1}}))
	e.publish(e.lifecycle(lifecycle.SnapshotCreated, lifecycle.Lifecycle{Resource: "snapshot/" + uuid.NewString(), Parent: vm, Owner: o, Amounts: lifecycle.Amounts{DiskGB: 5, SnapshotCount: 1}}))
	e.wantUsage(o, [5]int64{2, 0, 5, 1, 1})
	e.publish(e.lifecycle(lifecycle.InstanceDeleted, lifecycle.Lifecycle{Resource: vm}))
	e.wantUsage(o, [5]int64{})
	// A snapshot whose created event lands after its vm's deletion counts as deleted too.
	e.publish(e.lifecycle(lifecycle.SnapshotCreated, lifecycle.Lifecycle{Resource: "snapshot/" + uuid.NewString(), Parent: vm, Owner: o, Amounts: lifecycle.Amounts{SnapshotCount: 1}}))
	e.wantUsage(o, [5]int64{})
}

func TestReleasedParentHidesLateChild(t *testing.T) {
	e := newConsumerEnv(t)
	o, vm := newOwner(), "vm/"+uuid.NewString()
	if err := e.s.Release(context.Background(), e.org, []string{vm}); err != nil {
		t.Fatal(err)
	}
	e.publish(e.lifecycle(lifecycle.InstanceCreated, lifecycle.Lifecycle{Resource: vm, Owner: o, Amounts: lifecycle.Amounts{VCPU: 2, VMCount: 1}}))
	e.publish(e.lifecycle(lifecycle.SnapshotCreated, lifecycle.Lifecycle{Resource: "snapshot/" + uuid.NewString(), Parent: vm, Owner: o, Amounts: lifecycle.Amounts{SnapshotCount: 1}}))
	e.wantUsage(o, [5]int64{})
}

func TestReplayAfterDeleteIgnored(t *testing.T) {
	e := newConsumerEnv(t)
	o, vm := newOwner(), "vm/"+uuid.NewString()
	created := e.lifecycle(lifecycle.InstanceCreated, lifecycle.Lifecycle{Resource: vm, Owner: o, Amounts: lifecycle.Amounts{VCPU: 2, VMCount: 1}})
	e.publish(created)
	e.publish(e.lifecycle(lifecycle.InstanceDeleted, lifecycle.Lifecycle{Resource: vm}))
	e.wantUsage(o, [5]int64{})
	// Redelivered with its original, older id.
	e.publish(created)
	e.wantUsage(o, [5]int64{})
}

func TestCommittedDropsHolds(t *testing.T) {
	e := newConsumerEnv(t)
	o, vm := newOwner(), "vm/"+uuid.NewString()
	hold, _, err := e.s.Reserve(context.Background(), e.org, o, Amounts{VCPU: i32(2), VMCount: i32(1)})
	if err != nil {
		t.Fatal(err)
	}
	e.publish(e.lifecycle(lifecycle.InstanceCreated, lifecycle.Lifecycle{Resource: vm, Owner: o, Amounts: lifecycle.Amounts{VCPU: 2, VMCount: 1}}))
	e.publish(lifecycle.Committed(e.org, []uuid.UUID{hold}))
	deadline := time.Now().Add(10 * time.Second)
	for {
		var holds int
		if err := e.s.pool.QueryRow(context.Background(), "SELECT count(*) FROM reservations").Scan(&holds); err != nil {
			t.Fatal(err)
		}
		if u := e.usage(o); u[0] < 2 || u[3] < 1 {
			t.Fatalf("usage dipped to %v", u)
		}
		if holds == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("hold never dropped")
		}
		time.Sleep(5 * time.Millisecond)
	}
	e.wantUsage(o, [5]int64{2, 0, 0, 1, 0})
}

func memberEvent(org, user uuid.UUID, typ string, result eventsv1.Result) *eventsv1.Event {
	return &eventsv1.Event{Type: typ, Org: org.String(), Resource: coreauthz.ObjMembers + "/" + user.String(), Result: result, Audit: true}
}

func TestMemberDeleteDropsUserLimits(t *testing.T) {
	e := newConsumerEnv(t)
	gone, left, back := e.iam.AddUser("gone").ID, e.iam.AddUser("left").ID, e.iam.AddUser("back").ID
	team := uuid.New()
	for _, u := range []uuid.UUID{gone, left, back} {
		e.iam.AddMember(e.org, u, "member")
		setLimits(t, e.s, e.org, coreauthz.UserSubject(u), Limits{VCPU: i32(1)})
	}
	setLimits(t, e.s, e.org, coreauthz.TeamSubject(team), Limits{VCPU: i32(1)})
	e.iam.RemoveMember(e.org, gone)
	e.iam.RemoveMember(e.org, left)
	e.publish(memberEvent(e.org, gone, "iam.members.delete", eventsv1.Result_RESULT_ALLOWED))
	e.publish(memberEvent(e.org, left, "iam.members.leave", eventsv1.Result_RESULT_ALLOWED))
	// back was removed and has re-joined before quota saw the event.
	e.publish(memberEvent(e.org, back, "iam.members.delete", eventsv1.Result_RESULT_ALLOWED))
	e.settle()
	got := e.scopes()
	if got[coreauthz.UserSubject(gone)] || got[coreauthz.UserSubject(left)] || !got[coreauthz.UserSubject(back)] || !got[coreauthz.TeamSubject(team)] {
		t.Fatalf("scopes = %v", got)
	}
}

func TestTeamDeleteDropsTeamLimits(t *testing.T) {
	e := newConsumerEnv(t)
	deleted, kept, user := uuid.New(), uuid.New(), e.iam.AddUser("u").ID
	e.iam.AddMember(e.org, user, "member")
	for _, s := range []string{coreauthz.TeamSubject(deleted), coreauthz.TeamSubject(kept), coreauthz.UserSubject(user)} {
		setLimits(t, e.s, e.org, s, Limits{VCPU: i32(1)})
	}
	e.publish(&eventsv1.Event{Type: "iam.teams.delete", Org: e.org.String(), Resource: coreauthz.ObjTeams + "/" + deleted.String(), Result: eventsv1.Result_RESULT_ALLOWED})
	e.settle()
	got := e.scopes()
	if got[coreauthz.TeamSubject(deleted)] || !got[coreauthz.TeamSubject(kept)] || !got[coreauthz.UserSubject(user)] {
		t.Fatalf("scopes = %v", got)
	}
}

func TestDeniedIAMEventIgnored(t *testing.T) {
	e := newConsumerEnv(t)
	user, team := e.iam.AddUser("u").ID, uuid.New()
	e.iam.AddMember(e.org, user, "member")
	setLimits(t, e.s, e.org, coreauthz.UserSubject(user), Limits{VCPU: i32(1)})
	setLimits(t, e.s, e.org, coreauthz.TeamSubject(team), Limits{VCPU: i32(1)})
	e.iam.RemoveMember(e.org, user)
	e.publish(memberEvent(e.org, user, "iam.members.delete", eventsv1.Result_RESULT_DENIED))
	e.publish(&eventsv1.Event{Type: "iam.teams.delete", Org: e.org.String(), Resource: coreauthz.ObjTeams + "/" + team.String(), Result: eventsv1.Result_RESULT_FAILED})
	e.settle()
	if got := e.scopes(); !got[coreauthz.UserSubject(user)] || !got[coreauthz.TeamSubject(team)] {
		t.Fatalf("scopes = %v", got)
	}
}

func TestUnparsableEventAcked(t *testing.T) {
	e := newConsumerEnv(t)
	if _, err := e.js.Publish(context.Background(), "events."+e.org.String()+"."+lifecycle.InstanceCreated, []byte("not a proto")); err != nil {
		t.Fatal(err)
	}
	bad := e.lifecycle(lifecycle.InstanceCreated, lifecycle.Lifecycle{Resource: "node/x", Owner: newOwner(), Amounts: lifecycle.Amounts{VCPU: 1}})
	e.publish(bad)
	e.publish(&eventsv1.Event{Type: "iam.members.delete", Org: e.org.String(), Resource: "org/members/nope", Result: eventsv1.Result_RESULT_ALLOWED})
	e.publish(&eventsv1.Event{Type: "iam.teams.delete", Org: e.org.String(), Resource: "org/teams/nope", Result: eventsv1.Result_RESULT_ALLOWED})
	o := newOwner()
	e.publish(e.lifecycle(lifecycle.InstanceCreated, lifecycle.Lifecycle{Resource: "vm/" + uuid.NewString(), Owner: o, Amounts: lifecycle.Amounts{VCPU: 1}}))
	e.wantUsage(o, [5]int64{1, 0, 0, 0, 0})
}

func TestTombstonesPruned(t *testing.T) {
	s, ctx, org := newStore(t), context.Background(), uuid.New()
	now := time.Now()
	clock := now.Add(-9 * 24 * time.Hour)
	s.now = func() time.Time { return clock }
	o := newOwner()
	old, live := "vm/"+uuid.NewString(), "vm/"+uuid.NewString()
	if err := s.setAllocation(ctx, org, live, o, "", Amounts{VCPU: i32(1)}, uuid.Must(uuid.NewV7())); err != nil {
		t.Fatal(err)
	}
	if err := s.Release(ctx, org, []string{old}); err != nil {
		t.Fatal(err)
	}
	clock = now.Add(-7 * 24 * time.Hour)
	recent := "vm/" + uuid.NewString()
	if err := s.Release(ctx, org, []string{recent}); err != nil {
		t.Fatal(err)
	}
	clock = now
	if err := s.pruneTombstones(ctx); err != nil {
		t.Fatal(err)
	}
	rows, err := s.pool.Query(ctx, "SELECT resource FROM allocations ORDER BY resource")
	if err != nil {
		t.Fatal(err)
	}
	got, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]bool{live: true, recent: true}
	if len(got) != 2 || !want[got[0]] || !want[got[1]] {
		t.Fatalf("allocations = %v, want %s and %s", got, live, recent)
	}
}

func TestExpiredReservationsSwept(t *testing.T) {
	s, ctx, org := newStore(t), context.Background(), uuid.New()
	now := time.Now()
	clock := now.Add(-6 * time.Minute)
	s.now = func() time.Time { return clock }
	if _, _, err := s.Reserve(ctx, org, newOwner(), Amounts{VCPU: i32(1)}); err != nil {
		t.Fatal(err)
	}
	clock = now
	kept, _, err := s.Reserve(ctx, org, newOwner(), Amounts{VCPU: i32(1)})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.sweepReservations(ctx); err != nil {
		t.Fatal(err)
	}
	rows, err := s.pool.Query(ctx, "SELECT id FROM reservations")
	if err != nil {
		t.Fatal(err)
	}
	got, err := pgx.CollectRows(rows, pgx.RowTo[uuid.UUID])
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0] != kept {
		t.Fatalf("reservations = %v, want only %s", got, kept)
	}
}

func TestOtherComputeEventsNotConsumed(t *testing.T) {
	e := newConsumerEnv(t)
	e.publish(&eventsv1.Event{Type: "compute.disks.create", Org: e.org.String(), Resource: "org/disks/x", Result: eventsv1.Result_RESULT_ALLOWED, Audit: true})
	e.publish(&eventsv1.Event{Type: "compute.snapshots.delete", Org: e.org.String(), Resource: "org/vms/x", Result: eventsv1.Result_RESULT_ALLOWED, Audit: true})
	e.settle()
	c, err := e.js.Consumer(context.Background(), bus.Stream, "quota")
	if err != nil {
		t.Fatal(err)
	}
	info, err := c.Info(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if info.Delivered.Consumer != 1 {
		t.Fatalf("delivered %d messages, want only the marker", info.Delivered.Consumer)
	}
}

func TestCommittedScopedToOrg(t *testing.T) {
	e := newConsumerEnv(t)
	hold, _, err := e.s.Reserve(context.Background(), uuid.New(), newOwner(), Amounts{VCPU: i32(1)})
	if err != nil {
		t.Fatal(err)
	}
	e.publish(lifecycle.Committed(e.org, []uuid.UUID{hold}))
	e.settle()
	var n int
	if err := e.s.pool.QueryRow(context.Background(), "SELECT count(*) FROM reservations WHERE id = $1", hold).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatal("a committed event dropped another org's hold")
	}
}

func TestIAMOutageDoesNotBlockUsage(t *testing.T) {
	e := newConsumerEnvWith(t, func(context.Context) error { return connect.NewError(connect.CodeUnavailable, errors.New("iam down")) })
	user := e.iam.AddUser("u").ID
	e.publish(memberEvent(e.org, user, "iam.members.delete", eventsv1.Result_RESULT_ALLOWED))
	o := newOwner()
	e.publish(e.lifecycle(lifecycle.InstanceCreated, lifecycle.Lifecycle{Resource: "vm/" + uuid.NewString(), Owner: o, Amounts: lifecycle.Amounts{VCPU: 1}}))
	e.wantUsage(o, [5]int64{1, 0, 0, 0, 0})
}

func TestMemberEventRetriedUntilIAMAnswers(t *testing.T) {
	var calls atomic.Int32
	e := newConsumerEnvWith(t, func(context.Context) error {
		if calls.Add(1) == 1 {
			return connect.NewError(connect.CodeUnavailable, errors.New("iam restarting"))
		}
		return nil
	})
	user, team := e.iam.AddUser("u").ID, uuid.New()
	setLimits(t, e.s, e.org, coreauthz.UserSubject(user), Limits{VCPU: i32(1)})
	setLimits(t, e.s, e.org, coreauthz.TeamSubject(team), Limits{VCPU: i32(1)})
	e.publish(memberEvent(e.org, user, "iam.members.delete", eventsv1.Result_RESULT_ALLOWED))
	e.publish(&eventsv1.Event{Type: "iam.teams.delete", Org: e.org.String(), Resource: coreauthz.ObjTeams + "/" + team.String(), Result: eventsv1.Result_RESULT_ALLOWED})
	waitFor(t, "member and team limits dropped", func() bool { return len(e.scopes()) == 0 })
	if calls.Load() < 2 {
		t.Fatalf("GetMembership called %d times, want a retry", calls.Load())
	}
}

func TestShutdownDuringHandler(t *testing.T) {
	entered := make(chan struct{}, 1)
	e := newConsumerEnvWith(t, func(ctx context.Context) error {
		select {
		case entered <- struct{}{}:
		default:
		}
		<-ctx.Done()
		return ctx.Err()
	})
	e.publish(memberEvent(e.org, e.iam.AddUser("u").ID, "iam.members.delete", eventsv1.Result_RESULT_ALLOWED))
	select {
	case <-entered:
	case <-time.After(10 * time.Second):
		t.Fatal("handler never ran")
	}
	done := make(chan error, 1)
	go func() { done <- e.stop() }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(memberTimeout):
		t.Fatal("shutdown waited for the iam call")
	}
}

package outbox_test

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"net"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/fstest"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/nats-io/nats-server/v2/server"
	"github.com/nats-io/nats.go/jetstream"
	"google.golang.org/protobuf/proto"

	eventsv1 "github.com/asterism-dev/observatory/gen/go/observatory/events/v1"
	"github.com/asterism-dev/observatory/internal/db/dbtest"
	"github.com/asterism-dev/observatory/internal/platform/bus"
	"github.com/asterism-dev/observatory/internal/platform/outbox"
	"github.com/asterism-dev/observatory/internal/platform/pgsvc"
)

var errRollback = errors.New("rollback")

const token = "0123456789abcdef0123456789abcdef"

const org = "0b9d6a43-5d43-4a43-9a43-1d43c5d43a43"

func freePort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}

func startNATS(t *testing.T, dir string, port int) *server.Server {
	t.Helper()
	s, err := server.NewServer(&server.Options{Host: "127.0.0.1", Port: port, JetStream: true, StoreDir: dir, Authorization: token, NoSigs: true, NoLog: true})
	if err != nil {
		t.Fatal(err)
	}
	go s.Start()
	if !s.ReadyForConnections(10 * time.Second) {
		t.Fatal("nats not ready")
	}
	return s
}

type env struct {
	pool *pgxpool.Pool
	js   jetstream.JetStream
	nats *server.Server
	dir  string
	port int
}

func setup(t *testing.T) *env {
	t.Helper()
	ctx := context.Background()
	own := fstest.MapFS{"00001_things.sql": {Data: []byte("-- +goose Up\nCREATE TABLE things (id int);\n-- +goose Down\nDROP TABLE things;\n")}}
	pool, err := pgsvc.Open(ctx, dbtest.Empty(t), "svc", outbox.Migrations(own, 2))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)

	e := &env{pool: pool, dir: t.TempDir(), port: freePort(t)}
	e.nats = startNATS(t, e.dir, e.port)
	t.Cleanup(func() { e.nats.Shutdown() })
	js, closeConn, err := bus.Connect(ctx, "nats://127.0.0.1:"+strconv.Itoa(e.port), token)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(closeConn)
	if err := bus.EnsureStream(ctx, js); err != nil {
		t.Fatal(err)
	}
	e.js = js
	return e
}

func (e *env) runRelay(t *testing.T, configure func(*outbox.Relay)) (stop func()) {
	t.Helper()
	r := outbox.NewRelay(e.pool, e.js)
	if configure != nil {
		configure(r)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		r.Run(ctx)
	}()
	stopped := false
	stop = func() {
		if !stopped {
			stopped = true
			cancel()
			<-done
		}
	}
	t.Cleanup(stop)
	return stop
}

func (e *env) write(t *testing.T, commit bool) *eventsv1.Event {
	t.Helper()
	ctx := context.Background()
	ev := &eventsv1.Event{Type: "iam.members.delete", Org: org, Audit: true}
	err := pgx.BeginFunc(ctx, e.pool, func(tx pgx.Tx) error {
		if err := outbox.Write(ctx, tx, ev); err != nil {
			t.Fatal(err)
		}
		if !commit {
			return errRollback
		}
		return nil
	})
	if commit && err != nil {
		t.Fatal(err)
	}
	return ev
}

func (e *env) messages(t *testing.T) []*jetstream.RawStreamMsg {
	t.Helper()
	ctx := context.Background()
	s, err := e.js.Stream(ctx, bus.Stream)
	if err != nil {
		t.Fatal(err)
	}
	info, err := s.Info(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var msgs []*jetstream.RawStreamMsg
	for seq := info.State.FirstSeq; seq <= info.State.LastSeq && info.State.Msgs > 0; seq++ {
		m, err := s.GetMsg(ctx, seq)
		if err != nil {
			t.Fatal(err)
		}
		msgs = append(msgs, m)
	}
	return msgs
}

func (e *env) count(t *testing.T, where string) int {
	t.Helper()
	var n int
	if err := e.pool.QueryRow(context.Background(), "SELECT count(*) FROM outbox WHERE "+where).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	for deadline := time.Now().Add(20 * time.Second); time.Now().Before(deadline); time.Sleep(100 * time.Millisecond) {
		if cond() {
			return
		}
	}
	t.Fatalf("timed out waiting for %s", what)
}

func TestRollbackEmitsNothing(t *testing.T) {
	e := setup(t)
	e.runRelay(t, nil)
	e.write(t, false)
	time.Sleep(1500 * time.Millisecond)
	if n := len(e.messages(t)); n != 0 {
		t.Fatalf("messages = %d, want 0", n)
	}
	if n := e.count(t, "true"); n != 0 {
		t.Fatalf("outbox rows = %d, want 0", n)
	}
}

func TestCommitEmitsOneMessage(t *testing.T) {
	e := setup(t)
	e.runRelay(t, nil)
	ev := e.write(t, true)
	if ev.GetId() == "" || ev.GetTime() == nil {
		t.Fatalf("id/time not assigned: %v", ev)
	}
	eventually(t, "row sent", func() bool { return e.count(t, "sent_at IS NOT NULL") == 1 })
	time.Sleep(1500 * time.Millisecond)
	msgs := e.messages(t)
	if len(msgs) != 1 {
		t.Fatalf("messages = %d, want 1", len(msgs))
	}
	m := msgs[0]
	if got := m.Header.Get(jetstream.MsgIDHeader); got != ev.GetId() {
		t.Fatalf("Nats-Msg-Id = %q, want %q", got, ev.GetId())
	}
	if want := "events." + org + ".iam.members.delete"; m.Subject != want {
		t.Fatalf("subject = %q, want %q", m.Subject, want)
	}
	var got eventsv1.Event
	if err := proto.Unmarshal(m.Data, &got); err != nil {
		t.Fatal(err)
	}
	if !proto.Equal(&got, ev) {
		t.Fatalf("payload = %v, want %v", &got, ev)
	}
}

func TestPlatformSubject(t *testing.T) {
	e := setup(t)
	e.runRelay(t, nil)
	ctx := context.Background()
	ev := &eventsv1.Event{Type: "audit.settings.changed"}
	if err := pgx.BeginFunc(ctx, e.pool, func(tx pgx.Tx) error { return outbox.Write(ctx, tx, ev) }); err != nil {
		t.Fatal(err)
	}
	eventually(t, "message", func() bool { return len(e.messages(t)) == 1 })
	if s := e.messages(t)[0].Subject; s != "events.platform.audit.settings.changed" {
		t.Fatalf("subject = %q", s)
	}
}

func TestWriteRejectsInvalid(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	for _, ev := range []*eventsv1.Event{
		{Type: ""},
		{Type: "iam.*.delete"},
		{Type: "iam..delete"},
		{Type: "iam.members.delete", Org: "not-a-uuid"},
		{Type: "iam.members.delete", Org: "0B9D6A43-5D43-4A43-9A43-1D43C5D43A43"},
		{Type: "iam.members.delete", Id: "nope"},
		{Type: "iam.members.delete", Id: "0190F5A2-7C3B-7D4E-8F60-123456789ABC"},
	} {
		err := pgx.BeginFunc(ctx, e.pool, func(tx pgx.Tx) error { return outbox.Write(ctx, tx, ev) })
		if err == nil {
			t.Errorf("accepted %v", ev)
		}
	}
}

func TestDeliveredAfterNATSReturns(t *testing.T) {
	e := setup(t)
	e.runRelay(t, nil)
	e.nats.Shutdown()
	e.nats.WaitForShutdown()

	ev := e.write(t, true)
	time.Sleep(2 * time.Second)
	if n := e.count(t, "sent_at IS NULL"); n != 1 {
		t.Fatalf("unsent rows while NATS is down = %d, want 1", n)
	}

	e.nats = startNATS(t, e.dir, e.port)
	eventually(t, "row sent", func() bool { return e.count(t, "sent_at IS NOT NULL") == 1 })
	msgs := e.messages(t)
	if len(msgs) != 1 || msgs[0].Header.Get(jetstream.MsgIDHeader) != ev.GetId() {
		t.Fatalf("messages after NATS returned = %d", len(msgs))
	}
}

// stopAfter cancels the relay right after its n-th publish, in the middle of a batch.
type stopAfter struct {
	jetstream.JetStream
	n      int32
	count  *atomic.Int32
	cancel context.CancelFunc
}

func (s stopAfter) Publish(ctx context.Context, subject string, data []byte, opts ...jetstream.PublishOpt) (*jetstream.PubAck, error) {
	ack, err := s.JetStream.Publish(ctx, subject, data, opts...)
	if s.count.Add(1) == s.n {
		s.cancel()
	}
	return ack, err
}

func TestRelayStoppedMidBatchLosesAndReordersNothing(t *testing.T) {
	e := setup(t)
	var ids []string
	for range 20 {
		ids = append(ids, e.write(t, true).GetId())
	}
	ctx, cancel := context.WithCancel(context.Background())
	outbox.NewRelay(e.pool, stopAfter{JetStream: e.js, n: 7, count: new(atomic.Int32), cancel: cancel}).Run(ctx)
	if sent := e.count(t, "sent_at IS NOT NULL"); sent == 0 || sent == len(ids) {
		t.Fatalf("relay did not stop mid-batch: %d of %d sent", sent, len(ids))
	}

	for range 20 {
		ids = append(ids, e.write(t, true).GetId())
	}
	e.runRelay(t, nil)
	eventually(t, "all rows sent", func() bool { return e.count(t, "sent_at IS NULL") == 0 })

	msgs := e.messages(t)
	if len(msgs) != len(ids) {
		t.Fatalf("messages = %d, want %d", len(msgs), len(ids))
	}
	for i, m := range msgs {
		if got := m.Header.Get(jetstream.MsgIDHeader); got != ids[i] {
			t.Fatalf("message %d = %s, want %s", i, got, ids[i])
		}
	}
}

func TestSentRowsPruned(t *testing.T) {
	e := setup(t)
	var now atomic.Int64
	now.Store(time.Now().UnixNano())
	e.runRelay(t, func(r *outbox.Relay) {
		outbox.SetClock(r, func() time.Time { return time.Unix(0, now.Load()) })
	})
	e.write(t, true)
	eventually(t, "row sent", func() bool { return e.count(t, "sent_at IS NOT NULL") == 1 })

	now.Add(int64(23 * time.Hour))
	time.Sleep(1500 * time.Millisecond)
	if n := e.count(t, "true"); n != 1 {
		t.Fatalf("row pruned before 24 h: rows = %d", n)
	}
	now.Add(int64(2 * time.Hour))
	eventually(t, "row pruned", func() bool { return e.count(t, "true") == 0 })
}

func TestNotifyIsPerSchema(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	c, err := e.pool.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Release()
	if _, err := c.Exec(ctx, "LISTEN outbox_svc"); err != nil {
		t.Fatal(err)
	}
	defer func() { _, _ = c.Exec(ctx, "UNLISTEN *") }()
	e.write(t, true)
	wait, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	n, err := c.Conn().WaitForNotification(wait)
	if err != nil {
		t.Fatalf("no notification on outbox_svc: %v", err)
	}
	if n.Channel != "outbox_svc" {
		t.Fatalf("channel = %q", n.Channel)
	}
}

// failing stands in for a bus that refuses every publish while down is set.
type failing struct {
	jetstream.JetStream
	down *atomic.Bool
}

func (f failing) Publish(ctx context.Context, subject string, data []byte, opts ...jetstream.PublishOpt) (*jetstream.PubAck, error) {
	if f.down.Load() {
		return nil, errors.New("bus down")
	}
	return f.JetStream.Publish(ctx, subject, data, opts...)
}

func TestRelayLogsBusDownOnce(t *testing.T) {
	e := setup(t)
	var logs syncBuffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
	defer slog.SetDefault(prev)
	down := new(atomic.Bool)
	down.Store(true)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { outbox.NewRelay(e.pool, failing{e.js, down}).Run(ctx); close(done) }()
	defer func() { cancel(); <-done }()

	e.write(t, true)
	time.Sleep(3500 * time.Millisecond)
	if n := strings.Count(logs.String(), "outbox relay failed"); n != 1 {
		t.Fatalf("logged the outage %d times, want once:\n%s", n, logs.String())
	}
	down.Store(false)
	eventually(t, "row sent", func() bool { return e.count(t, "sent_at IS NULL") == 0 })
	eventually(t, "recovery logged", func() bool { return strings.Contains(logs.String(), "outbox relay recovered") })
}

type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

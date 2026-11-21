// Package outbox writes events in the business transaction and relays them to JetStream after commit.
package outbox

import (
	"context"
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"path"
	"regexp"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/nats-io/nats.go/jetstream"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	eventsv1 "github.com/asterism-dev/observatory/gen/go/observatory/events/v1"
)

const (
	pollInterval   = time.Second
	publishTimeout = 5 * time.Second
	retention      = 24 * time.Hour
	batchSize      = 100
)

// Segments may be camelCase like the permission names they mirror (iam.members.setRole).
var eventType = regexp.MustCompile(`^[a-z][a-zA-Z0-9_]*(\.[a-z][a-zA-Z0-9_]*)*$`)

//go:embed migrations/outbox.sql
var migrationFS embed.FS

// Migrations adds the outbox table to a service's own set as <version>_outbox.sql, keeping one goose version table per schema.
func Migrations(service fs.FS, version int64) fs.FS {
	return withOutbox{FS: service, name: fmt.Sprintf("%05d_outbox.sql", version)}
}

type withOutbox struct {
	fs.FS
	name string
}

func (w withOutbox) Open(name string) (fs.File, error) {
	if name == w.name {
		return migrationFS.Open("migrations/outbox.sql")
	}
	return w.FS.Open(name)
}

// Glob is what goose uses to list migrations.
func (w withOutbox) Glob(pattern string) ([]string, error) {
	names, err := fs.Glob(w.FS, pattern)
	if err != nil {
		return nil, err
	}
	if ok, _ := path.Match(pattern, w.name); ok {
		names = append(names, w.name)
	}
	return names, nil
}

// Write stores e in tx for the relay, assigning a UUIDv7 id and the current time when they are empty.
func Write(ctx context.Context, tx pgx.Tx, e *eventsv1.Event) error {
	if !eventType.MatchString(e.GetType()) {
		return fmt.Errorf("invalid event type %q", e.GetType())
	}
	scope := "platform"
	if e.GetOrg() != "" {
		if u, err := uuid.Parse(e.GetOrg()); err != nil || u.String() != e.GetOrg() {
			return fmt.Errorf("event org %q is not a canonical UUID", e.GetOrg())
		}
		scope = e.GetOrg()
	}
	if e.GetId() == "" {
		id, err := uuid.NewV7()
		if err != nil {
			return fmt.Errorf("event id: %w", err)
		}
		e.Id = id.String()
	} else if u, err := uuid.Parse(e.GetId()); err != nil || u.String() != e.GetId() {
		return fmt.Errorf("event id %q is not a canonical UUID", e.GetId())
	}
	if e.GetTime() == nil {
		e.Time = timestamppb.Now()
	}
	payload, err := proto.Marshal(e)
	if err != nil {
		return fmt.Errorf("marshal event: %w", err)
	}
	if _, err := tx.Exec(ctx, "INSERT INTO outbox (id, subject, payload) VALUES ($1, $2, $3)",
		e.GetId(), "events."+scope+"."+e.GetType(), payload); err != nil {
		return fmt.Errorf("write outbox: %w", err)
	}
	// Delivered on commit only, so the relay wakes for committed events and never for rolled-back ones.
	if _, err := tx.Exec(ctx, "NOTIFY outbox"); err != nil {
		return fmt.Errorf("notify outbox: %w", err)
	}
	return nil
}

// Relay publishes committed outbox rows to JetStream in id order, at least once.
type Relay struct {
	pool *pgxpool.Pool
	js   jetstream.JetStream
	now  func() time.Time
}

func NewRelay(pool *pgxpool.Pool, js jetstream.JetStream) *Relay {
	return &Relay{pool: pool, js: js, now: time.Now}
}

// Run relays on every outbox notification and at least every second until ctx ends.
func (r *Relay) Run(ctx context.Context) {
	for ctx.Err() == nil {
		if err := r.listen(ctx); err != nil && ctx.Err() == nil {
			slog.Warn("outbox listener failed", "err", err, "retry_in", pollInterval)
			select {
			case <-ctx.Done():
			case <-time.After(pollInterval):
			}
		}
	}
}

func (r *Relay) listen(ctx context.Context) error {
	c, err := r.pool.Acquire(ctx)
	if err != nil {
		return err
	}
	// Hijacked so the LISTEN never leaks back into the pool.
	conn := c.Hijack()
	defer func() { _ = conn.Close(context.Background()) }()
	if _, err := conn.Exec(ctx, "LISTEN outbox"); err != nil {
		return err
	}
	for {
		if err := r.relay(ctx); err != nil && ctx.Err() == nil {
			slog.Warn("outbox relay failed", "err", err)
		}
		waitCtx, cancel := context.WithTimeout(ctx, pollInterval)
		_, err := conn.WaitForNotification(waitCtx)
		cancel()
		if ctx.Err() != nil {
			return nil
		}
		if err != nil && !pgconn.Timeout(err) {
			return err
		}
	}
}

func (r *Relay) relay(ctx context.Context) error {
	for {
		n, err := r.publishBatch(ctx)
		if err != nil {
			return err
		}
		if n < batchSize {
			break
		}
	}
	if _, err := r.pool.Exec(ctx, "DELETE FROM outbox WHERE sent_at < $1", r.now().Add(-retention)); err != nil {
		return fmt.Errorf("prune outbox: %w", err)
	}
	return nil
}

// publishBatch locks the oldest unsent rows so concurrent relays of one service don't interleave.
func (r *Relay) publishBatch(ctx context.Context) (int, error) {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback(context.Background()) }()

	rows, err := tx.Query(ctx, "SELECT id::text, subject, payload FROM outbox WHERE sent_at IS NULL ORDER BY id LIMIT $1 FOR UPDATE", batchSize)
	if err != nil {
		return 0, err
	}
	type row struct {
		id, subject string
		payload     []byte
	}
	var batch []row
	for rows.Next() {
		var m row
		if err := rows.Scan(&m.id, &m.subject, &m.payload); err != nil {
			rows.Close()
			return 0, err
		}
		batch = append(batch, m)
	}
	if err := rows.Err(); err != nil {
		return 0, err
	}

	var sent []string
	var pubErr error
	for _, m := range batch {
		pubCtx, cancel := context.WithTimeout(ctx, publishTimeout)
		_, pubErr = r.js.Publish(pubCtx, m.subject, m.payload, jetstream.WithMsgID(m.id))
		cancel()
		if pubErr != nil {
			pubErr = fmt.Errorf("publish %s: %w", m.id, pubErr)
			break
		}
		sent = append(sent, m.id)
	}
	if len(sent) > 0 {
		// Rows already acked are marked even when the relay is stopping, so a restart resends as little as possible.
		ctx := context.WithoutCancel(ctx)
		if _, err := tx.Exec(ctx, "UPDATE outbox SET sent_at = $1 WHERE id = ANY($2::uuid[])", r.now(), sent); err != nil {
			return 0, errors.Join(pubErr, err)
		}
		if err := tx.Commit(ctx); err != nil {
			return 0, errors.Join(pubErr, err)
		}
	}
	return len(batch), pubErr
}

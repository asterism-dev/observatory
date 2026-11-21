package audit

import (
	"context"
	"embed"
	"encoding/base64"
	"errors"
	"fmt"
	"io/fs"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/structpb"
	"google.golang.org/protobuf/types/known/timestamppb"

	eventsv1 "github.com/asterism-dev/observatory/gen/go/observatory/events/v1"
	"github.com/asterism-dev/observatory/internal/platform/outbox"
)

//go:embed migrations/*.sql
var migrationFS embed.FS

func migrations() (fs.FS, error) {
	own, err := fs.Sub(migrationFS, "migrations")
	if err != nil {
		return nil, err
	}
	return outbox.Migrations(own, 2), nil
}

type store struct {
	pool *pgxpool.Pool
}

// insert stores e unless a row with its id exists, so redeliveries are harmless; NUL, which Postgres text and jsonb reject, is dropped.
func (s *store) insert(ctx context.Context, e *eventsv1.Event) error {
	details := []byte("{}")
	if e.GetDetails() != nil {
		clean := proto.Clone(e.GetDetails()).(*structpb.Struct)
		stripNULStruct(clean)
		var err error
		if details, err = protojson.Marshal(clean); err != nil {
			return fmt.Errorf("marshal details: %w", err)
		}
	}
	var org *string
	if e.GetOrg() != "" {
		org = &e.Org
	}
	_, err := s.pool.Exec(ctx, `INSERT INTO events (id, time, type, org, actor_kind, actor_id, resource, result, client_ip, user_agent, request_id, details)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12) ON CONFLICT (id) DO NOTHING`,
		e.GetId(), e.GetTime().AsTime(), stripNUL(e.GetType()), org, enumName(e.GetActor().GetKind().String(), "ACTOR_KIND_"),
		stripNUL(e.GetActor().GetId()), stripNUL(e.GetResource()), enumName(e.GetResult().String(), "RESULT_"),
		stripNUL(e.GetClientIp()), stripNUL(e.GetUserAgent()), stripNUL(e.GetRequestId()), details)
	if err != nil {
		return fmt.Errorf("insert event %s: %w", e.GetId(), err)
	}
	return nil
}

func stripNUL(s string) string { return strings.ReplaceAll(s, "\x00", "") }

func stripNULStruct(st *structpb.Struct) {
	for k, v := range st.GetFields() {
		stripNULValue(v)
		if c := stripNUL(k); c != k {
			delete(st.Fields, k)
			st.Fields[c] = v
		}
	}
}

func stripNULValue(v *structpb.Value) {
	switch k := v.GetKind().(type) {
	case *structpb.Value_StringValue:
		k.StringValue = stripNUL(k.StringValue)
	case *structpb.Value_StructValue:
		stripNULStruct(k.StructValue)
	case *structpb.Value_ListValue:
		for _, x := range k.ListValue.GetValues() {
			stripNULValue(x)
		}
	}
}

// enumName stores enums as lower-case names (ACTOR_KIND_USER → user) so the table reads without the proto.
func enumName(full, prefix string) string {
	return strings.ToLower(strings.TrimPrefix(full, prefix))
}

func enumValue(values map[string]int32, prefix, name string) int32 {
	return values[prefix+strings.ToUpper(name)]
}

// deleteBefore is the retention job's only way past the append-only trigger.
func (s *store) deleteBefore(ctx context.Context, cutoff time.Time) (int64, error) {
	var n int64
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, "SET LOCAL observatory.retention = 'on'"); err != nil {
			return err
		}
		tag, err := tx.Exec(ctx, "DELETE FROM events WHERE time < $1", cutoff)
		n = tag.RowsAffected()
		return err
	})
	if err != nil {
		return 0, fmt.Errorf("delete expired events: %w", err)
	}
	return n, nil
}

const (
	defaultPageSize = 50
	maxPageSize     = 200
)

var errBadPageToken = errors.New("invalid page token")

type listQuery struct {
	// org empty means every org and platform events.
	org, actorID, typ string
	result            eventsv1.Result
	from, to          *timestamppb.Timestamp
	pageSize          int32
	pageToken         string
}

// list returns one page newest first, keyset-paginated by (time, id).
func (s *store) list(ctx context.Context, q listQuery) ([]*eventsv1.Event, string, error) {
	var where []string
	var args []any
	add := func(cond string, arg any) {
		args = append(args, arg)
		where = append(where, fmt.Sprintf(cond, len(args)))
	}
	if q.org != "" {
		add("org = $%d", q.org)
	}
	if q.actorID != "" {
		add("actor_id = $%d", q.actorID)
	}
	if q.typ != "" {
		add("type = $%d", q.typ)
	}
	if q.result != eventsv1.Result_RESULT_UNSPECIFIED {
		add("result = $%d", enumName(q.result.String(), "RESULT_"))
	}
	if q.from != nil {
		add("time >= $%d", q.from.AsTime())
	}
	if q.to != nil {
		add("time < $%d", q.to.AsTime())
	}
	if q.pageToken != "" {
		at, id, err := decodePageToken(q.pageToken)
		if err != nil {
			return nil, "", err
		}
		args = append(args, at, id)
		where = append(where, fmt.Sprintf("(time, id) < ($%d, $%d)", len(args)-1, len(args)))
	}
	size := q.pageSize
	if size <= 0 {
		size = defaultPageSize
	}
	size = min(size, maxPageSize)
	sql := `SELECT id::text, time, type, coalesce(org::text, ''), actor_kind, actor_id, resource, result, client_ip, user_agent, request_id, details FROM events`
	if len(where) > 0 {
		sql += " WHERE " + strings.Join(where, " AND ")
	}
	args = append(args, size+1)
	sql += fmt.Sprintf(" ORDER BY time DESC, id DESC LIMIT $%d", len(args))

	rows, err := s.pool.Query(ctx, sql, args...)
	if err != nil {
		return nil, "", fmt.Errorf("list events: %w", err)
	}
	defer rows.Close()
	var out []*eventsv1.Event
	for rows.Next() {
		e := &eventsv1.Event{Actor: &eventsv1.Actor{}, Details: &structpb.Struct{}, Audit: true}
		var at time.Time
		var kind, result string
		var details []byte
		if err := rows.Scan(&e.Id, &at, &e.Type, &e.Org, &kind, &e.Actor.Id, &e.Resource, &result, &e.ClientIp, &e.UserAgent, &e.RequestId, &details); err != nil {
			return nil, "", fmt.Errorf("scan event: %w", err)
		}
		e.Time = timestamppb.New(at)
		e.Actor.Kind = eventsv1.ActorKind(enumValue(eventsv1.ActorKind_value, "ACTOR_KIND_", kind))
		e.Result = eventsv1.Result(enumValue(eventsv1.Result_value, "RESULT_", result))
		if err := protojson.Unmarshal(details, e.Details); err != nil {
			return nil, "", fmt.Errorf("event %s details: %w", e.GetId(), err)
		}
		out = append(out, e)
	}
	if err := rows.Err(); err != nil {
		return nil, "", fmt.Errorf("list events: %w", err)
	}
	next := ""
	if len(out) > int(size) {
		out = out[:size]
		last := out[len(out)-1]
		next = encodePageToken(last.GetTime().AsTime(), last.GetId())
	}
	return out, next, nil
}

// Page tokens carry the last row's time in microseconds, Postgres's precision, so the keyset comparison is exact.
func encodePageToken(at time.Time, id string) string {
	return base64.RawURLEncoding.EncodeToString([]byte(strconv.FormatInt(at.UnixMicro(), 10) + "/" + id))
}

func decodePageToken(token string) (time.Time, uuid.UUID, error) {
	raw, err := base64.RawURLEncoding.DecodeString(token)
	if err != nil {
		return time.Time{}, uuid.Nil, errBadPageToken
	}
	micros, rawID, ok := strings.Cut(string(raw), "/")
	if !ok {
		return time.Time{}, uuid.Nil, errBadPageToken
	}
	us, err := strconv.ParseInt(micros, 10, 64)
	if err != nil {
		return time.Time{}, uuid.Nil, errBadPageToken
	}
	id, err := uuid.Parse(rawID)
	if err != nil {
		return time.Time{}, uuid.Nil, errBadPageToken
	}
	return time.UnixMicro(us), id, nil
}

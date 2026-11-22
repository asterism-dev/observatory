// Package storetest builds hub stores against a fake quota service and reads the hub's events back from its outbox for tests.
package storetest

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"google.golang.org/protobuf/proto"

	eventsv1 "github.com/asterism-dev/observatory/gen/go/observatory/events/v1"
	"github.com/asterism-dev/observatory/gen/go/observatory/quota/v1/quotav1connect"
	"github.com/asterism-dev/observatory/internal/platform/quotatest"
	"github.com/asterism-dev/observatory/internal/store"
)

// Quota serves f in process and returns the hub's quota client for it.
func Quota(t testing.TB, f *quotatest.Fake) store.Quota {
	t.Helper()
	path, h := quotav1connect.NewQuotaInternalServiceHandler(f)
	mux := http.NewServeMux()
	mux.Handle(path, h)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return store.NewQuota(quotav1connect.NewQuotaInternalServiceClient(srv.Client(), srv.URL))
}

// New is a hub store on pool whose quota is a fresh fake without limits.
func New(t testing.TB, pool *pgxpool.Pool) *store.Store {
	t.Helper()
	return store.New(pool, Quota(t, quotatest.NewFake()))
}

// AuditEvents returns every outbox event, newest first.
func AuditEvents(t testing.TB, pool *pgxpool.Pool) []*eventsv1.Event {
	t.Helper()
	rows, err := pool.Query(context.Background(), "SELECT payload FROM outbox ORDER BY id DESC")
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
		out = append(out, e)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

// Audit returns the audit entries with action (all for ""), newest first, as the hub wrote them.
func Audit(t testing.TB, pool *pgxpool.Pool, action string) []store.AuditEntry {
	t.Helper()
	var out []store.AuditEntry
	for _, e := range AuditEvents(t, pool) {
		if !e.GetAudit() || action != "" && e.GetType() != action {
			continue
		}
		a := store.AuditEntry{
			ActorKind: store.ActorKind(strings.ToLower(strings.TrimPrefix(e.GetActor().GetKind().String(), "ACTOR_KIND_"))),
			ActorID:   e.GetActor().GetId(), Action: e.GetType(), Resource: e.GetResource(),
			Result: store.Result(strings.ToLower(strings.TrimPrefix(e.GetResult().String(), "RESULT_"))),
			IP:     e.GetClientIp(), UserAgent: e.GetUserAgent(), Details: e.GetDetails().AsMap(),
		}
		if e.GetOrg() != "" {
			org, err := uuid.Parse(e.GetOrg())
			if err != nil {
				t.Fatal(err)
			}
			a.OrgID = &org
		}
		out = append(out, a)
	}
	return out
}

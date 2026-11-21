package audit

import (
	"context"
	"net/http"
	"testing"
	"time"

	"connectrpc.com/connect"
	"github.com/google/uuid"

	auditv1 "github.com/asterism-dev/observatory/gen/go/observatory/audit/v1"
	"github.com/asterism-dev/observatory/gen/go/observatory/audit/v1/auditv1connect"
	eventsv1 "github.com/asterism-dev/observatory/gen/go/observatory/events/v1"
	"github.com/asterism-dev/observatory/internal/db/dbtest"
	"github.com/asterism-dev/observatory/internal/platform/auditlog"
	"github.com/asterism-dev/observatory/internal/platform/bus"
	"github.com/asterism-dev/observatory/internal/platform/clientip"
)

func as[T any](t *testing.T, f *fixture, user uuid.UUID, platformAdmin bool, msg *T) *connect.Request[T] {
	t.Helper()
	r := connect.NewRequest(msg)
	r.Header().Set("Authorization", "Bearer "+f.token(t, user, platformAdmin))
	r.Header().Set(clientip.HeaderClientIP, "198.51.100.4")
	return r
}

// recorded counts stored events of typ in org by actor with result.
func (f *fixture) recorded(t *testing.T, org, typ string, actor uuid.UUID, result string) int {
	t.Helper()
	var n int
	err := f.store.pool.QueryRow(context.Background(),
		"SELECT count(*) FROM events WHERE coalesce(org::text, '') = $1 AND type = $2 AND actor_id = $3 AND result = $4 AND client_ip = '198.51.100.4'",
		org, typ, actor.String(), result).Scan(&n)
	if err != nil {
		t.Fatal(err)
	}
	return n
}

func (f *fixture) waitRecorded(t *testing.T, org, typ string, actor uuid.UUID, result string) {
	t.Helper()
	waitFor(t, typ+" "+result+" recorded", func() bool { return f.recorded(t, org, typ, actor, result) > 0 })
}

func (f *fixture) setDataEvents(t *testing.T, user uuid.UUID, org string, on map[string]bool) (*auditv1.UpdateSettingsResponse, error) {
	t.Helper()
	resp, err := f.client.UpdateSettings(context.Background(), as(t, f, user, false, &auditv1.UpdateSettingsRequest{Org: org, DataEvents: on}))
	if err != nil {
		return nil, err
	}
	return resp.Msg, nil
}

func TestInterceptorRecordsWritesAndDenials(t *testing.T) {
	f := newFixture(t)
	if _, err := f.list(t, f.admin, &auditv1.ListEventsRequest{Org: f.orgA}); err != nil {
		t.Fatal(err)
	}

	got, err := f.setDataEvents(t, f.admin, f.orgA, map[string]bool{"compute": true})
	if err != nil {
		t.Fatal(err)
	}
	if len(got.GetDataEvents()) != len(services) || !got.GetDataEvents()["compute"] || got.GetDataEvents()["network"] {
		t.Fatalf("data events = %v", got.GetDataEvents())
	}
	f.waitRecorded(t, f.orgA, "audit.settings.update", f.admin, "allowed")

	_, err = f.setDataEvents(t, f.member, f.orgA, map[string]bool{"compute": false})
	wantCode(t, err, connect.CodePermissionDenied)
	f.waitRecorded(t, f.orgA, "audit.settings.update", f.member, "denied")

	_, err = f.client.GetSettings(context.Background(), as(t, f, f.outsider, false, &auditv1.GetSettingsRequest{Org: f.orgA}))
	wantCode(t, err, connect.CodeNotFound)
	f.waitRecorded(t, f.orgA, "audit.settings.get", f.outsider, "denied")

	_, err = f.setDataEvents(t, f.admin, f.orgA, map[string]bool{"nope": true})
	wantCode(t, err, connect.CodeInvalidArgument)
	f.waitRecorded(t, f.orgA, "audit.settings.update", f.admin, "failed")

	resp, err := f.client.GetSettings(context.Background(), as(t, f, f.admin, false, &auditv1.GetSettingsRequest{Org: f.orgA}))
	if err != nil {
		t.Fatal(err)
	}
	if !resp.Msg.GetDataEvents()["compute"] {
		t.Fatalf("compute data events lost: %v", resp.Msg.GetDataEvents())
	}
	if _, err := f.client.GetSettings(context.Background(), as(t, f, f.member, false, &auditv1.GetSettingsRequest{Org: f.orgA})); connect.CodeOf(err) != connect.CodePermissionDenied {
		t.Fatalf("member GetSettings: %v", err)
	}
}

func TestDataEventsApplyWithoutRestart(t *testing.T) {
	f := newFixture(t)
	if _, err := f.list(t, f.admin, &auditv1.ListEventsRequest{Org: f.orgA}); err != nil {
		t.Fatal(err)
	}
	for range 3 {
		if _, err := f.client.ListEvents(context.Background(), as(t, f, f.admin, false, &auditv1.ListEventsRequest{Org: f.orgA})); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := f.setDataEvents(t, f.admin, f.orgA, map[string]bool{"audit": true}); err != nil {
		t.Fatal(err)
	}
	// The relay and the consumer keep order, so the write stored means the earlier reads would be too.
	f.waitRecorded(t, f.orgA, "audit.settings.update", f.admin, "allowed")
	if n := f.recorded(t, f.orgA, "audit.events.list", f.admin, "allowed"); n != 0 {
		t.Fatalf("%d reads recorded with data events off", n)
	}

	waitFor(t, "read recorded after enabling", func() bool {
		if _, err := f.client.ListEvents(context.Background(), as(t, f, f.admin, false, &auditv1.ListEventsRequest{Org: f.orgA})); err != nil {
			t.Fatal(err)
		}
		time.Sleep(100 * time.Millisecond)
		return f.recorded(t, f.orgA, "audit.events.list", f.admin, "allowed") > 0
	})
	if f.recorded(t, f.orgB, "audit.events.list", f.adminB, "allowed") != 0 {
		t.Fatal("org B read recorded")
	}
}

func TestInternalDataEvents(t *testing.T) {
	f := newFixture(t)
	if _, err := f.list(t, f.admin, &auditv1.ListEventsRequest{Org: f.orgA}); err != nil {
		t.Fatal(err)
	}
	anon := auditv1connect.NewAuditInternalServiceClient(http.DefaultClient, f.baseURL+"/internal/audit")
	if _, err := anon.GetDataEvents(context.Background(), connect.NewRequest(&auditv1.GetDataEventsRequest{})); err == nil {
		t.Fatal("internal call without a service token succeeded")
	}

	settings, err := auditlog.NewSettings(f.baseURL+"/internal/audit", computeToken)
	if err != nil {
		t.Fatal(err)
	}
	js, closeBus, err := bus.Connect(context.Background(), f.natsURL, f.natsToken)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(closeBus)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { settings.Run(ctx, js.Conn()); close(done) }()
	t.Cleanup(func() { cancel(); <-done })

	if _, err := f.setDataEvents(t, f.admin, f.orgA, map[string]bool{"compute": true}); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "remote settings follow the change", func() bool { return settings.DataEvents(f.orgA, "compute") })
	if settings.DataEvents(f.orgA, "network") || settings.DataEvents(f.orgB, "compute") {
		t.Fatal("data events on for an unchanged org or service")
	}
	if _, err := f.setDataEvents(t, f.admin, f.orgA, map[string]bool{"compute": false}); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "remote settings follow the disable", func() bool { return !settings.DataEvents(f.orgA, "compute") })
}

func TestPlatformSettings(t *testing.T) {
	f := newFixture(t)
	if _, err := f.list(t, f.admin, &auditv1.ListEventsRequest{Org: f.orgA}); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	_, err := f.client.GetPlatformSettings(ctx, as(t, f, f.admin, false, &auditv1.GetPlatformSettingsRequest{}))
	wantCode(t, err, connect.CodePermissionDenied)
	_, err = f.client.UpdatePlatformSettings(ctx, as(t, f, f.admin, false, &auditv1.UpdatePlatformSettingsRequest{RetentionDays: 30}))
	wantCode(t, err, connect.CodePermissionDenied)
	f.waitRecorded(t, "", "audit.settings.update", f.admin, "denied")

	got, err := f.client.GetPlatformSettings(ctx, as(t, f, f.platformAdmin, true, &auditv1.GetPlatformSettingsRequest{}))
	if err != nil {
		t.Fatal(err)
	}
	if got.Msg.GetRetentionDays() != 365 {
		t.Fatalf("default retention = %d", got.Msg.GetRetentionDays())
	}
	for _, days := range []int32{0, 29, 3651} {
		_, err := f.client.UpdatePlatformSettings(ctx, as(t, f, f.platformAdmin, true, &auditv1.UpdatePlatformSettingsRequest{RetentionDays: days}))
		wantCode(t, err, connect.CodeInvalidArgument)
	}
	for _, days := range []int32{3650, 30} {
		resp, err := f.client.UpdatePlatformSettings(ctx, as(t, f, f.platformAdmin, true, &auditv1.UpdatePlatformSettingsRequest{RetentionDays: days}))
		if err != nil {
			t.Fatal(err)
		}
		if resp.Msg.GetRetentionDays() != days {
			t.Fatalf("retention = %d, want %d", resp.Msg.GetRetentionDays(), days)
		}
	}
	f.waitRecorded(t, "", "audit.settings.update", f.platformAdmin, "allowed")
}

func TestRetentionUsesSetting(t *testing.T) {
	ctx := context.Background()
	s := openStore(t, dbtest.Empty(t))
	if err := s.setRetentionDays(ctx, 30); err != nil {
		t.Fatal(err)
	}
	old := newEvent(uuid.NewString(), time.Now().Add(-31*24*time.Hour))
	fresh := newEvent(old.GetOrg(), time.Now().Add(-29*24*time.Hour))
	for _, e := range []*eventsv1.Event{old, fresh} {
		if err := s.insert(ctx, e); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := expire(ctx, s); err != nil {
		t.Fatal(err)
	}
	if count(t, s.pool, old.GetId()) != 0 || count(t, s.pool, fresh.GetId()) != 1 {
		t.Fatal("retention did not apply the 30-day setting")
	}
	if _, err := s.pool.Exec(ctx, "UPDATE platform_settings SET retention_days = 29"); err == nil {
		t.Fatal("retention below 30 days accepted by the database")
	}
}

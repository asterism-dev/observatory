package store_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/asterism-dev/observatory/internal/db/dbtest"
	"github.com/asterism-dev/observatory/internal/store"
)

func TestDashboards(t *testing.T) {
	ctx := context.Background()
	pool := dbtest.New(t)
	s := store.New(pool)
	a, b := login(t, s, "a").User.ID, login(t, s, "b").User.ID
	org := mustCreateOrg(t, s, "D", a)
	other := mustCreateOrg(t, s, "E", b)
	addMember(s, org.ID, b, store.Role("member"))
	team, err := createTeam(s, ctx, org.ID, "core")
	if err != nil {
		t.Fatal(err)
	}

	panels := json.RawMessage(`[{"title":"CPU","type":"stat","query":"up","unit":"none","width":"half"}]`)
	d, err := s.CreateDashboard(ctx, store.Dashboard{OrgID: org.ID, Scope: store.EnvScopeTeam, OwnerID: &team.ID, Name: "ops", Panels: panels, CreatedBy: b})
	if err != nil {
		t.Fatal(err)
	}
	if d.ID == uuid.Nil || d.Scope != store.EnvScopeTeam || d.OwnerID == nil || *d.OwnerID != team.ID || d.CreatedBy != b || d.UpdatedAt.IsZero() {
		t.Fatalf("created %+v", d)
	}
	var gotPanels []map[string]string
	if err := json.Unmarshal(d.Panels, &gotPanels); err != nil || len(gotPanels) != 1 || gotPanels[0]["query"] != "up" {
		t.Fatalf("panels %s %v", d.Panels, err)
	}
	if _, err := s.Dashboard(ctx, other.ID, d.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("other org: %v", err)
	}
	empty, err := s.CreateDashboard(ctx, store.Dashboard{OrgID: org.ID, Scope: store.EnvScopeOrg, Name: "blank", CreatedBy: a})
	if err != nil || string(empty.Panels) != "[]" {
		t.Fatalf("nil panels: %s %v", empty.Panels, err)
	}

	stale := d.UpdatedAt
	d.Name, d.Scope, d.OwnerID = "ops2", store.EnvScopeOrg, nil
	updated, err := s.UpdateDashboard(ctx, d, stale)
	if err != nil || updated.Name != "ops2" || updated.Scope != store.EnvScopeOrg || updated.OwnerID != nil || !updated.UpdatedAt.After(stale) || updated.CreatedBy != b {
		t.Fatalf("update %+v %v", updated, err)
	}
	d.Name = "lost"
	if _, err := s.UpdateDashboard(ctx, d, stale); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("stale update: %v", err)
	}
	if _, err := s.UpdateDashboard(ctx, store.Dashboard{ID: d.ID, OrgID: other.ID, Scope: store.EnvScopeOrg, Name: "x"}, updated.UpdatedAt); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("other org update: %v", err)
	}
	got, err := s.Dashboard(ctx, org.ID, d.ID)
	if err != nil || got.Name != "ops2" || !got.UpdatedAt.Equal(updated.UpdatedAt) {
		t.Fatalf("get %+v %v", got, err)
	}

	private, err := s.CreateDashboard(ctx, store.Dashboard{OrgID: org.ID, Scope: store.EnvScopeUser, OwnerID: &b, Name: "mine", CreatedBy: b})
	if err != nil {
		t.Fatal(err)
	}
	list, err := s.Dashboards(ctx, org.ID)
	if err != nil || len(list) != 3 {
		t.Fatalf("list %+v %v", list, err)
	}

	if err := s.DeleteDashboard(ctx, other.ID, d.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("other org delete: %v", err)
	}
	if err := s.DeleteDashboard(ctx, org.ID, d.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Dashboard(ctx, org.ID, d.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("after delete: %v", err)
	}

	if err := removeMember(s, ctx, org.ID, b); err != nil {
		t.Fatal(err)
	}
	if _, err := s.PurgeMember(ctx, org.ID, b, time.Now(), memberIn(s, org.ID, b)); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Dashboard(ctx, org.ID, private.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("private dashboard survives member removal: %v", err)
	}
}

func TestDashboardLimit(t *testing.T) {
	ctx := context.Background()
	s := store.New(dbtest.New(t))
	a := login(t, s, "a").User.ID
	org := mustCreateOrg(t, s, "D", a)
	for i := range 50 {
		if _, err := s.CreateDashboard(ctx, store.Dashboard{OrgID: org.ID, Scope: store.EnvScopeUser, OwnerID: &a, Name: fmt.Sprint(i), CreatedBy: a}); err != nil {
			t.Fatalf("%d: %v", i, err)
		}
	}
	if _, err := s.CreateDashboard(ctx, store.Dashboard{OrgID: org.ID, Scope: store.EnvScopeOrg, Name: "51", CreatedBy: a}); !errors.Is(err, store.ErrLimitExceeded) {
		t.Fatalf("51st: %v", err)
	}
}

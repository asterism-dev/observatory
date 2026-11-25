package store_test

import (
	"context"
	"slices"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/asterism-dev/observatory/internal/integrations"
	"github.com/asterism-dev/observatory/internal/store"
)

func accountID(t *testing.T, pool *pgxpool.Pool, org uuid.UUID, name string) uuid.UUID {
	t.Helper()
	var id uuid.UUID
	if err := pool.QueryRow(context.Background(), "SELECT id FROM accounts WHERE org_id = $1 AND name = $2", org, name).Scan(&id); err != nil {
		t.Fatalf("account %q: %v", name, err)
	}
	return id
}

func accountOf(t *testing.T, pool *pgxpool.Pool, table string, id uuid.UUID) uuid.UUID {
	t.Helper()
	var acc uuid.UUID
	if err := pool.QueryRow(context.Background(), "SELECT account_id FROM "+table+" WHERE id = $1", id).Scan(&acc); err != nil {
		t.Fatalf("%s %v: %v", table, id, err)
	}
	return acc
}

func TestAccountTriggers(t *testing.T) {
	ctx := context.Background()
	e := newVMEnv(t)
	def, teamAcc := accountID(t, e.pool, e.org.ID, "default"), accountID(t, e.pool, e.org.ID, "core")

	team := e.team.ID
	userVM, err := e.s.CreateVM(ctx, e.vm("u", nil, 1, 1, 1))
	if err != nil {
		t.Fatal(err)
	}
	teamVM, err := e.s.CreateVM(ctx, e.vm("t", &team, 1, 1, 1))
	if err != nil {
		t.Fatal(err)
	}
	if got := accountOf(t, e.pool, "vms", userVM.ID); got != def {
		t.Errorf("user VM account = %v, want default %v", got, def)
	}
	if got := accountOf(t, e.pool, "vms", teamVM.ID); got != teamAcc {
		t.Errorf("team VM account = %v, want team %v", got, teamAcc)
	}
	if got := accountOf(t, e.pool, "nodes", e.node.ID); got != def {
		t.Errorf("node account = %v, want default", got)
	}

	img, err := e.s.PlatformImage(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var platform uuid.UUID
	if err := e.pool.QueryRow(ctx, "SELECT id FROM accounts WHERE platform").Scan(&platform); err != nil {
		t.Fatal(err)
	}
	if got := accountOf(t, e.pool, "images", img.ID); got != platform {
		t.Errorf("platform image account = %v, want platform %v", got, platform)
	}

	if _, err := e.pool.Exec(ctx, "UPDATE teams SET name = 'core2' WHERE id = $1", team); err != nil {
		t.Fatal(err)
	}
	if accountID(t, e.pool, e.org.ID, "core2") != teamAcc {
		t.Error("renaming the team must rename its account")
	}
	var tags string
	if err := e.pool.QueryRow(ctx, "SELECT tags::text FROM vms WHERE id = $1", userVM.ID).Scan(&tags); err != nil || tags != "{}" {
		t.Errorf("tags = %q, %v", tags, err)
	}
}

func TestAccountSnapshot(t *testing.T) {
	ctx := context.Background()
	e := newVMEnv(t)
	def, teamAcc := accountID(t, e.pool, e.org.ID, "default"), accountID(t, e.pool, e.org.ID, "core")
	team := e.team.ID
	teamVM, err := e.s.CreateVM(ctx, e.vm("t", &team, 1, 1, 1))
	if err != nil {
		t.Fatal(err)
	}
	sub := newClaude(t, e.s.WithMasterKey(testKey(1)), e.org, e.user, store.EnvScopeUser, integrations.ModeProxy)

	snap, err := e.s.AccountSnapshot(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if got, ok := snap.Default(e.org.ID); !ok || got != def {
		t.Errorf("Default = %v, %v", got, ok)
	}
	if got, ok := snap.OfTeam(team); !ok || got != teamAcc {
		t.Errorf("OfTeam = %v, %v", got, ok)
	}
	if in := snap.InOrg(e.org.ID); len(in) != 2 || !slices.Contains(in, def) || !slices.Contains(in, teamAcc) {
		t.Errorf("InOrg = %v", in)
	}
	if got := snap.Resources["vm/"+teamVM.ID.String()]; got != teamAcc {
		t.Errorf("team VM resource account = %v", got)
	}
	if got := snap.Resources["node/"+e.node.ID.String()]; got != def {
		t.Errorf("node resource account = %v", got)
	}
	if !slices.Contains(snap.Subscriptions, store.Subscription{ID: sub.ID, Account: def, Owner: e.user}) {
		t.Errorf("Subscriptions = %+v", snap.Subscriptions)
	}
}

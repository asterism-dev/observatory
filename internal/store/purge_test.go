package store_test

import (
	"context"
	"errors"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/asterism-dev/observatory/internal/db/dbtest"
	"github.com/asterism-dev/observatory/internal/integrations"
	"github.com/asterism-dev/observatory/internal/store"
	"github.com/asterism-dev/observatory/internal/store/storetest"
)

func TestPurgeSkipsWhenIAMSaysMember(t *testing.T) {
	ctx := context.Background()
	s := store.New(dbtest.New(t)).WithMasterKey(testKey(1))
	a, m := login(t, s, "a"), login(t, s, "m")
	org := mustCreateOrg(t, s, "A", a.User.ID)
	mid := m.User.ID
	v := mustCreateEnv(t, s, envVar(org, mid, store.EnvPlain, store.EnvScopeUser, &mid, "P"), "p")
	asked := false
	purged, err := s.PurgeMember(ctx, org.ID, mid, 0, func(context.Context) (bool, error) { asked = true; return true, nil })
	if err != nil || purged || !asked {
		t.Fatalf("purge: purged=%v asked=%v err=%v", purged, asked, err)
	}
	if _, err := s.EnvVar(ctx, org.ID, v.ID); err != nil {
		t.Fatalf("member's variable: %v", err)
	}
}

// TestUserScopeCreateWaitsForPurge: every write that lands a row in a user's scope blocks on a running purge, then keeps its row.
func TestUserScopeCreateWaitsForPurge(t *testing.T) {
	cases := []struct {
		name string
		// prepare runs before the purge and returns the write and the count of the row it lands in the user's scope.
		prepare func(t *testing.T, s *store.Store, org testOrg, user uuid.UUID) (write func() error, count func() int)
	}{
		{"env create", func(t *testing.T, s *store.Store, org testOrg, user uuid.UUID) (func() error, func() int) {
			var v store.EnvVar
			return func() (err error) {
					v, err = s.CreateEnvVar(t.Context(), envVar(org, user, store.EnvPlain, store.EnvScopeUser, &user, "NEW"), "n")
					return err
				}, func() int {
					_, err := s.EnvVar(t.Context(), org.ID, v.ID)
					return boolCount(err == nil)
				}
		}},
		{"env move", func(t *testing.T, s *store.Store, org testOrg, user uuid.UUID) (func() error, func() int) {
			old := mustCreateEnv(t, s, envVar(org, user, store.EnvPlain, store.EnvScopeOrg, nil, "SHARED"), "s")
			return func() error {
					moved := old
					moved.Scope, moved.OwnerID, moved.UpdatedBy = store.EnvScopeUser, &user, user
					return s.MoveEnvVar(t.Context(), old, moved)
				}, func() int {
					v, err := s.EnvVar(t.Context(), org.ID, old.ID)
					return boolCount(err == nil && v.Scope == store.EnvScopeUser)
				}
		}},
		{"dashboard scope change", func(t *testing.T, s *store.Store, org testOrg, user uuid.UUID) (func() error, func() int) {
			d, err := s.CreateDashboard(t.Context(), store.Dashboard{OrgID: org.ID, Scope: store.EnvScopeOrg, Name: "d", CreatedBy: user})
			if err != nil {
				t.Fatal(err)
			}
			return func() error {
					moved := d
					moved.Scope, moved.OwnerID = store.EnvScopeUser, &user
					_, err := s.UpdateDashboard(t.Context(), moved, d.UpdatedAt)
					return err
				}, func() int {
					got, err := s.Dashboard(t.Context(), org.ID, d.ID)
					return boolCount(err == nil && got.Scope == store.EnvScopeUser)
				}
		}},
		{"device create", func(t *testing.T, s *store.Store, org testOrg, user uuid.UUID) (func() error, func() int) {
			if _, err := s.OrgSlice(t.Context(), org.ID); err != nil {
				t.Fatal(err)
			}
			return func() error {
					_, err := s.CreateDevice(t.Context(), store.Device{OrgID: org.ID, UserID: user, Name: "laptop", PublicKey: "key-" + user.String()})
					return err
				}, func() int {
					ds, _ := s.Devices(t.Context(), org.ID)
					return len(ds)
				}
		}},
		{"integration create", func(t *testing.T, s *store.Store, org testOrg, user uuid.UUID) (func() error, func() int) {
			var v store.Integration
			return func() (err error) {
					v, err = s.CreateIntegration(t.Context(), store.Integration{OrgID: org.ID, Kind: integrations.GitHub, Source: integrations.SourcePAT,
						Scope: store.EnvScopeUser, OwnerID: &user, CreatedBy: user, UpdatedBy: user}, store.IntegrationSecret{Token: "ghp_dummy0000000000000000"})
					return err
				}, func() int {
					_, err := s.Integration(t.Context(), org.ID, v.ID)
					return boolCount(err == nil)
				}
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			ctx := context.Background()
			s := store.New(dbtest.New(t)).WithMasterKey(testKey(1))
			a, m := login(t, s, "a"), login(t, s, "m")
			org := mustCreateOrg(t, s, "A", a.User.ID)
			mid := m.User.ID
			old := mustCreateEnv(t, s, envVar(org, mid, store.EnvPlain, store.EnvScopeUser, &mid, "OLD"), "o")
			write, count := c.prepare(t, s, org, mid)
			locked, release := make(chan struct{}), make(chan struct{})
			var releaseOnce sync.Once
			unblock := func() { releaseOnce.Do(func() { close(release) }) }
			// A failing check must not leave the purge holding its transaction while the test database is dropped.
			t.Cleanup(unblock)
			purgeDone := make(chan error, 1)
			go func() {
				_, err := s.PurgeMember(ctx, org.ID, mid, 0, func(context.Context) (bool, error) {
					close(locked)
					<-release
					return false, nil
				})
				purgeDone <- err
			}()
			<-locked
			writeDone := make(chan error, 1)
			go func() { writeDone <- write() }()
			select {
			case err := <-writeDone:
				t.Fatalf("write didn't wait for the purge: %v", err)
			case <-time.After(200 * time.Millisecond):
			}
			unblock()
			if err := <-purgeDone; err != nil {
				t.Fatal(err)
			}
			if err := <-writeDone; err != nil {
				t.Fatal(err)
			}
			if _, err := s.EnvVar(ctx, org.ID, old.ID); !errors.Is(err, store.ErrNotFound) {
				t.Fatalf("old variable: %v", err)
			}
			if n := count(); n != 1 {
				t.Fatalf("rows landed after the purge: %d", n)
			}
		})
	}
}

func boolCount(ok bool) int {
	if ok {
		return 1
	}
	return 0
}

func TestPurgeMemberRevokesTokensByRevision(t *testing.T) {
	ctx := context.Background()
	pool := dbtest.New(t)
	s := store.New(pool)
	org, bob, carol := uuid.New(), uuid.New(), uuid.New()
	const removedAt = 10
	old, _, err := s.CreateTelemetryToken(ctx, org, bob, "old", removedAt-1)
	if err != nil {
		t.Fatal(err)
	}
	last, _, err := s.CreateTelemetryToken(ctx, org, bob, "last", removedAt)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.CreateTelemetryToken(ctx, org, carol, "carol", removedAt-1); err != nil {
		t.Fatal(err)
	}
	fresh, _, err := s.CreateTelemetryToken(ctx, org, bob, "fresh", removedAt+1)
	if err != nil {
		t.Fatal(err)
	}
	for _, back := range []bool{true, false} {
		if _, err := s.PurgeMember(ctx, org, bob, removedAt, func(context.Context) (bool, error) { return back, nil }); err != nil {
			t.Fatal(err)
		}
		toks, err := s.TelemetryTokens(ctx, org)
		if err != nil || len(toks) != 2 || !slices.ContainsFunc(toks, func(tok store.TelemetryToken) bool { return tok.ID == fresh.ID }) {
			t.Fatalf("back=%v: active tokens %+v %v", back, toks, err)
		}
	}
	revoked := map[string]bool{}
	for _, a := range storetest.Audit(t, pool, store.ActionGrafanaTokenRevoke) {
		if a.ActorKind != store.ActorSystem || a.OrgID == nil || *a.OrgID != org {
			t.Fatalf("audit: %+v", a)
		}
		revoked[a.Resource] = true
	}
	if len(revoked) != 2 || !revoked["telemetry-token/"+old.ID.String()] || !revoked["telemetry-token/"+last.ID.String()] {
		t.Fatalf("revoked %v", revoked)
	}
}

func TestPurgeMemberDropsUserLimits(t *testing.T) {
	ctx := context.Background()
	pool := dbtest.New(t)
	s := store.New(pool)
	orgs, bob, carol := []uuid.UUID{uuid.New(), uuid.New()}, uuid.New(), uuid.New()
	for _, org := range orgs {
		for _, u := range []uuid.UUID{bob, carol} {
			execSQL(t, pool, `INSERT INTO limits (org_id, scope, vcpu) VALUES ($1, $2, 1)`, org, "user:"+u.String())
		}
	}
	if _, err := s.PurgeMember(ctx, orgs[0], bob, 0, func(context.Context) (bool, error) { return false, nil }); err != nil {
		t.Fatal(err)
	}
	var left int
	if err := pool.QueryRow(ctx, "SELECT count(*) FROM limits").Scan(&left); err != nil || left != 3 {
		t.Fatalf("limits left %d %v, want 3", left, err)
	}
	if err := pool.QueryRow(ctx, "SELECT count(*) FROM limits WHERE org_id = $1 AND scope = $2", orgs[0], "user:"+bob.String()).Scan(&left); err != nil || left != 0 {
		t.Fatalf("bob's limit in the org: %d %v", left, err)
	}
}

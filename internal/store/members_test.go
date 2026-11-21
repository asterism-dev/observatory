package store_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/asterism-dev/observatory/internal/db/dbtest"
	"github.com/asterism-dev/observatory/internal/integrations"
	"github.com/asterism-dev/observatory/internal/store"
)

func login(t *testing.T, s *store.Store, sub string) store.LoginResult {
	t.Helper()
	res, err := s.Login(context.Background(), store.Identity{Subject: sub, Email: sub + "@x.io", Name: sub})
	if err != nil {
		t.Fatal(err)
	}
	return res
}

func memberRoles(t *testing.T, s *store.Store, org uuid.UUID) map[uuid.UUID]store.Role {
	t.Helper()
	ms, err := s.Members(context.Background(), org)
	if err != nil {
		t.Fatal(err)
	}
	out := map[uuid.UUID]store.Role{}
	for _, m := range ms {
		out[m.User.ID] = m.Role
	}
	return out
}

func TestLastOwnerProtected(t *testing.T) {
	ctx := context.Background()
	pool := dbtest.New(t)
	s := store.New(pool)
	a, m := login(t, s, "a"), login(t, s, "m")
	org, err := s.CreateOrg(ctx, "Acme", a.User.ID)
	if err != nil {
		t.Fatal(err)
	}
	execSQL(t, pool, `INSERT INTO memberships (org_id, user_id, role) VALUES ($1, $2, 'member')`, org.ID, m.User.ID)

	ms, err := s.Members(ctx, org.ID)
	if err != nil || len(ms) != 2 || ms[0].User.Email != "a@x.io" || ms[0].Role != store.RoleOwner || ms[0].JoinedAt.IsZero() {
		t.Fatalf("members: %+v %v", ms, err)
	}
	if err := s.SetMemberRole(ctx, org.ID, a.User.ID, store.RoleAdmin); !errors.Is(err, store.ErrLastOwner) {
		t.Fatalf("demote last owner: %v", err)
	}
	if err := s.RemoveMember(ctx, org.ID, a.User.ID); !errors.Is(err, store.ErrLastOwner) {
		t.Fatalf("remove last owner: %v", err)
	}
	if err := s.SetMemberRole(ctx, org.ID, m.User.ID, store.RoleOwner); err != nil {
		t.Fatal(err)
	}
	if err := s.SetMemberRole(ctx, org.ID, a.User.ID, store.RoleMember); err != nil {
		t.Fatalf("demote one of two owners: %v", err)
	}
	if err := s.RemoveMember(ctx, org.ID, m.User.ID); !errors.Is(err, store.ErrLastOwner) {
		t.Fatalf("remove new last owner: %v", err)
	}
	if err := s.RemoveMember(ctx, org.ID, a.User.ID); err != nil {
		t.Fatalf("remove member: %v", err)
	}
	if got := memberRoles(t, s, org.ID); len(got) != 1 || got[m.User.ID] != store.RoleOwner {
		t.Fatalf("after: %v", got)
	}

	if err := s.SetMemberRole(ctx, org.ID, a.User.ID, store.RoleOwner); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("set role of non-member: %v", err)
	}
	if err := s.RemoveMember(ctx, org.ID, a.User.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("remove non-member: %v", err)
	}
	if err := s.SetMemberRole(ctx, a.PersonalOrg.ID, a.User.ID, store.RoleAdmin); !errors.Is(err, store.ErrPersonalOrg) {
		t.Fatalf("personal set role: %v", err)
	}
	if err := s.RemoveMember(ctx, a.PersonalOrg.ID, a.User.ID); !errors.Is(err, store.ErrPersonalOrg) {
		t.Fatalf("personal leave: %v", err)
	}
}

func TestConcurrentOwnerDemotion(t *testing.T) {
	ctx := context.Background()
	pool := dbtest.New(t)
	s := store.New(pool)
	a, b := login(t, s, "a"), login(t, s, "b")
	org, err := s.CreateOrg(ctx, "Acme", a.User.ID)
	if err != nil {
		t.Fatal(err)
	}
	execSQL(t, pool, `INSERT INTO memberships (org_id, user_id, role) VALUES ($1, $2, 'owner')`, org.ID, b.User.ID)

	// a demotes b in an open transaction; a's concurrent demotion by b must wait for it, then see one owner left.
	second := make(chan error, 1)
	first := s.InTx(ctx, func(tx *store.Store) error {
		if err := tx.SetMemberRole(ctx, org.ID, b.User.ID, store.RoleAdmin); err != nil {
			return err
		}
		go func() { second <- s.SetMemberRole(ctx, org.ID, a.User.ID, store.RoleAdmin) }()
		deadline := time.Now().Add(5 * time.Second)
		for time.Now().Before(deadline) {
			if len(second) > 0 {
				return nil
			}
			var waiting int
			if err := pool.QueryRow(ctx, `SELECT count(*) FROM pg_stat_activity WHERE datname = current_database() AND wait_event_type = 'Lock'`).Scan(&waiting); err != nil {
				return err
			}
			if waiting > 0 {
				return nil
			}
			time.Sleep(10 * time.Millisecond)
		}
		t.Error("second demotion neither blocked nor finished")
		return nil
	})
	if first != nil {
		t.Fatalf("first demotion: %v", first)
	}
	if err := <-second; !errors.Is(err, store.ErrLastOwner) {
		t.Fatalf("second demotion: %v", err)
	}
	if got := memberRoles(t, s, org.ID); got[a.User.ID] != store.RoleOwner || got[b.User.ID] != store.RoleAdmin {
		t.Fatalf("roles: %v", got)
	}
}

func TestRemoveMemberCleansTeamsAndBindings(t *testing.T) {
	ctx := context.Background()
	pool := dbtest.New(t)
	s := store.New(pool)
	a, m := login(t, s, "a"), login(t, s, "m")
	var orgs [2]uuid.UUID
	for i, name := range []string{"Acme", "Other"} {
		org, err := s.CreateOrg(ctx, name, a.User.ID)
		if err != nil {
			t.Fatal(err)
		}
		orgs[i] = org.ID
		execSQL(t, pool, `INSERT INTO memberships (org_id, user_id, role) VALUES ($1, $2, 'member')`, org.ID, m.User.ID)
		var team, role uuid.UUID
		if err := pool.QueryRow(ctx, `INSERT INTO teams (org_id, name) VALUES ($1, 't') RETURNING id`, org.ID).Scan(&team); err != nil {
			t.Fatal(err)
		}
		if err := pool.QueryRow(ctx, `INSERT INTO roles (org_id, name) VALUES ($1, 'r') RETURNING id`, org.ID).Scan(&role); err != nil {
			t.Fatal(err)
		}
		for _, u := range []uuid.UUID{a.User.ID, m.User.ID} {
			execSQL(t, pool, `INSERT INTO team_members (team_id, user_id) VALUES ($1, $2)`, team, u)
			execSQL(t, pool, `INSERT INTO role_bindings (org_id, subject, role_id) VALUES ($1, $2, $3)`, org.ID, "user:"+u.String(), role)
			execSQL(t, pool, `INSERT INTO limits (org_id, scope, vcpu) VALUES ($1, $2, 1)`, org.ID, "user:"+u.String())
		}
	}

	if err := s.RemoveMember(ctx, orgs[0], m.User.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.PurgeMember(ctx, orgs[0], m.User.ID, memberIn(s, orgs[0], m.User.ID)); err != nil {
		t.Fatal(err)
	}
	count := func(sql string, args ...any) int {
		t.Helper()
		var n int
		if err := pool.QueryRow(ctx, sql, args...).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	teams := `SELECT count(*) FROM team_members tm JOIN teams t ON t.id = tm.team_id WHERE t.org_id = $1 AND tm.user_id = $2`
	bindings := `SELECT count(*) FROM role_bindings WHERE org_id = $1 AND subject = $2`
	limits := `SELECT count(*) FROM limits WHERE org_id = $1 AND scope = $2`
	if count(teams, orgs[0], m.User.ID) != 0 || count(bindings, orgs[0], "user:"+m.User.ID.String()) != 0 || count(limits, orgs[0], "user:"+m.User.ID.String()) != 0 {
		t.Fatal("team membership, binding or limit left in org")
	}
	if count(teams, orgs[0], a.User.ID) != 1 || count(bindings, orgs[0], "user:"+a.User.ID.String()) != 1 || count(limits, orgs[0], "user:"+a.User.ID.String()) != 1 {
		t.Fatal("other member's rows removed")
	}
	if count(teams, orgs[1], m.User.ID) != 1 || count(bindings, orgs[1], "user:"+m.User.ID.String()) != 1 || count(limits, orgs[1], "user:"+m.User.ID.String()) != 1 {
		t.Fatal("rows in other org removed")
	}
	if _, err := s.Membership(ctx, orgs[0], m.User.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("membership: %v", err)
	}
}

func execSQL(t *testing.T, pool *pgxpool.Pool, sql string, args ...any) {
	t.Helper()
	if _, err := pool.Exec(context.Background(), sql, args...); err != nil {
		t.Fatal(err)
	}
}

// memberIn answers like iam's GetMembership, from the memberships table.
func memberIn(s *store.Store, org, user uuid.UUID) func(context.Context) (bool, error) {
	return func(ctx context.Context) (bool, error) {
		_, err := s.Membership(ctx, org, user)
		if errors.Is(err, store.ErrNotFound) {
			return false, nil
		}
		return err == nil, err
	}
}

func TestPurgeSkipsWhenIAMSaysMember(t *testing.T) {
	ctx := context.Background()
	s := store.New(dbtest.New(t)).WithMasterKey(testKey(1))
	a, m := login(t, s, "a"), login(t, s, "m")
	org := mustCreateOrg(t, s, "A", a.User.ID)
	mid := m.User.ID
	v := mustCreateEnv(t, s, envVar(org, mid, store.EnvPlain, store.EnvScopeUser, &mid, "P"), "p")
	asked := false
	purged, err := s.PurgeMember(ctx, org.ID, mid, func(context.Context) (bool, error) { asked = true; return true, nil })
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
		prepare func(t *testing.T, s *store.Store, org store.Org, user uuid.UUID) (write func() error, count func() int)
	}{
		{"env create", func(t *testing.T, s *store.Store, org store.Org, user uuid.UUID) (func() error, func() int) {
			var v store.EnvVar
			return func() (err error) {
					v, err = s.CreateEnvVar(t.Context(), envVar(org, user, store.EnvPlain, store.EnvScopeUser, &user, "NEW"), "n")
					return err
				}, func() int {
					_, err := s.EnvVar(t.Context(), org.ID, v.ID)
					return boolCount(err == nil)
				}
		}},
		{"env move", func(t *testing.T, s *store.Store, org store.Org, user uuid.UUID) (func() error, func() int) {
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
		{"dashboard scope change", func(t *testing.T, s *store.Store, org store.Org, user uuid.UUID) (func() error, func() int) {
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
		{"device create", func(t *testing.T, s *store.Store, org store.Org, user uuid.UUID) (func() error, func() int) {
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
		{"integration create", func(t *testing.T, s *store.Store, org store.Org, user uuid.UUID) (func() error, func() int) {
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
				_, err := s.PurgeMember(ctx, org.ID, mid, func(context.Context) (bool, error) {
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

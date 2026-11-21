package store_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/asterism-dev/observatory/internal/db/dbtest"
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
	if _, err := s.PurgeMember(ctx, orgs[0], m.User.ID); err != nil {
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

func TestPurgeMemberWaitsForConcurrentRejoin(t *testing.T) {
	ctx := context.Background()
	pool := dbtest.New(t)
	s := store.New(pool)
	a, m := login(t, s, "a"), login(t, s, "m")
	org := mustCreateOrg(t, s, "A", a.User.ID)
	rejoin, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rejoin.Rollback(ctx) }()
	if _, err := rejoin.Exec(ctx, "INSERT INTO memberships (org_id, user_id, role) VALUES ($1, $2, 'member')", org.ID, m.User.ID); err != nil {
		t.Fatal(err)
	}
	type result struct {
		purged bool
		err    error
	}
	done := make(chan result, 1)
	go func() {
		purged, err := s.PurgeMember(ctx, org.ID, m.User.ID)
		done <- result{purged, err}
	}()
	select {
	case r := <-done:
		t.Fatalf("purge didn't wait for the uncommitted re-join: %+v", r)
	case <-time.After(200 * time.Millisecond):
	}
	if err := rejoin.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if r := <-done; r.err != nil || r.purged {
		t.Fatalf("purge after the re-join: %+v", r)
	}
}

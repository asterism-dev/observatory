package store_test

import (
	"bytes"
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/asterism-dev/observatory/internal/authz"
	"github.com/asterism-dev/observatory/internal/db/dbtest"
	"github.com/asterism-dev/observatory/internal/egress"
	"github.com/asterism-dev/observatory/internal/seal"
	"github.com/asterism-dev/observatory/internal/store"
)

func testKey(b byte) seal.Key { return seal.Key(bytes.Repeat([]byte{b}, 32)) }

func newEnvStore(t *testing.T) (*store.Store, loginResult, testOrg, testTeam) {
	t.Helper()
	ctx := context.Background()
	s := store.New(dbtest.New(t)).WithMasterKey(testKey(1))
	u := login(t, s, "env")
	org, err := createOrg(s, ctx, "E", u.User.ID)
	if err != nil {
		t.Fatal(err)
	}
	team, err := createTeam(s, ctx, org.ID, "core")
	if err != nil {
		t.Fatal(err)
	}
	return s, u, org, team
}

func mustCreateEnv(t *testing.T, s *store.Store, v store.EnvVar, value string) store.EnvVar {
	t.Helper()
	got, err := s.CreateEnvVar(context.Background(), v, value)
	if err != nil {
		t.Fatal(err)
	}
	return got
}

func mustEnvVar(t *testing.T, s *store.Store, org, id uuid.UUID) store.EnvVar {
	t.Helper()
	v, err := s.EnvVar(context.Background(), org, id)
	if err != nil {
		t.Fatal(err)
	}
	return v
}

func envVar(org testOrg, user uuid.UUID, kind store.EnvKind, scope store.EnvScope, owner *uuid.UUID, name string) store.EnvVar {
	return store.EnvVar{OrgID: org.ID, Kind: kind, Scope: scope, OwnerID: owner, Name: name, CreatedBy: user, UpdatedBy: user}
}

func TestEnvVarRoundTrip(t *testing.T) {
	ctx := context.Background()
	s, u, org, team := newEnvStore(t)
	uid := u.User.ID
	cases := []store.EnvVar{
		envVar(org, uid, store.EnvPlain, store.EnvScopeOrg, nil, "A"),
		envVar(org, uid, store.EnvSecret, store.EnvScopeOrg, nil, "B"),
		envVar(org, uid, store.EnvPlain, store.EnvScopeTeam, &team.ID, "C"),
		envVar(org, uid, store.EnvSecret, store.EnvScopeTeam, &team.ID, "D"),
		envVar(org, uid, store.EnvPlain, store.EnvScopeUser, &uid, "E"),
		envVar(org, uid, store.EnvSecret, store.EnvScopeUser, &uid, "F"),
	}
	for _, c := range cases {
		v, err := s.CreateEnvVar(ctx, c, "val-"+c.Name)
		if err != nil {
			t.Fatal(err)
		}
		got, err := s.EnvValue(ctx, v)
		if err != nil || got != "val-"+c.Name {
			t.Fatalf("%s: %q %v", c.Name, got, err)
		}
		meta, err := s.EnvVar(ctx, org.ID, v.ID)
		if err != nil || meta.Name != c.Name || meta.Kind != c.Kind || meta.Scope != c.Scope {
			t.Fatalf("%s: %+v %v", c.Name, meta, err)
		}
	}
	list, err := s.EnvVars(ctx, org.ID)
	if err != nil || len(list) != len(cases) || list[0].Name != "A" || list[5].Name != "F" {
		t.Fatalf("list: %+v %v", list, err)
	}
	if _, err := s.EnvVar(ctx, org.ID, uuid.New()); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("missing: %v", err)
	}
}

func TestEnvVarDuplicate(t *testing.T) {
	ctx := context.Background()
	s, u, org, team := newEnvStore(t)
	uid := u.User.ID
	if _, err := s.CreateEnvVar(ctx, envVar(org, uid, store.EnvPlain, store.EnvScopeTeam, &team.ID, "X"), "1"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateEnvVar(ctx, envVar(org, uid, store.EnvSecret, store.EnvScopeTeam, &team.ID, "X"), "2"); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("duplicate: %v", err)
	}
	if _, err := s.CreateEnvVar(ctx, envVar(org, uid, store.EnvPlain, store.EnvScopeOrg, nil, "X"), "3"); err != nil {
		t.Fatalf("other scope: %v", err)
	}
}

func TestEnvVarRowSwap(t *testing.T) {
	ctx := context.Background()
	pool := dbtest.New(t)
	s := store.New(pool).WithMasterKey(testKey(1))
	u := login(t, s, "swap")
	org := mustCreateOrg(t, s, "S", u.User.ID)
	a := mustCreateEnv(t, s, envVar(org, u.User.ID, store.EnvSecret, store.EnvScopeOrg, nil, "A"), "a")
	b := mustCreateEnv(t, s, envVar(org, u.User.ID, store.EnvSecret, store.EnvScopeOrg, nil, "B"), "b")
	if got, err := s.EnvValue(ctx, b); err != nil || got != "b" {
		t.Fatalf("before swap: %q %v", got, err)
	}
	if _, err := pool.Exec(ctx, `UPDATE env_vars SET nonce = a.nonce, ciphertext = a.ciphertext FROM env_vars a WHERE a.id = $1 AND env_vars.id = $2`, a.ID, b.ID); err != nil {
		t.Fatal(err)
	}
	_, err := s.EnvValue(ctx, b)
	if err == nil || errors.Is(err, store.ErrNotFound) {
		t.Fatalf("want decryption error, got %v", err)
	}
}

func TestEnvVarWrongMasterKey(t *testing.T) {
	ctx := context.Background()
	s, u, org, _ := newEnvStore(t)
	v, err := s.CreateEnvVar(ctx, envVar(org, u.User.ID, store.EnvSecret, store.EnvScopeOrg, nil, "A"), "a")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.WithMasterKey(testKey(2)).EnvValue(ctx, v); !errors.Is(err, store.ErrMasterKeyMismatch) {
		t.Fatalf("wrong master key: %v", err)
	}
}

func TestCheckMasterKey(t *testing.T) {
	ctx := context.Background()
	s, u, org, _ := newEnvStore(t)
	if err := s.WithMasterKey(testKey(2)).CheckMasterKey(ctx); err != nil {
		t.Fatalf("no org keys yet: %v", err)
	}
	mustCreateEnv(t, s, envVar(org, u.User.ID, store.EnvSecret, store.EnvScopeOrg, nil, "A"), "a")
	if err := s.CheckMasterKey(ctx); err != nil {
		t.Fatalf("same key: %v", err)
	}
	if err := s.WithMasterKey(testKey(2)).CheckMasterKey(ctx); !errors.Is(err, store.ErrMasterKeyMismatch) {
		t.Fatalf("other key: %v", err)
	}
}

func TestEnvVarKindBound(t *testing.T) {
	ctx := context.Background()
	pool := dbtest.New(t)
	s := store.New(pool).WithMasterKey(testKey(1))
	u := login(t, s, "kind")
	org := mustCreateOrg(t, s, "K", u.User.ID)
	v := mustCreateEnv(t, s, envVar(org, u.User.ID, store.EnvSecret, store.EnvScopeOrg, nil, "A"), "hidden")
	execSQL(t, pool, "UPDATE env_vars SET kind = 'env' WHERE id = $1", v.ID)
	flipped := mustEnvVar(t, s, org.ID, v.ID)
	if got, err := s.EnvValue(ctx, flipped); err == nil || errors.Is(err, store.ErrNotFound) {
		t.Fatalf("flipped kind revealed %q, err %v", got, err)
	}
}

func TestEnvVarUpdateKeepsValue(t *testing.T) {
	ctx := context.Background()
	s, u, org, team := newEnvStore(t)
	v := mustCreateEnv(t, s, envVar(org, u.User.ID, store.EnvPlain, store.EnvScopeOrg, nil, "A"), "keep")
	old := v
	v.Name, v.Kind = "B", store.EnvSecret
	if err := s.UpdateEnvVar(ctx, old, v, nil); err != nil {
		t.Fatal(err)
	}
	got := mustEnvVar(t, s, org.ID, v.ID)
	if got.Name != "B" || got.Kind != store.EnvSecret {
		t.Fatalf("%+v", got)
	}
	if val, err := s.EnvValue(ctx, got); err != nil || val != "keep" {
		t.Fatalf("%q %v", val, err)
	}
	nv := "new"
	if err := s.UpdateEnvVar(ctx, got, got, &nv); err != nil {
		t.Fatal(err)
	}
	if val, err := s.EnvValue(ctx, got); err != nil || val != "new" {
		t.Fatalf("%q %v", val, err)
	}

	target := got
	target.Scope, target.OwnerID = store.EnvScopeTeam, &team.ID
	if err := s.MoveEnvVar(ctx, got, target); err != nil {
		t.Fatal(err)
	}
	moved := mustEnvVar(t, s, org.ID, v.ID)
	if moved.Scope != store.EnvScopeTeam || moved.OwnerID == nil || *moved.OwnerID != team.ID {
		t.Fatalf("%+v", moved)
	}
	if val, err := s.EnvValue(ctx, moved); err != nil || val != "new" {
		t.Fatalf("%q %v", val, err)
	}
	if _, err := s.CreateEnvVar(ctx, envVar(org, u.User.ID, store.EnvPlain, store.EnvScopeOrg, nil, "B"), "x"); err != nil {
		t.Fatal(err)
	}
	back := moved
	back.Scope, back.OwnerID = store.EnvScopeOrg, nil
	if err := s.MoveEnvVar(ctx, moved, back); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("move onto existing: %v", err)
	}
	missing := store.EnvVar{ID: uuid.New(), OrgID: org.ID, Name: "Z", Kind: store.EnvPlain, Scope: store.EnvScopeOrg, UpdatedBy: u.User.ID}
	if err := s.UpdateEnvVar(ctx, missing, missing, nil); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("missing: %v", err)
	}
}

func TestEnvVarStaleWrites(t *testing.T) {
	ctx := context.Background()
	s, u, org, team := newEnvStore(t)
	plain := mustCreateEnv(t, s, envVar(org, u.User.ID, store.EnvPlain, store.EnvScopeOrg, nil, "A"), "a")

	// Request 1 turns the var into a secret; request 2 loaded it while still plain and renames it afterwards.
	secret := plain
	secret.Kind = store.EnvSecret
	nv := "sealed"
	if err := s.UpdateEnvVar(ctx, plain, secret, &nv); err != nil {
		t.Fatal(err)
	}
	renamed := plain
	renamed.Name = "B"
	if err := s.UpdateEnvVar(ctx, plain, renamed, nil); !errors.Is(err, store.ErrStale) {
		t.Fatalf("stale kind update: %v", err)
	}
	if got := mustEnvVar(t, s, org.ID, plain.ID); got.Kind != store.EnvSecret || got.Name != "A" {
		t.Fatalf("stale update changed the var: %+v", got)
	}

	cur := mustEnvVar(t, s, org.ID, plain.ID)
	inTeam := cur
	inTeam.Scope, inTeam.OwnerID = store.EnvScopeTeam, &team.ID
	if err := s.MoveEnvVar(ctx, cur, inTeam); err != nil {
		t.Fatal(err)
	}
	// cur still claims org scope, which the permission checks were made against.
	toUser := cur
	toUser.Scope, toUser.OwnerID = store.EnvScopeUser, &u.User.ID
	if err := s.MoveEnvVar(ctx, cur, toUser); !errors.Is(err, store.ErrStale) {
		t.Fatalf("stale move: %v", err)
	}
	if err := s.UpdateEnvVar(ctx, cur, cur, &nv); !errors.Is(err, store.ErrStale) {
		t.Fatalf("stale scope update: %v", err)
	}
	if err := s.DeleteEnvVar(ctx, cur); !errors.Is(err, store.ErrStale) {
		t.Fatalf("stale delete: %v", err)
	}
	if got := mustEnvVar(t, s, org.ID, plain.ID); got.Scope != store.EnvScopeTeam || *got.OwnerID != team.ID {
		t.Fatalf("stale writes changed the var: %+v", got)
	}
	if err := s.DeleteEnvVar(ctx, inTeam); err != nil {
		t.Fatalf("current delete: %v", err)
	}
}

func TestEnvVarNoKey(t *testing.T) {
	ctx := context.Background()
	s, u, org, _ := newEnvStore(t)
	v := mustCreateEnv(t, s, envVar(org, u.User.ID, store.EnvPlain, store.EnvScopeOrg, nil, "A"), "a")
	bare := store.New(dbtest.New(t))
	if _, err := bare.CreateEnvVar(ctx, v, "x"); !errors.Is(err, store.ErrNoMasterKey) {
		t.Fatalf("create: %v", err)
	}
	if _, err := bare.EnvValue(ctx, v); !errors.Is(err, store.ErrNoMasterKey) {
		t.Fatalf("value: %v", err)
	}
}

func TestPurgeMemberDeletesPrivateEnv(t *testing.T) {
	ctx := context.Background()
	pool := dbtest.New(t)
	s := store.New(pool).WithMasterKey(testKey(1))
	owner := login(t, s, "owner")
	org := mustCreateOrg(t, s, "R", owner.User.ID)
	m := login(t, s, "member")
	addMember(s, org.ID, m.User.ID, store.Role("member"))
	mid := m.User.ID
	private := mustCreateEnv(t, s, envVar(org, mid, store.EnvSecret, store.EnvScopeUser, &mid, "P"), "p")
	shared := mustCreateEnv(t, s, envVar(org, mid, store.EnvPlain, store.EnvScopeOrg, nil, "S"), "s")
	ownerVar := mustCreateEnv(t, s, envVar(org, owner.User.ID, store.EnvPlain, store.EnvScopeUser, &owner.User.ID, "O"), "o")
	_ = removeMember(s, ctx, org.ID, mid)
	if purged, err := s.PurgeMember(ctx, org.ID, mid, time.Now(), memberIn(s, org.ID, mid)); err != nil || !purged {
		t.Fatalf("purge: %v %v", purged, err)
	}
	if _, err := s.EnvVar(ctx, org.ID, private.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("private: %v", err)
	}
	for _, v := range []store.EnvVar{shared, ownerVar} {
		if _, err := s.EnvVar(ctx, org.ID, v.ID); err != nil {
			t.Fatalf("%s: %v", v.Name, err)
		}
	}
}

func TestPurgeTeamDeletesTeamEnv(t *testing.T) {
	ctx := context.Background()
	s, u, org, team := newEnvStore(t)
	v := mustCreateEnv(t, s, envVar(org, u.User.ID, store.EnvPlain, store.EnvScopeTeam, &team.ID, "T"), "t")
	if err := s.PurgeTeam(ctx, org.ID, team.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.EnvVar(ctx, org.ID, v.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("%v", err)
	}
	if err := s.DeleteEnvVar(ctx, v); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("delete missing: %v", err)
	}
}

func TestOrgKeyNotPlain(t *testing.T) {
	ctx := context.Background()
	pool := dbtest.New(t)
	s := store.New(pool).WithMasterKey(testKey(1))
	u := login(t, s, "key")
	org := mustCreateOrg(t, s, "K", u.User.ID)
	for _, n := range []string{"A", "B"} {
		if _, err := s.CreateEnvVar(ctx, envVar(org, u.User.ID, store.EnvSecret, store.EnvScopeOrg, nil, n), "x"); err != nil {
			t.Fatal(err)
		}
	}
	var count int
	var wrapped, nonce []byte
	if err := pool.QueryRow(ctx, "SELECT count(*) FROM org_keys WHERE org_id = $1", org.ID).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, "SELECT wrapped, nonce FROM org_keys WHERE org_id = $1", org.ID).Scan(&wrapped, &nonce); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("rows: %d", count)
	}
	plain, err := seal.Open(testKey(1), nonce, wrapped, org.ID[:])
	if err != nil || len(plain) != 32 || bytes.Equal(plain, wrapped[:32]) {
		t.Fatalf("unwrap: %v", err)
	}
}

func TestResolveEnv(t *testing.T) {
	ctx := context.Background()
	pool := dbtest.New(t)
	s := store.New(pool).WithMasterKey(testKey(1))
	u := login(t, s, "env")
	org := mustCreateOrg(t, s, "E", u.User.ID)
	team, err := createTeam(s, ctx, org.ID, "core")
	if err != nil {
		t.Fatal(err)
	}
	team2, err := createTeam(s, ctx, org.ID, "other")
	if err != nil {
		t.Fatal(err)
	}
	empty := mustCreateOrg(t, s, "Empty", u.User.ID)
	uid := u.User.ID
	other := login(t, s, "env-other").User.ID
	addMember(s, org.ID, other, store.Role("member"))
	for _, c := range []struct {
		kind  store.EnvKind
		scope store.EnvScope
		owner *uuid.UUID
		name  string
		value string
	}{
		{store.EnvPlain, store.EnvScopeOrg, nil, "ORG", "o"},
		{store.EnvSecret, store.EnvScopeOrg, nil, "SHARED_ORG", "org"},
		{store.EnvPlain, store.EnvScopeTeam, &team.ID, "TEAM", "t"},
		{store.EnvSecret, store.EnvScopeTeam, &team.ID, "SHARED_ORG", "team"},
		{store.EnvSecret, store.EnvScopeUser, &uid, "MINE", "m"},
		{store.EnvPlain, store.EnvScopeUser, &uid, "SHARED_ORG", "user"},
		{store.EnvSecret, store.EnvScopeUser, &other, "THEIRS", "x"},
		{store.EnvPlain, store.EnvScopeTeam, &team2.ID, "TEAM2", "t2"},
	} {
		mustCreateEnv(t, s, envVar(org, uid, c.kind, c.scope, c.owner, c.name), c.value)
	}
	policy, err := policySnapshot(s, ctx)
	if err != nil {
		t.Fatal(err)
	}
	e, err := authz.New(policy)
	if err != nil {
		t.Fatal(err)
	}
	eq := func(got, want map[string]string) {
		t.Helper()
		if len(got) != len(want) {
			t.Fatalf("got %v want %v", got, want)
		}
		for k, v := range want {
			if got[k] != v {
				t.Fatalf("got %v want %v", got, want)
			}
		}
	}

	got, err := s.ResolveEnv(ctx, e, org.ID, authz.UserSubject(uid))
	if err != nil {
		t.Fatal(err)
	}
	eq(got, map[string]string{"ORG": "o", "SHARED_ORG": "user", "MINE": "m"})

	got, err = s.ResolveEnv(ctx, e, org.ID, authz.TeamSubject(team.ID))
	if err != nil {
		t.Fatal(err)
	}
	eq(got, map[string]string{"ORG": "o", "SHARED_ORG": "team", "TEAM": "t"})

	denied := policy
	denied.Roles = []authz.CustomRole{{ID: uuid.New(), OrgID: org.ID, Name: "no-org-env",
		Rules: []authz.Rule{{Effect: authz.Deny, Object: authz.ObjEnv, Action: "secrets.variables.use"}}}}
	denied.Bindings = []authz.Binding{{OrgID: org.ID, Subject: authz.UserSubject(uid), RoleID: denied.Roles[0].ID}}
	de, err := authz.New(denied)
	if err != nil {
		t.Fatal(err)
	}
	got, err = s.ResolveEnv(ctx, de, org.ID, authz.UserSubject(uid))
	if err != nil {
		t.Fatal(err)
	}
	eq(got, map[string]string{"MINE": "m", "SHARED_ORG": "user"})

	deniedOwn := policy
	deniedOwn.Roles = []authz.CustomRole{{ID: uuid.New(), OrgID: org.ID, Name: "no-own-env",
		Rules: []authz.Rule{{Effect: authz.Deny, Object: authz.UserEnvObject(uid), Action: "secrets.variables.use"}}}}
	deniedOwn.Bindings = []authz.Binding{{OrgID: org.ID, Subject: authz.UserSubject(uid), RoleID: deniedOwn.Roles[0].ID}}
	oe, err := authz.New(deniedOwn)
	if err != nil {
		t.Fatal(err)
	}
	got, err = s.ResolveEnv(ctx, oe, org.ID, authz.UserSubject(uid))
	if err != nil {
		t.Fatal(err)
	}
	eq(got, map[string]string{"ORG": "o", "SHARED_ORG": "org"})

	got, err = s.ResolveEnv(ctx, e, empty.ID, authz.UserSubject(uid))
	if err != nil {
		t.Fatal(err)
	}
	eq(got, map[string]string{})
	var keys int
	if err := pool.QueryRow(ctx, "SELECT count(*) FROM org_keys WHERE org_id = $1", empty.ID).Scan(&keys); err != nil || keys != 0 {
		t.Fatalf("org key created by read: %d %v", keys, err)
	}

	if _, err := s.ResolveEnv(ctx, e, org.ID, "vm:"+uuid.NewString()); err == nil {
		t.Fatal("vm owner accepted")
	}
}

func boundVar(org testOrg, user uuid.UUID, scope store.EnvScope, owner *uuid.UUID, name string, hosts ...string) store.EnvVar {
	v := envVar(org, user, store.EnvSecret, scope, owner, name)
	v.Hosts = hosts
	return v
}

func placeholderOf(t *testing.T, pool *pgxpool.Pool, id uuid.UUID) (p string) {
	t.Helper()
	if err := pool.QueryRow(context.Background(), "SELECT coalesce(placeholder, '') FROM env_vars WHERE id = $1", id).Scan(&p); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestEnvHostsRoundTrip(t *testing.T) {
	ctx := context.Background()
	s, u, org, _ := newEnvStore(t)
	hosts := []string{"api.github.com", "github.com"}
	v := mustCreateEnv(t, s, boundVar(org, u.User.ID, store.EnvScopeOrg, nil, "GH", hosts...), "0123456789abcdef")
	if got := mustEnvVar(t, s, org.ID, v.ID); !slices.Equal(got.Hosts, hosts) {
		t.Fatalf("hosts %v", got.Hosts)
	}
	list, err := s.EnvVars(ctx, org.ID)
	if err != nil || len(list) != 1 || !slices.Equal(list[0].Hosts, hosts) {
		t.Fatalf("list %v %v", list, err)
	}
}

func TestEnvHostsOnlySecret(t *testing.T) {
	ctx := context.Background()
	pool := dbtest.New(t)
	s := store.New(pool).WithMasterKey(testKey(1))
	u := login(t, s, "env")
	org := mustCreateOrg(t, s, "E", u.User.ID)
	bad := envVar(org, u.User.ID, store.EnvPlain, store.EnvScopeOrg, nil, "P")
	bad.Hosts = []string{"a.test"}
	if _, err := s.CreateEnvVar(ctx, bad, "0123456789abcdef"); !errors.Is(err, store.ErrHostsNeedSecret) {
		t.Fatalf("got %v", err)
	}
	mustCreateEnv(t, s, envVar(org, u.User.ID, store.EnvPlain, store.EnvScopeOrg, nil, "Q"), "x")
	if _, err := pool.Exec(ctx, "UPDATE env_vars SET hosts = '{a.test}' WHERE kind = 'env'"); err == nil {
		t.Fatal("check did not reject hosts on an env variable")
	}
}

func TestEnvHostsValueTooShort(t *testing.T) {
	ctx := context.Background()
	s, u, org, _ := newEnvStore(t)
	uid := u.User.ID
	if _, err := s.CreateEnvVar(ctx, boundVar(org, uid, store.EnvScopeOrg, nil, "A", "a.test"), "12345678901"); !errors.Is(err, store.ErrValueTooShort) {
		t.Fatalf("got %v", err)
	}
	old := mustCreateEnv(t, s, envVar(org, uid, store.EnvSecret, store.EnvScopeOrg, nil, "B"), "12345678")
	v := old
	v.Hosts = []string{"a.test"}
	if err := s.UpdateEnvVar(ctx, old, v, nil); !errors.Is(err, store.ErrValueTooShort) {
		t.Fatalf("got %v", err)
	}
	short := "short"
	if err := s.UpdateEnvVar(ctx, old, v, &short); !errors.Is(err, store.ErrValueTooShort) {
		t.Fatalf("got %v", err)
	}
	long := "0123456789abcdef"
	if err := s.UpdateEnvVar(ctx, old, v, &long); err != nil {
		t.Fatal(err)
	}
}

func TestPlaceholderStable(t *testing.T) {
	ctx := context.Background()
	pool := dbtest.New(t)
	s := store.New(pool).WithMasterKey(testKey(1))
	u := login(t, s, "env")
	org := mustCreateOrg(t, s, "E", u.User.ID)
	uid := u.User.ID
	team, err := createTeam(s, ctx, org.ID, "core")
	if err != nil {
		t.Fatal(err)
	}
	const val = "0123456789abcdef"
	v := mustCreateEnv(t, s, boundVar(org, uid, store.EnvScopeOrg, nil, "A", "a.test"), val)
	p1 := placeholderOf(t, pool, v.ID)
	if !strings.HasPrefix(p1, "obsph_") {
		t.Fatalf("placeholder %q", p1)
	}
	same := func(step string) {
		t.Helper()
		if got := placeholderOf(t, pool, v.ID); got != p1 {
			t.Fatalf("%s: placeholder %q, want %q", step, got, p1)
		}
	}
	update := func(mod func(*store.EnvVar), value *string) {
		t.Helper()
		old := mustEnvVar(t, s, org.ID, v.ID)
		n := old
		mod(&n)
		if err := s.UpdateEnvVar(ctx, old, n, value); err != nil {
			t.Fatal(err)
		}
	}
	nv := val + "2"
	update(func(*store.EnvVar) {}, &nv)
	same("value")
	update(func(n *store.EnvVar) { n.Name = "B" }, nil)
	same("rename")
	old := mustEnvVar(t, s, org.ID, v.ID)
	moved := old
	moved.Scope, moved.OwnerID = store.EnvScopeTeam, &team.ID
	if err := s.MoveEnvVar(ctx, old, moved); err != nil {
		t.Fatal(err)
	}
	same("move")
	update(func(n *store.EnvVar) { n.Hosts = nil }, nil)
	same("unbind")
	if h := mustEnvVar(t, s, org.ID, v.ID).Hosts; h != nil {
		t.Fatalf("hosts %v", h)
	}
	update(func(n *store.EnvVar) { n.Hosts = []string{"b.test"} }, nil)
	same("rebind")
	w := mustCreateEnv(t, s, boundVar(org, uid, store.EnvScopeOrg, nil, "C", "c.test"), val)
	if p2 := placeholderOf(t, pool, w.ID); p2 == p1 || p2 == "" {
		t.Fatalf("second placeholder %q", p2)
	}
}

func TestResolveVMEnvBound(t *testing.T) {
	ctx := context.Background()
	pool := dbtest.New(t)
	s := store.New(pool).WithMasterKey(testKey(1))
	u := login(t, s, "env")
	org := mustCreateOrg(t, s, "E", u.User.ID)
	uid := u.User.ID
	team, err := createTeam(s, ctx, org.ID, "core")
	if err != nil {
		t.Fatal(err)
	}
	const val = "0123456789abcdef"
	hosts := []string{"api.github.com", "github.com"}
	gh := mustCreateEnv(t, s, boundVar(org, uid, store.EnvScopeOrg, nil, "GH_TOKEN", hosts...), val)
	mustCreateEnv(t, s, boundVar(org, uid, store.EnvScopeUser, &uid, "MINE", "m.test"), val+"m")
	policy, err := policySnapshot(s, ctx)
	if err != nil {
		t.Fatal(err)
	}
	e, err := authz.New(policy)
	if err != nil {
		t.Fatal(err)
	}
	p1 := placeholderOf(t, pool, gh.ID)

	env, bound, err := s.ResolveVMEnv(ctx, e, org.ID, authz.TeamSubject(team.ID))
	if err != nil {
		t.Fatal(err)
	}
	if len(env) != 1 || env["GH_TOKEN"] != p1 {
		t.Fatalf("env %v", env)
	}
	if len(bound) != 1 || bound[0].ID != gh.ID.String() || bound[0].Name != "GH_TOKEN" || bound[0].Placeholder != p1 ||
		bound[0].Value != val || !slices.Equal(bound[0].Hosts, hosts) || bound[0].Headers != nil {
		t.Fatalf("bound %+v", bound)
	}

	env, bound, err = s.ResolveVMEnv(ctx, e, org.ID, authz.UserSubject(uid))
	if err != nil {
		t.Fatal(err)
	}
	if len(env) != 2 || len(bound) != 2 || bound[0].Name != "GH_TOKEN" || bound[1].Name != "MINE" {
		t.Fatalf("env %v bound %+v", env, bound)
	}

	mustCreateEnv(t, s, envVar(org, uid, store.EnvSecret, store.EnvScopeUser, &uid, "GH_TOKEN"), "private-real")
	env, bound, err = s.ResolveVMEnv(ctx, e, org.ID, authz.UserSubject(uid))
	if err != nil {
		t.Fatal(err)
	}
	if env["GH_TOKEN"] != "private-real" || len(bound) != 1 || bound[0].Name != "MINE" {
		t.Fatalf("env %v bound %+v", env, bound)
	}

	plain, err := s.ResolveEnv(ctx, e, org.ID, authz.TeamSubject(team.ID))
	if err != nil {
		t.Fatal(err)
	}
	for k, v := range plain {
		if strings.Contains(v, val) {
			t.Fatalf("ResolveEnv leaked the bound value in %s", k)
		}
	}
}

func TestEnvHostsNormalized(t *testing.T) {
	ctx := context.Background()
	s, u, org, _ := newEnvStore(t)
	v := mustCreateEnv(t, s, boundVar(org, u.User.ID, store.EnvScopeOrg, nil, "A", " B.test", "a.test", "A.test"), "0123456789abcdef")
	if got := mustEnvVar(t, s, org.ID, v.ID); !slices.Equal(got.Hosts, []string{"a.test", "b.test"}) {
		t.Fatalf("hosts %v", got.Hosts)
	}
	if _, err := s.CreateEnvVar(ctx, boundVar(org, u.User.ID, store.EnvScopeOrg, nil, "B", "*.test"), "0123456789abcdef"); !errors.Is(err, egress.ErrInvalidHost) {
		t.Fatalf("got %v", err)
	}
}

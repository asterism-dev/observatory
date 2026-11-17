package store_test

import (
	"bytes"
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"

	"github.com/asterism-dev/observatory/internal/db/dbtest"
	"github.com/asterism-dev/observatory/internal/seal"
	"github.com/asterism-dev/observatory/internal/store"
)

func testKey(b byte) seal.Key { return seal.Key(bytes.Repeat([]byte{b}, 32)) }

func newEnvStore(t *testing.T) (*store.Store, store.LoginResult, store.Org, store.Team) {
	t.Helper()
	ctx := context.Background()
	s := store.New(dbtest.New(t)).WithMasterKey(testKey(1))
	u := login(t, s, "env")
	org, err := s.CreateOrg(ctx, "E", u.User.ID)
	if err != nil {
		t.Fatal(err)
	}
	team, err := s.CreateTeam(ctx, org.ID, "core")
	if err != nil {
		t.Fatal(err)
	}
	return s, u, org, team
}

func mustCreateOrg(t *testing.T, s *store.Store, name string, owner uuid.UUID) store.Org {
	t.Helper()
	org, err := s.CreateOrg(context.Background(), name, owner)
	if err != nil {
		t.Fatal(err)
	}
	return org
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

func envVar(org store.Org, user uuid.UUID, kind store.EnvKind, scope store.EnvScope, owner *uuid.UUID, name string) store.EnvVar {
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
	if _, err := s.WithMasterKey(testKey(2)).EnvValue(ctx, v); err == nil {
		t.Fatal("wrong master key decrypted")
	}
}

func TestEnvVarUpdateKeepsValue(t *testing.T) {
	ctx := context.Background()
	s, u, org, team := newEnvStore(t)
	v := mustCreateEnv(t, s, envVar(org, u.User.ID, store.EnvPlain, store.EnvScopeOrg, nil, "A"), "keep")
	v.Name, v.Kind = "B", store.EnvSecret
	if err := s.UpdateEnvVar(ctx, v, nil); err != nil {
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
	if err := s.UpdateEnvVar(ctx, got, &nv); err != nil {
		t.Fatal(err)
	}
	if val, err := s.EnvValue(ctx, got); err != nil || val != "new" {
		t.Fatalf("%q %v", val, err)
	}

	got.Scope, got.OwnerID = store.EnvScopeTeam, &team.ID
	if err := s.MoveEnvVar(ctx, got); err != nil {
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
	moved.Scope, moved.OwnerID = store.EnvScopeOrg, nil
	if err := s.MoveEnvVar(ctx, moved); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("move onto existing: %v", err)
	}
	if err := s.UpdateEnvVar(ctx, store.EnvVar{ID: uuid.New(), OrgID: org.ID, Name: "Z", Kind: store.EnvPlain, UpdatedBy: u.User.ID}, nil); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("missing: %v", err)
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

func TestRemoveMemberDeletesPrivateEnv(t *testing.T) {
	ctx := context.Background()
	pool := dbtest.New(t)
	s := store.New(pool).WithMasterKey(testKey(1))
	owner := login(t, s, "owner")
	org := mustCreateOrg(t, s, "R", owner.User.ID)
	m := login(t, s, "member")
	execSQL(t, pool, `INSERT INTO memberships (org_id, user_id, role) VALUES ($1, $2, 'member')`, org.ID, m.User.ID)
	mid := m.User.ID
	private := mustCreateEnv(t, s, envVar(org, mid, store.EnvSecret, store.EnvScopeUser, &mid, "P"), "p")
	shared := mustCreateEnv(t, s, envVar(org, mid, store.EnvPlain, store.EnvScopeOrg, nil, "S"), "s")
	ownerVar := mustCreateEnv(t, s, envVar(org, owner.User.ID, store.EnvPlain, store.EnvScopeUser, &owner.User.ID, "O"), "o")
	if err := s.RemoveMember(ctx, org.ID, mid); err != nil {
		t.Fatal(err)
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

func TestDeleteTeamDeletesTeamEnv(t *testing.T) {
	ctx := context.Background()
	s, u, org, team := newEnvStore(t)
	v := mustCreateEnv(t, s, envVar(org, u.User.ID, store.EnvPlain, store.EnvScopeTeam, &team.ID, "T"), "t")
	if err := s.DeleteTeam(ctx, org.ID, team.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.EnvVar(ctx, org.ID, v.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("%v", err)
	}
	if err := s.DeleteEnvVar(ctx, org.ID, v.ID); !errors.Is(err, store.ErrNotFound) {
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

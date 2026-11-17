package store

import (
	"context"
	"errors"
	"slices"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/asterism-dev/observatory/internal/authz"
	"github.com/asterism-dev/observatory/internal/db/dbtest"
)

type execer interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
}

func mustExec(ctx context.Context, t *testing.T, q execer, sql string, args ...any) {
	t.Helper()
	if _, err := q.Exec(ctx, sql, args...); err != nil {
		t.Fatal(err)
	}
}

func TestPolicySnapshot(t *testing.T) {
	ctx := context.Background()
	pool := dbtest.New(t)
	s := New(pool)
	var org, user, team, emptyTeam, parent, child uuid.UUID
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	must(pool.QueryRow(ctx, `INSERT INTO users (subject,email,name) VALUES ('s','e','n') RETURNING id`).Scan(&user))
	must(pool.QueryRow(ctx, `INSERT INTO orgs (name) VALUES ('o') RETURNING id`).Scan(&org))
	mustExec(ctx, t, pool, `INSERT INTO memberships (org_id,user_id,role) VALUES ($1,$2,'admin')`, org, user)
	must(pool.QueryRow(ctx, `INSERT INTO teams (org_id,name) VALUES ($1,'t') RETURNING id`, org).Scan(&team))
	mustExec(ctx, t, pool, `INSERT INTO team_members (team_id,user_id,admin) VALUES ($1,$2,true)`, team, user)
	must(pool.QueryRow(ctx, `INSERT INTO teams (org_id,name) VALUES ($1,'empty') RETURNING id`, org).Scan(&emptyTeam))
	must(pool.QueryRow(ctx, `INSERT INTO roles (org_id,name,description) VALUES ($1,'parent','pd') RETURNING id`, org).Scan(&parent))
	must(pool.QueryRow(ctx, `INSERT INTO roles (org_id,name) VALUES ($1,'child') RETURNING id`, org).Scan(&child))
	mustExec(ctx, t, pool, `INSERT INTO role_rules (role_id,position,effect,object,action) VALUES
		($1,1,'deny','vms/*','vm:delete'), ($1,0,'allow','vms','vm:read')`, child)
	mustExec(ctx, t, pool, `INSERT INTO role_parents (role_id,parent_builtin) VALUES ($1,'member')`, parent)
	mustExec(ctx, t, pool, `INSERT INTO role_parents (role_id,parent_role) VALUES ($1,$2)`, child, parent)
	mustExec(ctx, t, pool, `INSERT INTO role_bindings (org_id,subject,role_id) VALUES ($1,$2,$3)`, org, authz.TeamSubject(team), child)

	p, err := s.PolicySnapshot(ctx)
	must(err)
	if len(p.Memberships) != 1 || p.Memberships[0] != (authz.MemberRole{OrgID: org, UserID: user, Role: "admin"}) {
		t.Errorf("memberships: %+v", p.Memberships)
	}
	if len(p.TeamMembers) != 1 || p.TeamMembers[0] != (authz.TeamMember{OrgID: org, TeamID: team, UserID: user, Admin: true}) {
		t.Errorf("team members: %+v", p.TeamMembers)
	}
	if len(p.Teams) != 2 || !slices.Contains(p.Teams, authz.TeamRef{OrgID: org, ID: emptyTeam}) {
		t.Errorf("teams: %+v", p.Teams)
	}
	if len(p.Bindings) != 1 || p.Bindings[0] != (authz.Binding{OrgID: org, Subject: authz.TeamSubject(team), RoleID: child}) {
		t.Errorf("bindings: %+v", p.Bindings)
	}
	if len(p.Roles) != 2 {
		t.Fatalf("roles: %+v", p.Roles)
	}
	for _, r := range p.Roles {
		switch r.ID {
		case parent:
			if r.OrgID != org || r.Name != "parent" || r.Description != "pd" || len(r.Rules) != 0 ||
				len(r.Parents) != 1 || r.Parents[0] != "member" {
				t.Errorf("parent role: %+v", r)
			}
		case child:
			want := []authz.Rule{{Effect: authz.Allow, Object: "vms", Action: "vm:read"}, {Effect: authz.Deny, Object: "vms/*", Action: "vm:delete"}}
			if len(r.Rules) != 2 || r.Rules[0] != want[0] || r.Rules[1] != want[1] {
				t.Errorf("child rules not ordered by position: %+v", r.Rules)
			}
			if len(r.Parents) != 1 || r.Parents[0] != parent.String() {
				t.Errorf("child parents: %+v", r.Parents)
			}
		default:
			t.Errorf("unexpected role %s", r.ID)
		}
	}
}

func TestPolicySnapshotSeesOwnTransaction(t *testing.T) {
	ctx := context.Background()
	pool := dbtest.New(t)
	s := New(pool)
	var org uuid.UUID
	if err := pool.QueryRow(ctx, `INSERT INTO orgs (name) VALUES ('o') RETURNING id`).Scan(&org); err != nil {
		t.Fatal(err)
	}
	err := s.InTx(ctx, func(tx *Store) error {
		mustExec(ctx, t, tx.q, `INSERT INTO roles (org_id,name) VALUES ($1,'r')`, org)
		p, err := tx.PolicySnapshot(ctx)
		if err != nil {
			return err
		}
		if len(p.Roles) != 1 {
			t.Errorf("tx snapshot missed uncommitted role: %+v", p.Roles)
		}
		outside, err := s.PolicySnapshot(ctx)
		if err != nil {
			return err
		}
		if len(outside.Roles) != 0 {
			t.Errorf("outside snapshot saw uncommitted role: %+v", outside.Roles)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestRoleStore(t *testing.T) {
	ctx := context.Background()
	pool := dbtest.New(t)
	s := New(pool)
	var org, orgB, user, outsider, team, teamB uuid.UUID
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	must(pool.QueryRow(ctx, `INSERT INTO users (subject,email,name) VALUES ('s','e','n') RETURNING id`).Scan(&user))
	must(pool.QueryRow(ctx, `INSERT INTO users (subject,email,name) VALUES ('o','o','o') RETURNING id`).Scan(&outsider))
	must(pool.QueryRow(ctx, `INSERT INTO orgs (name) VALUES ('a') RETURNING id`).Scan(&org))
	must(pool.QueryRow(ctx, `INSERT INTO orgs (name) VALUES ('b') RETURNING id`).Scan(&orgB))
	mustExec(ctx, t, pool, `INSERT INTO memberships (org_id,user_id,role) VALUES ($1,$2,'member'), ($3,$4,'owner')`, org, user, orgB, outsider)
	must(pool.QueryRow(ctx, `INSERT INTO teams (org_id,name) VALUES ($1,'t') RETURNING id`, org).Scan(&team))
	must(pool.QueryRow(ctx, `INSERT INTO teams (org_id,name) VALUES ($1,'t') RETURNING id`, orgB).Scan(&teamB))

	rules := []authz.Rule{{Effect: authz.Allow, Object: "org/audit", Action: "audit:read"}, {Effect: authz.Deny, Object: "org/teams/*", Action: "team:delete"}}
	a, err := s.CreateRole(ctx, org, authz.CustomRole{Name: "a", Description: "d", Rules: rules, Parents: []string{"member"}})
	must(err)
	if a.ID == uuid.Nil || a.OrgID != org {
		t.Fatalf("created: %+v", a)
	}
	if _, err := s.CreateRole(ctx, org, authz.CustomRole{Name: "a"}); !errors.Is(err, ErrConflict) {
		t.Fatalf("duplicate name: %v", err)
	}
	b, err := s.CreateRole(ctx, org, authz.CustomRole{Name: "b", Parents: []string{a.ID.String()}})
	must(err)
	foreign, err := s.CreateRole(ctx, orgB, authz.CustomRole{Name: "f"})
	must(err)
	if _, err := s.CreateRole(ctx, org, authz.CustomRole{Name: "c", Parents: []string{foreign.ID.String()}}); !errors.Is(err, ErrInvalidParent) {
		t.Fatalf("foreign parent: %v", err)
	}
	for _, parent := range []uuid.UUID{a.ID, b.ID} {
		if err := s.UpdateRole(ctx, org, authz.CustomRole{ID: a.ID, Name: "a", Parents: []string{parent.String()}}); !errors.Is(err, ErrConflict) {
			t.Fatalf("cycle via %s: %v", parent, err)
		}
	}
	if err := s.UpdateRole(ctx, org, authz.CustomRole{ID: foreign.ID, Name: "x"}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("update foreign: %v", err)
	}
	must(s.UpdateRole(ctx, org, authz.CustomRole{ID: b.ID, Name: "b2", Rules: rules[:1], Parents: []string{"admin"}}))
	got, err := s.Role(ctx, org, b.ID)
	must(err)
	if got.Name != "b2" || len(got.Rules) != 1 || len(got.Parents) != 1 || got.Parents[0] != "admin" {
		t.Fatalf("updated: %+v", got)
	}
	if _, err := s.Role(ctx, org, foreign.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("foreign role: %v", err)
	}
	roles, err := s.Roles(ctx, org)
	must(err)
	if len(roles) != 2 || roles[0].ID != a.ID || len(roles[0].Rules) != 2 || roles[0].Rules[1] != rules[1] {
		t.Fatalf("roles: %+v", roles)
	}

	bu, err := s.Bind(ctx, org, authz.UserSubject(user), a.ID)
	must(err)
	if _, err := s.Bind(ctx, org, authz.TeamSubject(team), a.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Bind(ctx, org, authz.UserSubject(user), a.ID); !errors.Is(err, ErrConflict) {
		t.Fatalf("duplicate binding: %v", err)
	}
	for _, sub := range []string{authz.UserSubject(outsider), authz.TeamSubject(teamB), "bogus", "user:" + user.String() + " "} {
		if _, err := s.Bind(ctx, org, sub, a.ID); !errors.Is(err, ErrNotMember) {
			t.Fatalf("bind %q: %v", sub, err)
		}
	}
	if _, err := s.Bind(ctx, org, authz.UserSubject(user), foreign.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("foreign role binding: %v", err)
	}
	bs, err := s.Bindings(ctx, org)
	must(err)
	if len(bs) != 2 || bs[0].ID != bu.ID || bs[0].Subject != authz.UserSubject(user) || bs[0].RoleID != a.ID {
		t.Fatalf("bindings: %+v", bs)
	}
	if _, err := s.Unbind(ctx, orgB, bu.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unbind via other org: %v", err)
	}
	removed, err := s.Unbind(ctx, org, bu.ID)
	must(err)
	if removed.RoleID != a.ID || removed.Subject != authz.UserSubject(user) {
		t.Fatalf("unbound: %+v", removed)
	}
	if err := s.DeleteRole(ctx, orgB, a.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("delete via other org: %v", err)
	}
	must(s.DeleteRole(ctx, org, a.ID))
	if bs, _ := s.Bindings(ctx, org); len(bs) != 0 {
		t.Fatalf("bindings survive role delete: %+v", bs)
	}
	if err := s.DeleteRole(ctx, org, a.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("delete again: %v", err)
	}
}

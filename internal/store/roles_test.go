package store

import (
	"context"
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
	var org, user, team, parent, child uuid.UUID
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

package store_test

import (
	"context"
	"slices"
	"sync"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/asterism-dev/observatory/internal/authz"
	"github.com/asterism-dev/observatory/internal/authz/authztest"
	"github.com/asterism-dev/observatory/internal/store"
)

// The hub keeps no IAM tables; these stand in for iam's users, orgs, teams and memberships, one policy per store.

type testUser struct{ ID uuid.UUID }

type loginResult struct {
	User        testUser
	PersonalOrg *testOrg
}

type testOrg struct {
	ID   uuid.UUID
	Name string
}

type testTeam struct {
	ID, OrgID uuid.UUID
	Name      string
}

var policies sync.Map

func policyOf(s *store.Store) *iamPolicy {
	p, _ := policies.LoadOrStore(s, &iamPolicy{})
	return p.(*iamPolicy)
}

type iamPolicy struct {
	mu sync.Mutex
	p  authz.Policy
}

func (f *iamPolicy) change(fn func(*authz.Policy)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	fn(&f.p)
}

func login(t *testing.T, s *store.Store, _ string) loginResult {
	t.Helper()
	u, org := uuid.New(), uuid.New()
	addMember(s, org, u, store.RoleOwner)
	return loginResult{User: testUser{u}, PersonalOrg: &testOrg{ID: org}}
}

func createOrg(s *store.Store, _ context.Context, name string, owner uuid.UUID) (testOrg, error) {
	org := testOrg{ID: uuid.New(), Name: name}
	addMember(s, org.ID, owner, store.RoleOwner)
	return org, nil
}

func mustCreateOrg(t *testing.T, s *store.Store, name string, owner uuid.UUID) testOrg {
	t.Helper()
	org, _ := createOrg(s, context.Background(), name, owner)
	return org
}

func createTeam(s *store.Store, _ context.Context, org uuid.UUID, name string) (testTeam, error) {
	team := testTeam{ID: uuid.New(), OrgID: org, Name: name}
	policyOf(s).change(func(p *authz.Policy) { p.Teams = append(p.Teams, authz.TeamRef{OrgID: org, ID: team.ID}) })
	return team, nil
}

func addMember(s *store.Store, org, user uuid.UUID, role store.Role) {
	policyOf(s).change(func(p *authz.Policy) {
		p.Memberships = append(p.Memberships, authz.MemberRole{OrgID: org, UserID: user, Role: string(role)})
	})
}

func setTeamMember(s *store.Store, _ context.Context, org, team, user uuid.UUID, admin bool) error {
	policyOf(s).change(func(p *authz.Policy) {
		p.TeamMembers = append(p.TeamMembers, authz.TeamMember{OrgID: org, TeamID: team, UserID: user, Admin: admin})
	})
	return nil
}

func removeMember(s *store.Store, _ context.Context, org, user uuid.UUID) error {
	policyOf(s).change(func(p *authz.Policy) {
		p.Memberships = slices.DeleteFunc(p.Memberships, func(m authz.MemberRole) bool { return m.OrgID == org && m.UserID == user })
		p.TeamMembers = slices.DeleteFunc(p.TeamMembers, func(m authz.TeamMember) bool { return m.OrgID == org && m.UserID == user })
	})
	return nil
}

// policySnapshot is iam's policy merged with the hub's owner refs, as the hub enforces it.
func policySnapshot(s *store.Store, ctx context.Context) (authz.Policy, error) {
	f := policyOf(s)
	f.mu.Lock()
	p := f.p
	p.Memberships, p.Teams, p.TeamMembers = slices.Clone(p.Memberships), slices.Clone(p.Teams), slices.Clone(p.TeamMembers)
	f.mu.Unlock()
	p.Catalogs = authztest.Legacy()
	var err error
	p.Owned, err = s.OwnerRefs(ctx)
	return p, err
}

// memberIn answers like iam's GetMembership.
func memberIn(s *store.Store, org, user uuid.UUID) func(context.Context) (bool, error) {
	return func(ctx context.Context) (bool, error) {
		p, err := policySnapshot(s, ctx)
		return slices.ContainsFunc(p.Memberships, func(m authz.MemberRole) bool { return m.OrgID == org && m.UserID == user }), err
	}
}

func execSQL(t *testing.T, pool *pgxpool.Pool, sql string, args ...any) {
	t.Helper()
	if _, err := pool.Exec(context.Background(), sql, args...); err != nil {
		t.Fatal(err)
	}
}

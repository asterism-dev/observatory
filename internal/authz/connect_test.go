package authz

import (
	"math/rand/v2"
	"slices"
	"testing"

	"github.com/google/uuid"
)

// referenceConnect is the casbin-backed definition: the owner relation of a known VM, then Allowed.
func referenceConnect(e *Enforcer, p Policy, user uuid.UUID, vm VMRef) bool {
	if !slices.ContainsFunc(p.VMs, func(k VMRef) bool {
		return k.ID == vm.ID && k.OrgID == vm.OrgID && sameOwner(k.UserID, vm.UserID) && sameOwner(k.TeamID, vm.TeamID)
	}) {
		return false
	}
	switch {
	case vm.UserID != nil:
		if *vm.UserID != user {
			return false
		}
	case vm.TeamID != nil:
		if !slices.ContainsFunc(p.TeamMembers, func(tm TeamMember) bool {
			return tm.OrgID == vm.OrgID && tm.TeamID == *vm.TeamID && tm.UserID == user
		}) {
			return false
		}
	default:
		return false
	}
	return e.Allowed(UserSubject(user), vm.OrgID, VMObject(vm.ID), "vm:connect")
}

func randomConnectPolicy(r *rand.Rand) (Policy, []uuid.UUID, []VMRef) {
	pick := func(n int) int { return r.IntN(n) }
	orgs := []uuid.UUID{uuid.New(), uuid.New()}
	users := make([]uuid.UUID, 8)
	var p Policy
	for i := range users {
		users[i] = uuid.New()
		for oi, org := range orgs {
			if oi == 0 || pick(2) == 0 {
				p.Memberships = append(p.Memberships, MemberRole{OrgID: org, UserID: users[i], Role: []string{"owner", "admin", "member"}[pick(3)]})
			}
		}
	}
	member := func(org, u uuid.UUID) bool {
		return slices.ContainsFunc(p.Memberships, func(m MemberRole) bool { return m.OrgID == org && m.UserID == u })
	}
	type team struct{ org, id uuid.UUID }
	var teams []team
	for i := range 5 {
		t := team{orgs[i%2], uuid.New()}
		teams = append(teams, t)
		p.Teams = append(p.Teams, TeamRef{OrgID: t.org, ID: t.id})
		for _, u := range users {
			if member(t.org, u) && pick(3) == 0 {
				p.TeamMembers = append(p.TeamMembers, TeamMember{OrgID: t.org, TeamID: t.id, UserID: u, Admin: pick(4) == 0})
			}
		}
	}
	for range 10 {
		if pick(2) == 0 {
			t := teams[pick(len(teams))]
			p.VMs = append(p.VMs, VMRef{OrgID: t.org, ID: uuid.New(), TeamID: &t.id})
			continue
		}
		org := orgs[pick(2)]
		u := users[pick(len(users))]
		if member(org, u) {
			p.VMs = append(p.VMs, VMRef{OrgID: org, ID: uuid.New(), UserID: &u})
		}
	}
	for _, org := range orgs {
		var roles []CustomRole
		for range 1 + pick(3) {
			objects := []string{"org/*", ObjVMs, ObjVMs + "/*", "org/teams/*", UserSubject(users[pick(len(users))]) + "/*"}
			if len(p.VMs) > 0 {
				objects = append(objects, VMObject(p.VMs[pick(len(p.VMs))].ID))
			}
			cr := CustomRole{ID: uuid.New(), OrgID: org, Name: "r"}
			for range 1 + pick(3) {
				cr.Rules = append(cr.Rules, Rule{[]Effect{Allow, Deny}[pick(2)], objects[pick(len(objects))], []string{"vm:connect", "vm:*", "*", "vm:read"}[pick(4)]})
			}
			if len(roles) > 0 && pick(2) == 0 {
				cr.Parents = []string{roles[pick(len(roles))].ID.String()}
			} else if pick(3) == 0 {
				cr.Parents = []string{"member"}
			}
			roles = append(roles, cr)
		}
		p.Roles = append(p.Roles, roles...)
		for _, cr := range roles {
			for range pick(3) {
				sub := UserSubject(users[pick(len(users))])
				if pick(2) == 0 {
					if t := teams[pick(len(teams))]; t.org == org {
						sub = TeamSubject(t.id)
					}
				}
				p.Bindings = append(p.Bindings, Binding{OrgID: org, Subject: sub, RoleID: cr.ID})
			}
		}
	}
	refs := slices.Clone(p.VMs)
	for _, vm := range p.VMs {
		cross := vm
		cross.OrgID = orgs[0]
		if vm.OrgID == orgs[0] {
			cross.OrgID = orgs[1]
		}
		refs = append(refs, cross)
	}
	other := users[0]
	refs = append(refs, VMRef{OrgID: orgs[0], ID: uuid.New(), UserID: &other})
	return p, users, refs
}

func TestConnectCheckerMatchesCasbin(t *testing.T) {
	r := rand.New(rand.NewPCG(1, 2))
	pairs, hits := 0, 0
	for i := range 300 {
		p, users, refs := randomConnectPolicy(r)
		e, err := New(p)
		if err != nil {
			t.Fatalf("policy %d: %v", i, err)
		}
		c := e.ConnectChecker()
		for _, u := range users {
			for _, vm := range refs {
				want := referenceConnect(e, p, u, vm)
				if got := c.CanConnect(u, vm); got != want {
					t.Fatalf("policy %d user %s vm %+v: checker %v, casbin %v\nroles %+v\nbindings %+v", i, u, vm, got, want, p.Roles, p.Bindings)
				}
				if e.CanConnect(u, vm) != want {
					t.Fatalf("policy %d: Enforcer.CanConnect disagrees", i)
				}
				pairs++
				if want {
					hits++
				}
			}
		}
	}
	if hits == 0 || hits == pairs {
		t.Fatalf("degenerate sample: %d of %d pairs connect", hits, pairs)
	}
	t.Logf("%d pairs, %d connect", pairs, hits)
}

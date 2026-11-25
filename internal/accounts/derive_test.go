package accounts

import (
	"reflect"
	"slices"
	"testing"

	"github.com/google/uuid"

	"github.com/asterism-dev/observatory/internal/authz"
	"github.com/asterism-dev/observatory/internal/policy"
	"github.com/asterism-dev/observatory/internal/store"
)

type fixture struct {
	org, team, def, ml       uuid.UUID
	owner, member, teamAdmin uuid.UUID
	snap                     store.AccountSnapshot
	policy                   authz.Policy
}

func newFixture() fixture {
	f := fixture{org: uuid.New(), team: uuid.New(), def: uuid.New(), ml: uuid.New(), owner: uuid.New(), member: uuid.New(), teamAdmin: uuid.New()}
	f.snap = store.AccountSnapshot{
		Accounts:  []store.Account{{ID: f.def, OrgID: f.org, Name: "default"}, {ID: f.ml, OrgID: f.org, TeamID: &f.team, Name: "ml"}},
		Resources: map[string]uuid.UUID{},
	}
	f.policy = authz.Policy{
		Memberships: []authz.MemberRole{{OrgID: f.org, UserID: f.owner, Role: "owner"}, {OrgID: f.org, UserID: f.member, Role: "member"}, {OrgID: f.org, UserID: f.teamAdmin, Role: "member"}},
		TeamMembers: []authz.TeamMember{{OrgID: f.org, TeamID: f.team, UserID: f.teamAdmin, Admin: true}},
		Teams:       []authz.TeamRef{{OrgID: f.org, ID: f.team}},
	}
	return f
}

func TestDeriveBuiltins(t *testing.T) {
	f := newFixture()
	r := Derive(f.policy, f.snap)
	if !reflect.DeepEqual(r[f.owner].Accounts[f.def], policy.BuiltinRole("admin")) || !reflect.DeepEqual(r[f.owner].Accounts[f.ml], policy.BuiltinRole("admin")) {
		t.Error("owner must be admin in every account")
	}
	if !reflect.DeepEqual(r[f.member].Accounts[f.def], policy.BuiltinRole("member")) || r[f.member].Accounts[f.ml] != nil {
		t.Error("member: member on default only")
	}
	if !slices.ContainsFunc(r[f.teamAdmin].Accounts[f.ml], func(x policy.Rule) bool { return slices.Contains(x.Actions, "iam:*") }) {
		t.Error("team admin must be admin in the team account")
	}
	if !slices.ContainsFunc(r[f.member].Shares, func(s policy.Share) bool { return s.Owner == f.def && slices.Contains(s.Grantees, f.ml) }) {
		t.Error("shares from default to team accounts missing")
	}
	node := policy.NewID("compute", f.def, "node", uuid.NewString())
	if d := policy.Decide(r[f.teamAdmin], policy.Request{Acting: f.ml, Action: "compute:node:use", Resource: node}); !d.Allowed {
		t.Errorf("team admin using a default node from the team account: %+v", d)
	}
}

func TestDeriveOrgWideCustomRoleInEveryAccount(t *testing.T) {
	f := newFixture()
	role := uuid.New()
	f.policy.Roles = []authz.CustomRole{{ID: role, OrgID: f.org, Name: "careful", Rules: []authz.Rule{
		{Effect: authz.Allow, Object: "org/vms/*", Action: "vm:delete"},
		{Effect: authz.Deny, Object: "org/vms/*", Action: "vm:delete"},
	}}}
	f.policy.Bindings = []authz.Binding{{OrgID: f.org, Subject: authz.UserSubject(f.member), RoleID: role}}
	r := Derive(f.policy, f.snap)
	vm := policy.NewID("compute", f.ml, "vm", uuid.NewString())
	if d := policy.Decide(r[f.member], policy.Request{Acting: f.ml, Action: "compute:vm:delete", Resource: vm}); d.Reason != "deny" {
		t.Fatalf("org-wide deny on a team account VM = %+v, want deny", d)
	}
}

func TestDeriveSubscriptionDeny(t *testing.T) {
	f := newFixture()
	alice, bob := f.owner, f.member
	sub := uuid.New()
	f.snap.Subscriptions = []store.Subscription{{ID: sub, Account: f.def, Owner: alice}}
	r := Derive(f.policy, f.snap)
	id := policy.NewID("integrations", f.def, "integration", sub.String())
	q := policy.Request{Acting: f.def, Action: "integrations:integration:use", Resource: id}
	if d := policy.Decide(r[bob], q); d.Allowed {
		t.Errorf("bob uses alice's subscription: %+v", d)
	}
	if d := policy.Decide(r[alice], q); !d.Allowed {
		t.Errorf("alice cannot use her subscription: %+v", d)
	}
	q.Acting = f.ml
	if d := policy.Decide(r[f.teamAdmin], q); d.Allowed {
		t.Errorf("team account uses alice's subscription through the share: %+v", d)
	}
}

func TestRewriteRule(t *testing.T) {
	f := newFixture()
	for _, c := range []struct {
		in   authz.Rule
		want policy.Rule
		ok   bool
	}{
		{authz.Rule{Effect: authz.Deny, Object: "org/vms/*", Action: "vm:delete"}, policy.Rule{Effect: policy.Deny, Actions: []string{"compute:vm:delete"}, Resources: []string{"obs:compute:${account}:vm/*"}}, true},
		{authz.Rule{Effect: authz.Allow, Object: "org/team-env/*", Action: "env:use"}, policy.Rule{Effect: policy.Allow, Actions: []string{"secrets:env:use"}, Resources: []string{"obs:secrets:" + f.ml.String() + ":env/*"}}, true},
		{authz.Rule{Effect: authz.Allow, Object: "org/team-vms/" + f.team.String(), Action: "vm:create"}, policy.Rule{Effect: policy.Allow, Actions: []string{"compute:vm:create"}, Resources: []string{"obs:compute:" + f.ml.String() + ":vm/*"}}, true},
		{authz.Rule{Effect: authz.Allow, Object: "org/settings", Action: "telemetry:token"}, policy.Rule{Effect: policy.Allow, Actions: []string{"telemetry:token:create"}, Resources: []string{"obs:telemetry:" + f.def.String() + ":token/*"}}, true},
		{authz.Rule{Effect: authz.Allow, Object: "org/members", Action: "member:invite"}, policy.Rule{}, false},
		{authz.Rule{Effect: authz.Allow, Object: "org/user-devices/*", Action: "device:create"}, policy.Rule{}, false},
	} {
		got, ok := RewriteRule(c.in, f.org, f.snap)
		if ok != c.ok || ok && !reflect.DeepEqual(got, c.want) {
			t.Errorf("RewriteRule(%+v) = %+v, %v", c.in, got, ok)
		}
	}
}

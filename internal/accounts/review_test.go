package accounts

import (
	"context"
	"slices"
	"testing"

	"github.com/google/uuid"

	"github.com/asterism-dev/observatory/internal/authz"
	"github.com/asterism-dev/observatory/internal/policy"
	"github.com/asterism-dev/observatory/internal/store"
)

func TestShadowReloadRecovers(t *testing.T) {
	sh := NewShadow(func(context.Context) (store.AccountSnapshot, error) { panic("boom") })
	sh.Reload(context.Background(), authz.Policy{})
}

// teamBound binds a one-rule custom role to the fixture's team.
func teamBound(f *fixture, r authz.Rule) {
	role := uuid.New()
	f.policy.Roles = append(f.policy.Roles, authz.CustomRole{ID: role, OrgID: f.org, Name: "r", Rules: []authz.Rule{r}})
	f.policy.Bindings = append(f.policy.Bindings, authz.Binding{OrgID: f.org, Subject: authz.TeamSubject(f.team), RoleID: role})
}

func TestDeriveTeamBindingAppliesOrgWide(t *testing.T) {
	f := newFixture()
	teamBound(&f, authz.Rule{Effect: authz.Deny, Object: "org/vms/*", Action: "vm:delete"})
	r := Derive(f.policy, f.snap)
	own := policy.NewID("compute", f.def, "vm", uuid.NewString())
	if d := policy.Decide(r[f.teamAdmin], policy.Request{Acting: f.def, Action: "compute:vm:delete", Resource: own}); d.Reason != "deny" {
		t.Fatalf("team-bound deny on a VM in default = %+v, want deny", d)
	}
}

func TestRewriteScopesStayScoped(t *testing.T) {
	f := newFixture()
	r := Derive(func() authz.Policy {
		role := uuid.New()
		f.policy.Roles = []authz.CustomRole{{ID: role, OrgID: f.org, Name: "r", Rules: []authz.Rule{
			{Effect: authz.Deny, Object: "org/team-vms/*", Action: "vm:create"},
			{Effect: authz.Deny, Object: "org/integrations", Action: "integration:use"},
		}}}
		f.policy.Bindings = []authz.Binding{{OrgID: f.org, Subject: authz.UserSubject(f.owner), RoleID: role}}
		return f.policy
	}(), f.snap)
	if d := policy.Decide(r[f.owner], policy.Request{Acting: f.def, Action: "compute:vm:create", Resource: policy.NewID("compute", f.def, "vm", "-")}); !d.Allowed {
		t.Errorf("deny on team VMs blocked creating a VM in default: %+v", d)
	}
	if d := policy.Decide(r[f.owner], policy.Request{Acting: f.ml, Action: "compute:vm:create", Resource: policy.NewID("compute", f.ml, "vm", "-")}); d.Allowed {
		t.Errorf("deny on team VMs did not apply in the team account: %+v", d)
	}
	if d := policy.Decide(r[f.owner], policy.Request{Acting: f.ml, Action: "integrations:integration:use", Resource: policy.NewID("integrations", f.ml, "integration", "-")}); !d.Allowed {
		t.Errorf("deny on org integrations blocked team integrations: %+v", d)
	}
}

func TestCompareReportsCollapsedRules(t *testing.T) {
	f := newFixture()
	role := uuid.New()
	f.policy.Roles = []authz.CustomRole{{ID: role, OrgID: f.org, Name: "no-private-env", Rules: []authz.Rule{{Effect: authz.Deny, Object: "org/user-env/*", Action: "env:set"}}}}
	f.policy.Bindings = []authz.Binding{{OrgID: f.org, Subject: authz.UserSubject(f.member), RoleID: role}}
	rep, err := Compare(f.policy, f.snap)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.ContainsFunc(rep.Collapsed, func(c string) bool { return c == "no-private-env: deny env:set on org/user-env/*" }) {
		t.Fatalf("Collapsed = %v", rep.Collapsed)
	}
}

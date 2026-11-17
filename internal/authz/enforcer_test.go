package authz

import (
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/google/uuid"
)

func mustNew(t *testing.T, p Policy) *Enforcer {
	t.Helper()
	e, err := New(p)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return e
}

// concrete turns a catalog object pattern into an object a request would carry.
func concrete(obj string) string {
	return strings.Replace(obj, "*", uuid.NewString(), 1)
}

func TestBuiltinRoles(t *testing.T) {
	org := uuid.New()
	owner, admin, member := uuid.New(), uuid.New(), uuid.New()
	e := mustNew(t, Policy{Memberships: []MemberRole{
		{OrgID: org, UserID: owner, Role: "owner"},
		{OrgID: org, UserID: admin, Role: "admin"},
		{OrgID: org, UserID: member, Role: "member"},
	}})
	memberActs := []string{"member:read", "team:read", "team:leave", "role:read", "limit:read"}
	want := map[uuid.UUID]func(act string) bool{
		owner:  func(string) bool { return true },
		admin:  func(act string) bool { return act != "member:set-role" },
		member: func(act string) bool { return slices.Contains(memberActs, act) },
	}
	names := map[uuid.UUID]string{owner: "owner", admin: "admin", member: "member"}
	for u, allowed := range want {
		for _, p := range Catalog {
			if got := e.Allowed(UserSubject(u), org, concrete(p.Object), p.Action); got != allowed(p.Action) {
				t.Errorf("%s %s@%s: got %v", names[u], p.Action, p.Object, got)
			}
		}
	}
	if e.Allowed(UserSubject(member), org, TeamObject(uuid.New()), "team:read") != true {
		t.Error("member should read a single team")
	}
}

func TestCustomRoleInheritsBuiltin(t *testing.T) {
	org, u, role := uuid.New(), uuid.New(), uuid.New()
	e := mustNew(t, Policy{
		Memberships: []MemberRole{{OrgID: org, UserID: u, Role: "member"}},
		Roles: []CustomRole{{ID: role, OrgID: org, Name: "auditor", Parents: []string{"member"},
			Rules: []Rule{{Effect: Allow, Object: ObjAudit, Action: "audit:read"}}}},
		Bindings: []Binding{{OrgID: org, Subject: UserSubject(u), RoleID: role}},
	})
	if !e.Allowed(UserSubject(u), org, ObjAudit, "audit:read") {
		t.Error("custom role should grant audit:read")
	}
	if !e.Allowed(RoleSubject(role.String()), org, ObjMembers, "member:read") {
		t.Error("custom role should inherit member:read from parent")
	}
	if e.Allowed(UserSubject(u), org, ObjLimits, "limit:set") {
		t.Error("custom role must not grant limit:set")
	}
}

func TestDenyWins(t *testing.T) {
	org, u, role := uuid.New(), uuid.New(), uuid.New()
	e := mustNew(t, Policy{
		Memberships: []MemberRole{{OrgID: org, UserID: u, Role: "admin"}},
		Roles: []CustomRole{{ID: role, OrgID: org, Name: "no-limits",
			Rules: []Rule{{Effect: Deny, Object: ObjLimits, Action: "limit:set"}}}},
		Bindings: []Binding{{OrgID: org, Subject: UserSubject(u), RoleID: role}},
	})
	if e.Allowed(UserSubject(u), org, ObjLimits, "limit:set") {
		t.Error("deny should win over admin allow")
	}
	if !e.Allowed(UserSubject(u), org, ObjLimits, "limit:read") {
		t.Error("unrelated permission should stay allowed")
	}
}

func TestTeamSubject(t *testing.T) {
	org, in, out, team, role := uuid.New(), uuid.New(), uuid.New(), uuid.New(), uuid.New()
	e := mustNew(t, Policy{
		Memberships: []MemberRole{{OrgID: org, UserID: in, Role: "member"}, {OrgID: org, UserID: out, Role: "member"}},
		TeamMembers: []TeamMember{{OrgID: org, TeamID: team, UserID: in}},
		Roles: []CustomRole{{ID: role, OrgID: org, Name: "auditors",
			Rules: []Rule{{Effect: Allow, Object: ObjAudit, Action: "audit:read"}}}},
		Bindings: []Binding{{OrgID: org, Subject: TeamSubject(team), RoleID: role}},
	})
	if !e.Allowed(UserSubject(in), org, ObjAudit, "audit:read") {
		t.Error("team member should get the team's role")
	}
	if e.Allowed(UserSubject(out), org, ObjAudit, "audit:read") {
		t.Error("non-member must not get the team's role")
	}
}

func TestTeamAdminManagesOwnTeamOnly(t *testing.T) {
	org, u, mine, other := uuid.New(), uuid.New(), uuid.New(), uuid.New()
	e := mustNew(t, Policy{
		Memberships: []MemberRole{{OrgID: org, UserID: u, Role: "member"}},
		TeamMembers: []TeamMember{{OrgID: org, TeamID: mine, UserID: u, Admin: true}, {OrgID: org, TeamID: other, UserID: u}},
	})
	if !e.Allowed(UserSubject(u), org, TeamObject(mine), "team:manage-members") {
		t.Error("team admin should manage own team")
	}
	if e.Allowed(UserSubject(u), org, TeamObject(other), "team:manage-members") {
		t.Error("plain team member must not manage the team")
	}
	if e.Allowed(UserSubject(u), org, TeamObject(mine), "team:update") {
		t.Error("team admin must not update the team")
	}
}

func TestCrossOrgIsolation(t *testing.T) {
	a, b, u, team, role := uuid.New(), uuid.New(), uuid.New(), uuid.New(), uuid.New()
	e := mustNew(t, Policy{
		Memberships: []MemberRole{{OrgID: a, UserID: u, Role: "admin"}},
		TeamMembers: []TeamMember{{OrgID: a, TeamID: team, UserID: u, Admin: true}},
		Roles: []CustomRole{{ID: role, OrgID: a, Name: "x",
			Rules: []Rule{{Effect: Allow, Object: "org/*", Action: "*"}}}},
		Bindings: []Binding{{OrgID: a, Subject: UserSubject(u), RoleID: role}, {OrgID: a, Subject: TeamSubject(team), RoleID: role}},
	})
	if !e.Allowed(UserSubject(u), a, ObjMembers, "member:set-role") {
		t.Error("custom role should grant everything in A")
	}
	for _, p := range Catalog {
		if e.Allowed(UserSubject(u), b, concrete(p.Object), p.Action) {
			t.Errorf("%s@%s allowed in B", p.Action, p.Object)
		}
	}
	if e.Allowed(UserSubject(u), b, TeamObject(team), "team:manage-members") {
		t.Error("team admin rights leaked into B")
	}
	if e.Allowed(UserSubject(u), b, "user:"+u.String()+"/vm/x", "vm:delete") {
		t.Error("own-resource rule leaked into B (not a member)")
	}
}

func TestUserOwnsOwnObjects(t *testing.T) {
	org, u, other, role := uuid.New(), uuid.New(), uuid.New(), uuid.New()
	p := Policy{Memberships: []MemberRole{{OrgID: org, UserID: u, Role: "member"}, {OrgID: org, UserID: other, Role: "member"}}}
	e := mustNew(t, p)
	if !e.Allowed(UserSubject(u), org, "user:"+u.String()+"/vm/x", "vm:delete") {
		t.Error("user should delete own vm")
	}
	if e.Allowed(UserSubject(u), org, "user:"+other.String()+"/vm/x", "vm:delete") {
		t.Error("user must not delete another user's vm")
	}
	p.Roles = []CustomRole{{ID: role, OrgID: org, Name: "no-delete",
		Rules: []Rule{{Effect: Deny, Object: "user:" + u.String() + "/*", Action: "vm:delete"}}}}
	p.Bindings = []Binding{{OrgID: org, Subject: UserSubject(u), RoleID: role}}
	e = mustNew(t, p)
	if e.Allowed(UserSubject(u), org, "user:"+u.String()+"/vm/x", "vm:delete") {
		t.Error("org deny should win over own-resource allow")
	}
}

func TestValidatePattern(t *testing.T) {
	id := uuid.NewString()
	cases := map[string]bool{
		ObjSettings: true, ObjMembers: true, ObjTeams: true, ObjRoles: true, ObjLimits: true, ObjAudit: true,
		"org/teams/*":                        true,
		"org/*":                              true,
		"org/vm/*":                           true,
		"org/vm/abc":                         true,
		"user:" + id + "/*":                  true,
		"user:" + id + "/vm/*":               true,
		"user:" + id + "/vm/abc":             true,
		"":                                   false,
		"*":                                  false,
		"org":                                false,
		"org/":                               false,
		"org/vm":                             false,
		"org/vm*":                            false,
		"org/**":                             false,
		"org/*/x":                            false,
		"org/vm/a*":                          false,
		"org/vm/a/b":                         false,
		"org//x":                             false,
		"x/*":                                false,
		"user:nope/*":                        false,
		"user:" + strings.ToUpper(id) + "/*": false,
		"user:{" + id + "}/*":                false,
		"user:" + id:                         false,
		"user:" + id + "/":                   false,
		"user:" + id + "/vm":                 false,
		"user:" + id + "/vm/a/b":             false,
		"user:" + id + "/vm/*/x":             false,
	}
	for in, ok := range cases {
		if err := ValidatePattern(in); (err == nil) != ok {
			t.Errorf("ValidatePattern(%q) = %v, want ok=%v", in, err, ok)
		}
	}
}

func TestExpandActions(t *testing.T) {
	if got := ExpandActions("*"); len(got) != len(Catalog) {
		t.Errorf("*: got %d actions, want %d", len(got), len(Catalog))
	}
	want := []string{"team:read", "team:create", "team:update", "team:delete", "team:manage-members", "team:leave"}
	if got := ExpandActions("team:*"); !reflect.DeepEqual(got, want) {
		t.Errorf("team:*: got %v", got)
	}
	if got := ExpandActions("audit:read"); !reflect.DeepEqual(got, []string{"audit:read"}) {
		t.Errorf("exact: got %v", got)
	}
	if got := ExpandActions("vm:delete"); len(got) != 0 {
		t.Errorf("unknown: got %v", got)
	}
}

func TestDenyRulesFor(t *testing.T) {
	org, otherOrg, u, team := uuid.New(), uuid.New(), uuid.New(), uuid.New()
	direct, parent, viaTeam, teamParent, foreign := uuid.New(), uuid.New(), uuid.New(), uuid.New(), uuid.New()
	deny := func(obj string) []Rule {
		return []Rule{{Effect: Deny, Object: obj, Action: "*"}, {Effect: Allow, Object: obj, Action: "x:y"}}
	}
	e := mustNew(t, Policy{
		Memberships: []MemberRole{{OrgID: org, UserID: u, Role: "admin"}, {OrgID: otherOrg, UserID: u, Role: "admin"}},
		TeamMembers: []TeamMember{{OrgID: org, TeamID: team, UserID: u}},
		Roles: []CustomRole{
			{ID: direct, OrgID: org, Rules: deny("org/direct"), Parents: []string{parent.String()}},
			{ID: parent, OrgID: org, Rules: deny("org/parent"), Parents: []string{"admin"}},
			{ID: viaTeam, OrgID: org, Rules: deny("org/team"), Parents: []string{teamParent.String()}},
			{ID: teamParent, OrgID: org, Rules: deny("org/teamparent")},
			{ID: foreign, OrgID: otherOrg, Rules: deny("org/foreign")},
		},
		Bindings: []Binding{
			{OrgID: org, Subject: UserSubject(u), RoleID: direct},
			{OrgID: org, Subject: TeamSubject(team), RoleID: viaTeam},
			{OrgID: otherOrg, Subject: UserSubject(u), RoleID: foreign},
		},
	})
	var got []string
	rules, err := e.DenyRulesFor(UserSubject(u), org)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range rules {
		if r.Effect != Deny {
			t.Errorf("non-deny rule returned: %+v", r)
		}
		got = append(got, r.Object)
	}
	slices.Sort(got)
	want := []string{"org/direct", "org/parent", "org/team", "org/teamparent"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
	if got, err := e.DenyRulesFor(UserSubject(uuid.New()), org); err != nil || len(got) != 0 {
		t.Errorf("stranger: got %v", got)
	}
}

// chainPolicy builds user→team→r1→…→rN with a deny on limit:set at rN, so the chain is n+1 hops long.
func chainPolicy(n int) (Policy, uuid.UUID, uuid.UUID) {
	org, u, team := uuid.New(), uuid.New(), uuid.New()
	ids := make([]uuid.UUID, n)
	for i := range ids {
		ids[i] = uuid.New()
	}
	p := Policy{
		Memberships: []MemberRole{{OrgID: org, UserID: u, Role: "admin"}},
		TeamMembers: []TeamMember{{OrgID: org, TeamID: team, UserID: u}},
		Bindings:    []Binding{{OrgID: org, Subject: TeamSubject(team), RoleID: ids[0]}},
	}
	for i, id := range ids {
		cr := CustomRole{ID: id, OrgID: org}
		if i+1 < n {
			cr.Parents = []string{ids[i+1].String()}
		} else {
			cr.Rules = []Rule{{Effect: Deny, Object: ObjLimits, Action: "limit:set"}}
		}
		p.Roles = append(p.Roles, cr)
	}
	return p, org, u
}

func TestRoleDepth(t *testing.T) {
	p, org, u := chainPolicy(MaxRoleDepth - 1)
	e := mustNew(t, p)
	if e.Allowed(UserSubject(u), org, ObjLimits, "limit:set") {
		t.Error("deny at the deep end of a chain at the limit should win")
	}
	if rules, err := e.DenyRulesFor(UserSubject(u), org); err != nil || len(rules) != 1 {
		t.Errorf("DenyRulesFor at the limit: %v, %v", rules, err)
	}
	p, _, _ = chainPolicy(MaxRoleDepth)
	if _, err := New(p); err == nil {
		t.Error("chain longer than MaxRoleDepth should fail")
	}
}

func TestNewRejectsInvalidPolicy(t *testing.T) {
	org, other, u, role, foreign := uuid.New(), uuid.New(), uuid.New(), uuid.New(), uuid.New()
	base := func() Policy {
		return Policy{
			Memberships: []MemberRole{{OrgID: org, UserID: u, Role: "member"}},
			Roles:       []CustomRole{{ID: role, OrgID: org}, {ID: foreign, OrgID: other}},
		}
	}
	cases := map[string]func(p *Policy){
		"unknown membership role": func(p *Policy) { p.Memberships[0].Role = "root" },
		"owner parent":            func(p *Policy) { p.Roles[0].Parents = []string{"owner"} },
		"foreign-org parent":      func(p *Policy) { p.Roles[0].Parents = []string{foreign.String()} },
		"unknown parent":          func(p *Policy) { p.Roles[0].Parents = []string{uuid.NewString()} },
		"non-canonical parent":    func(p *Policy) { p.Roles[0].Parents = []string{strings.ToUpper(foreign.String())} },
		"self cycle":              func(p *Policy) { p.Roles[0].Parents = []string{role.String()} },
		"binding to foreign role": func(p *Policy) {
			p.Bindings = []Binding{{OrgID: org, Subject: UserSubject(u), RoleID: foreign}}
		},
		"binding to unknown role": func(p *Policy) {
			p.Bindings = []Binding{{OrgID: org, Subject: UserSubject(u), RoleID: uuid.New()}}
		},
		"binding with role subject": func(p *Policy) {
			p.Bindings = []Binding{{OrgID: org, Subject: RoleSubject("admin"), RoleID: role}}
		},
		"binding with bad user id": func(p *Policy) {
			p.Bindings = []Binding{{OrgID: org, Subject: "user:x", RoleID: role}}
		},
	}
	if _, err := New(base()); err != nil {
		t.Fatalf("base policy: %v", err)
	}
	for name, mutate := range cases {
		p := base()
		mutate(&p)
		if _, err := New(p); err == nil {
			t.Errorf("%s: want error", name)
		}
	}
}

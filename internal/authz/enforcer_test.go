package authz

import (
	"errors"
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
	memberPerms := []Permission{{"member:read", ObjMembers}, {"team:read", ObjTeams}, {"team:leave", ObjTeams + "/*"},
		{"role:read", ObjRoles}, {"limit:read", ObjLimits}, {"env:list", ObjEnv}, {"env:use", ObjEnv},
		{"node:read", ObjNodes}, {"node:read", ObjNodes + "/*"}, {"node:use", ObjNodes + "/*"}, {"vm:create", ObjVMs},
		{"network:read", ObjNetworks}, {"network:create", ObjNetworks}, {"network:use", ObjNetworks + "/*"}, {"disk:create", ObjDisks}}
	// No built-in role reaches other members' private env scopes, connects to or forks VMs, or creates others' devices.
	notBuiltin := func(p Permission) bool {
		return p.Object == ObjUserEnv+"/*" || p.Action == "vm:connect" || p.Action == "vm:fork" || p.Action == "device:create" || p.Action == "disk:use"
	}
	want := map[uuid.UUID]func(p Permission) bool{
		owner:  func(p Permission) bool { return !notBuiltin(p) },
		admin:  func(p Permission) bool { return p.Action != "member:set-role" && !notBuiltin(p) },
		member: func(p Permission) bool { return slices.Contains(memberPerms, p) },
	}
	names := map[uuid.UUID]string{owner: "owner", admin: "admin", member: "member"}
	for u, allowed := range want {
		for _, p := range Catalog {
			if got := e.Allowed(UserSubject(u), org, concrete(p.Object), p.Action); got != allowed(p) {
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
		ObjNodes + "/*":                      true,
		ObjVMs + "/*":                        true,
		TeamVMsObject(uuid.New()):            true,
		ObjTeamVMs + "/*":                    true,
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
	if got := ExpandActions("*"); len(got) != len(Catalog)-11 {
		t.Errorf("*: got %d actions, want %d", len(got), len(Catalog)-11)
	}
	want := []string{"team:read", "team:create", "team:update", "team:delete", "team:manage-members", "team:leave"}
	if got := ExpandActions("team:*"); !reflect.DeepEqual(got, want) {
		t.Errorf("team:*: got %v", got)
	}
	if got := ExpandActions("audit:read"); !reflect.DeepEqual(got, []string{"audit:read"}) {
		t.Errorf("exact: got %v", got)
	}
	if got := ExpandActions("vm:*"); !reflect.DeepEqual(got, []string{"vm:create", "vm:read", "vm:power", "vm:delete", "vm:connect", "vm:network", "vm:disks", "vm:snapshot", "vm:restore", "vm:fork"}) {
		t.Errorf("vm:*: got %v", got)
	}
	if got := ExpandActions("vm:ssh"); len(got) != 0 {
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

func TestEnvPermissions(t *testing.T) {
	org, otherOrg := uuid.New(), uuid.New()
	owner, admin, member, teamMember, teamAdmin, denied := uuid.New(), uuid.New(), uuid.New(), uuid.New(), uuid.New(), uuid.New()
	team, otherTeam, emptyTeam, denyRole := uuid.New(), uuid.New(), uuid.New(), uuid.New()
	e := mustNew(t, Policy{
		Memberships: []MemberRole{
			{OrgID: org, UserID: owner, Role: "owner"},
			{OrgID: org, UserID: admin, Role: "admin"},
			{OrgID: org, UserID: member, Role: "member"},
			{OrgID: org, UserID: teamMember, Role: "member"},
			{OrgID: org, UserID: teamAdmin, Role: "member"},
			{OrgID: org, UserID: denied, Role: "member"},
		},
		TeamMembers: []TeamMember{
			{OrgID: org, TeamID: team, UserID: teamMember},
			{OrgID: org, TeamID: team, UserID: teamAdmin, Admin: true},
		},
		Teams: []TeamRef{{OrgID: org, ID: team}, {OrgID: org, ID: otherTeam}, {OrgID: org, ID: emptyTeam}},
		Roles: []CustomRole{{ID: denyRole, OrgID: org, Name: "no-env", Rules: []Rule{
			{Effect: Deny, Object: ObjEnv, Action: "env:use"},
			{Effect: Deny, Object: ObjUserEnv + "/*", Action: "env:set"},
		}}},
		Bindings: []Binding{{OrgID: org, Subject: UserSubject(denied), RoleID: denyRole}},
	})
	user := func(id uuid.UUID) string { return UserSubject(id) }
	tests := []struct {
		name     string
		sub      string
		org      uuid.UUID
		obj, act string
		want     bool
	}{
		{"member lists org env", user(member), org, ObjEnv, "env:list", true},
		{"member uses org env", user(member), org, ObjEnv, "env:use", true},
		{"member sets org env", user(member), org, ObjEnv, "env:set", false},
		{"admin sets org env", user(admin), org, ObjEnv, "env:set", true},
		{"admin deletes org env", user(admin), org, ObjEnv, "env:delete", true},
		{"admin sets any team env", user(admin), org, TeamEnvObject(otherTeam), "env:set", true},
		{"admin deletes any team env", user(admin), org, TeamEnvObject(otherTeam), "env:delete", true},
		{"admin sets other user env", user(admin), org, UserEnvObject(member), "env:set", false},
		{"owner sets org env", user(owner), org, ObjEnv, "env:set", true},
		{"owner sets any team env", user(owner), org, TeamEnvObject(team), "env:set", true},
		{"owner lists other user env", user(owner), org, UserEnvObject(member), "env:list", false},
		{"member lists own user env", user(member), org, UserEnvObject(member), "env:list", true},
		{"member sets own user env", user(member), org, UserEnvObject(member), "env:set", true},
		{"member deletes own user env", user(member), org, UserEnvObject(member), "env:delete", true},
		{"member uses own user env", user(member), org, UserEnvObject(member), "env:use", true},
		{"member lists other user env", user(member), org, UserEnvObject(admin), "env:list", false},
		{"member uses other user env", user(member), org, UserEnvObject(admin), "env:use", false},
		{"team member lists own team env", user(teamMember), org, TeamEnvObject(team), "env:list", true},
		{"team member uses own team env", user(teamMember), org, TeamEnvObject(team), "env:use", true},
		{"team member lists other team env", user(teamMember), org, TeamEnvObject(otherTeam), "env:list", false},
		{"team member sets own team env", user(teamMember), org, TeamEnvObject(team), "env:set", false},
		{"team admin sets own team env", user(teamAdmin), org, TeamEnvObject(team), "env:set", true},
		{"team admin deletes own team env", user(teamAdmin), org, TeamEnvObject(team), "env:delete", true},
		{"team admin sets other team env", user(teamAdmin), org, TeamEnvObject(otherTeam), "env:set", false},
		{"empty team uses org env", TeamSubject(emptyTeam), org, ObjEnv, "env:use", true},
		{"empty team uses own team env", TeamSubject(emptyTeam), org, TeamEnvObject(emptyTeam), "env:use", true},
		{"empty team uses other team env", TeamSubject(emptyTeam), org, TeamEnvObject(otherTeam), "env:use", false},
		{"empty team sets own team env", TeamSubject(emptyTeam), org, TeamEnvObject(emptyTeam), "env:set", false},
		{"deny env:use on org env", user(denied), org, ObjEnv, "env:use", false},
		{"deny keeps env:list", user(denied), org, ObjEnv, "env:list", true},
		{"deny env:set on own user env", user(denied), org, UserEnvObject(denied), "env:set", false},
		{"cross-org org env", user(admin), otherOrg, ObjEnv, "env:use", false},
		{"cross-org own user env", user(member), otherOrg, UserEnvObject(member), "env:list", false},
		{"cross-org team env", user(teamAdmin), otherOrg, TeamEnvObject(team), "env:set", false},
		{"cross-org team subject", TeamSubject(team), otherOrg, TeamEnvObject(team), "env:use", false},
	}
	for _, tt := range tests {
		if got := e.Allowed(tt.sub, tt.org, tt.obj, tt.act); got != tt.want {
			t.Errorf("%s: got %v", tt.name, got)
		}
	}
	if got := ExpandActions("env:*"); !reflect.DeepEqual(got, []string{"env:list", "env:set", "env:delete", "env:use"}) {
		t.Errorf("env:*: got %v", got)
	}
}

func TestVMPermissions(t *testing.T) {
	org, otherOrg := uuid.New(), uuid.New()
	owner, admin, member, teamMember, teamAdmin, otherAdmin, denied := uuid.New(), uuid.New(), uuid.New(), uuid.New(), uuid.New(), uuid.New(), uuid.New()
	team, otherTeam, denyRole := uuid.New(), uuid.New(), uuid.New()
	ownVM, adminVM, teamVM, otherTeamVM, deniedVM, foreignVM := uuid.New(), uuid.New(), uuid.New(), uuid.New(), uuid.New(), uuid.New()
	e := mustNew(t, Policy{
		Memberships: []MemberRole{
			{OrgID: org, UserID: owner, Role: "owner"},
			{OrgID: org, UserID: admin, Role: "admin"},
			{OrgID: org, UserID: member, Role: "member"},
			{OrgID: org, UserID: teamMember, Role: "member"},
			{OrgID: org, UserID: teamAdmin, Role: "member"},
			{OrgID: org, UserID: otherAdmin, Role: "member"},
			{OrgID: org, UserID: denied, Role: "owner"},
			{OrgID: otherOrg, UserID: member, Role: "member"},
		},
		TeamMembers: []TeamMember{
			{OrgID: org, TeamID: team, UserID: teamMember},
			{OrgID: org, TeamID: team, UserID: teamAdmin, Admin: true},
			{OrgID: org, TeamID: otherTeam, UserID: otherAdmin, Admin: true},
		},
		Teams: []TeamRef{{OrgID: org, ID: team}, {OrgID: org, ID: otherTeam}},
		VMs: []VMRef{
			{OrgID: org, ID: ownVM, UserID: &member},
			{OrgID: org, ID: adminVM, UserID: &admin},
			{OrgID: org, ID: teamVM, TeamID: &team},
			{OrgID: org, ID: otherTeamVM, TeamID: &otherTeam},
			{OrgID: org, ID: deniedVM, UserID: &denied},
			{OrgID: otherOrg, ID: foreignVM, UserID: &member},
		},
		Roles: []CustomRole{{ID: denyRole, OrgID: org, Name: "no-vm-delete", Rules: []Rule{
			{Effect: Deny, Object: ObjVMs + "/*", Action: "vm:delete"},
		}}},
		Bindings: []Binding{{OrgID: org, Subject: UserSubject(denied), RoleID: denyRole}},
	})
	user := func(id uuid.UUID) string { return UserSubject(id) }
	node := NodeObject(uuid.New())
	tests := []struct {
		name     string
		sub      string
		org      uuid.UUID
		obj, act string
		want     bool
	}{
		// see nodes, place VMs on them
		{"member lists nodes", user(member), org, ObjNodes, "node:read", true},
		{"member reads node", user(member), org, node, "node:read", true},
		{"member uses node", user(member), org, node, "node:use", true},
		{"team admin uses node", user(teamAdmin), org, node, "node:use", true},
		{"admin uses node", user(admin), org, node, "node:use", true},
		{"owner uses node", user(owner), org, node, "node:use", true},
		// add, rename, delete nodes
		{"member creates node", user(member), org, ObjNodes, "node:create", false},
		{"member updates node", user(member), org, node, "node:update", false},
		{"member deletes node", user(member), org, node, "node:delete", false},
		{"team admin creates node", user(teamAdmin), org, ObjNodes, "node:create", false},
		{"team admin deletes node", user(teamAdmin), org, node, "node:delete", false},
		{"admin creates node", user(admin), org, ObjNodes, "node:create", true},
		{"admin updates node", user(admin), org, node, "node:update", true},
		{"admin deletes node", user(admin), org, node, "node:delete", true},
		{"owner creates node", user(owner), org, ObjNodes, "node:create", true},
		{"owner deletes node", user(owner), org, node, "node:delete", true},
		// create own VMs
		{"member creates own vm", user(member), org, ObjVMs, "vm:create", true},
		{"team admin creates own vm", user(teamAdmin), org, ObjVMs, "vm:create", true},
		{"admin creates own vm", user(admin), org, ObjVMs, "vm:create", true},
		{"owner creates own vm", user(owner), org, ObjVMs, "vm:create", true},
		// create team VMs
		{"member creates team vm", user(member), org, TeamVMsObject(team), "vm:create", false},
		{"team member creates team vm", user(teamMember), org, TeamVMsObject(team), "vm:create", false},
		{"team admin creates team vm", user(teamAdmin), org, TeamVMsObject(team), "vm:create", true},
		{"team admin creates other team vm", user(teamAdmin), org, TeamVMsObject(otherTeam), "vm:create", false},
		{"admin creates any team vm", user(admin), org, TeamVMsObject(otherTeam), "vm:create", true},
		{"owner creates any team vm", user(owner), org, TeamVMsObject(team), "vm:create", true},
		// own VMs: all actions
		{"member reads own vm", user(member), org, VMObject(ownVM), "vm:read", true},
		{"member powers own vm", user(member), org, VMObject(ownVM), "vm:power", true},
		{"member deletes own vm", user(member), org, VMObject(ownVM), "vm:delete", true},
		{"admin deletes own vm", user(admin), org, VMObject(adminVM), "vm:delete", true},
		{"member changes own vm networks", user(member), org, VMObject(ownVM), "vm:network", true},
		{"member connects own vm", user(member), org, VMObject(ownVM), "vm:connect", true},
		{"team member connects team vm", user(teamMember), org, VMObject(teamVM), "vm:connect", true},
		{"team member changes team vm networks", user(teamMember), org, VMObject(teamVM), "vm:network", false},
		{"team admin changes team vm networks", user(teamAdmin), org, VMObject(teamVM), "vm:network", true},
		{"other team admin connects team vm", user(otherAdmin), org, VMObject(teamVM), "vm:connect", false},
		// team VMs: read, power
		{"team member reads team vm", user(teamMember), org, VMObject(teamVM), "vm:read", true},
		{"team member powers team vm", user(teamMember), org, VMObject(teamVM), "vm:power", true},
		{"team subject powers team vm", TeamSubject(team), org, VMObject(teamVM), "vm:power", true},
		{"team admin reads team vm", user(teamAdmin), org, VMObject(teamVM), "vm:read", true},
		{"team admin powers team vm", user(teamAdmin), org, VMObject(teamVM), "vm:power", true},
		{"non-member reads team vm", user(member), org, VMObject(teamVM), "vm:read", false},
		{"team member reads other team vm", user(teamMember), org, VMObject(otherTeamVM), "vm:read", false},
		// team VMs: delete
		{"team member deletes team vm", user(teamMember), org, VMObject(teamVM), "vm:delete", false},
		{"team admin deletes team vm", user(teamAdmin), org, VMObject(teamVM), "vm:delete", true},
		{"team admin deletes other team vm", user(teamAdmin), org, VMObject(otherTeamVM), "vm:delete", false},
		{"other team admin deletes team vm", user(otherAdmin), org, VMObject(teamVM), "vm:delete", false},
		{"admin deletes team vm", user(admin), org, VMObject(teamVM), "vm:delete", true},
		{"owner deletes team vm", user(owner), org, VMObject(teamVM), "vm:delete", true},
		// see, power and delete every VM in the org
		{"member reads other's vm", user(teamMember), org, VMObject(ownVM), "vm:read", false},
		{"member powers other's vm", user(teamMember), org, VMObject(ownVM), "vm:power", false},
		{"member deletes other's vm", user(member), org, VMObject(adminVM), "vm:delete", false},
		{"team admin deletes other's vm", user(teamAdmin), org, VMObject(ownVM), "vm:delete", false},
		{"admin reads other's vm", user(admin), org, VMObject(ownVM), "vm:read", true},
		{"admin powers other's vm", user(admin), org, VMObject(ownVM), "vm:power", true},
		{"admin deletes other's vm", user(admin), org, VMObject(ownVM), "vm:delete", true},
		{"owner reads other's vm", user(owner), org, VMObject(ownVM), "vm:read", true},
		{"owner powers other's vm", user(owner), org, VMObject(ownVM), "vm:power", true},
		{"owner deletes other's vm", user(owner), org, VMObject(ownVM), "vm:delete", true},
		// deny wins over the generated owner rule
		{"deny vm:delete blocks owner of vm", user(denied), org, VMObject(deniedVM), "vm:delete", false},
		{"deny keeps vm:power", user(denied), org, VMObject(deniedVM), "vm:power", true},
		{"deny blocks owner role on other vm", user(denied), org, VMObject(ownVM), "vm:delete", false},
		// cross-org
		{"cross-org own vm", user(member), otherOrg, VMObject(ownVM), "vm:delete", false},
		{"cross-org team vm", user(teamAdmin), otherOrg, VMObject(teamVM), "vm:delete", false},
		{"cross-org team subject", TeamSubject(team), otherOrg, VMObject(teamVM), "vm:read", false},
		{"cross-org team vm create", user(teamAdmin), otherOrg, TeamVMsObject(team), "vm:create", false},
		{"cross-org admin node", user(admin), otherOrg, ObjNodes, "node:create", false},
		{"cross-org foreign vm in A", user(member), org, VMObject(foreignVM), "vm:delete", false},
		{"foreign vm in its org", user(member), otherOrg, VMObject(foreignVM), "vm:delete", true},
	}
	for _, tt := range tests {
		if got := e.Allowed(tt.sub, tt.org, tt.obj, tt.act); got != tt.want {
			t.Errorf("%s: got %v", tt.name, got)
		}
	}
}

// vmFixture is an org with an owner, an admin, two members (a owns userVM), a team with member tm and teamVM.
type vmFixture struct {
	org, owner, admin, a, b, tm, team, userVM, teamVM uuid.UUID
}

func newVMFixture() vmFixture {
	return vmFixture{uuid.New(), uuid.New(), uuid.New(), uuid.New(), uuid.New(), uuid.New(), uuid.New(), uuid.New(), uuid.New()}
}

func (f vmFixture) policy() Policy {
	return Policy{
		Memberships: []MemberRole{
			{OrgID: f.org, UserID: f.owner, Role: "owner"},
			{OrgID: f.org, UserID: f.admin, Role: "admin"},
			{OrgID: f.org, UserID: f.a, Role: "member"},
			{OrgID: f.org, UserID: f.b, Role: "member"},
			{OrgID: f.org, UserID: f.tm, Role: "member"},
		},
		TeamMembers: []TeamMember{{OrgID: f.org, TeamID: f.team, UserID: f.tm}},
		Teams:       []TeamRef{{OrgID: f.org, ID: f.team}},
		VMs:         []VMRef{f.userRef(), f.teamRef()},
	}
}

func (f vmFixture) userRef() VMRef { return VMRef{OrgID: f.org, ID: f.userVM, UserID: &f.a} }
func (f vmFixture) teamRef() VMRef { return VMRef{OrgID: f.org, ID: f.teamVM, TeamID: &f.team} }

func TestAdminHasNoConnect(t *testing.T) {
	f := newVMFixture()
	e := mustNew(t, f.policy())
	for _, u := range []uuid.UUID{f.owner, f.admin} {
		for _, vm := range []uuid.UUID{f.userVM, f.teamVM} {
			if e.Allowed(UserSubject(u), f.org, VMObject(vm), "vm:connect") {
				t.Errorf("%s holds vm:connect on %s", u, vm)
			}
			for _, act := range []string{"vm:power", "vm:network"} {
				if !e.Allowed(UserSubject(u), f.org, VMObject(vm), act) {
					t.Errorf("%s lacks %s on %s", u, act, vm)
				}
			}
		}
	}
}

func TestNoWildcardVMGrants(t *testing.T) {
	f := newVMFixture()
	teamAdmin := uuid.New()
	p := f.policy()
	p.Memberships = append(p.Memberships, MemberRole{OrgID: f.org, UserID: teamAdmin, Role: "member"})
	p.TeamMembers = append(p.TeamMembers, TeamMember{OrgID: f.org, TeamID: f.team, UserID: teamAdmin, Admin: true})
	e := mustNew(t, p)
	for _, c := range []struct{ sub, vm uuid.UUID }{{f.a, f.userVM}, {f.tm, f.teamVM}, {teamAdmin, f.teamVM}, {f.admin, f.userVM}, {f.owner, f.teamVM}} {
		if e.Allowed(UserSubject(c.sub), f.org, VMObject(c.vm), "vm:share") {
			t.Errorf("%s: vm:share allowed on %s", c.sub, c.vm)
		}
	}
	if e.Allowed(UserSubject(f.admin), f.org, TeamVMsObject(f.team), "vm:share") {
		t.Error("admin: vm:share on team vms allowed")
	}
}

func TestCanConnect(t *testing.T) {
	f := newVMFixture()
	role := uuid.New()
	p := f.policy()
	p.Roles = []CustomRole{{ID: role, OrgID: f.org, Name: "connect-a", Rules: []Rule{{Allow, VMObject(f.userVM), "vm:connect"}}}}
	p.Bindings = []Binding{{OrgID: f.org, Subject: UserSubject(f.b), RoleID: role}}
	e := mustNew(t, p)
	tests := []struct {
		name string
		user uuid.UUID
		vm   VMRef
		want bool
	}{
		{"owner", f.a, f.userRef(), true},
		{"other member on user vm", f.b, f.userRef(), false},
		{"admin on user vm", f.admin, f.userRef(), false},
		{"org owner on user vm", f.owner, f.userRef(), false},
		{"team member", f.tm, f.teamRef(), true},
		{"non-member on team vm", f.a, f.teamRef(), false},
		{"admin on team vm", f.admin, f.teamRef(), false},
	}
	for _, tt := range tests {
		if got := e.CanConnect(tt.user, tt.vm); got != tt.want {
			t.Errorf("%s: got %v", tt.name, got)
		}
	}
	if !e.Allowed(UserSubject(f.b), f.org, VMObject(f.userVM), "vm:connect") {
		t.Error("custom grant should show in Allowed, only CanConnect ignores it")
	}
}

func TestDenyConnectBlocksOwner(t *testing.T) {
	f := newVMFixture()
	role := uuid.New()
	p := f.policy()
	p.Roles = []CustomRole{{ID: role, OrgID: f.org, Name: "no-connect", Rules: []Rule{{Deny, ObjVMs + "/*", "vm:connect"}}}}
	p.Bindings = []Binding{{OrgID: f.org, Subject: UserSubject(f.a), RoleID: role}, {OrgID: f.org, Subject: TeamSubject(f.team), RoleID: role}}
	e := mustNew(t, p)
	if e.CanConnect(f.a, f.userRef()) {
		t.Error("deny should block the owner")
	}
	if e.CanConnect(f.tm, f.teamRef()) {
		t.Error("deny should block the team member")
	}
}

func TestCustomConnectGrantEscalates(t *testing.T) {
	f := newVMFixture()
	e := mustNew(t, f.policy())
	for _, u := range []uuid.UUID{f.admin, f.owner} {
		if err := e.CanGrant(UserSubject(u), f.org, true, []Rule{{Allow, ObjVMs + "/*", "vm:connect"}}); !errors.Is(err, ErrEscalation) {
			t.Errorf("%s: got %v, want ErrEscalation", u, err)
		}
	}
}

func TestDevicePermissions(t *testing.T) {
	f := newVMFixture()
	e := mustNew(t, f.policy())
	tests := []struct {
		name    string
		sub, of uuid.UUID
		act     string
		want    bool
	}{
		{"member creates own", f.a, f.a, "device:create", true},
		{"member lists own", f.a, f.a, "device:list", true},
		{"member deletes own", f.a, f.a, "device:delete", true},
		{"member creates other's", f.a, f.b, "device:create", false},
		{"member lists other's", f.a, f.b, "device:list", false},
		{"member deletes other's", f.a, f.b, "device:delete", false},
		{"admin lists other's", f.admin, f.a, "device:list", true},
		{"admin deletes other's", f.admin, f.a, "device:delete", true},
		{"admin creates other's", f.admin, f.a, "device:create", false},
		{"owner creates other's", f.owner, f.a, "device:create", false},
		{"admin creates own", f.admin, f.admin, "device:create", true},
	}
	for _, tt := range tests {
		if got := e.Allowed(UserSubject(tt.sub), f.org, UserDevicesObject(tt.of), tt.act); got != tt.want {
			t.Errorf("%s: got %v", tt.name, got)
		}
	}
	if e.Allowed(UserSubject(f.a), uuid.New(), UserDevicesObject(f.a), "device:create") {
		t.Error("cross-org device:create allowed")
	}
}

func TestNetworkPermissions(t *testing.T) {
	f := newVMFixture()
	e := mustNew(t, f.policy())
	net := NetworkObject(uuid.New())
	tests := []struct {
		name     string
		sub      uuid.UUID
		obj, act string
		want     bool
	}{
		{"member reads", f.a, ObjNetworks, "network:read", true},
		{"member creates", f.a, ObjNetworks, "network:create", true},
		{"member uses", f.a, net, "network:use", true},
		{"member deletes", f.a, net, "network:delete", false},
		{"admin deletes", f.admin, net, "network:delete", true},
		{"owner deletes", f.owner, net, "network:delete", true},
	}
	for _, tt := range tests {
		if got := e.Allowed(UserSubject(tt.sub), f.org, tt.obj, tt.act); got != tt.want {
			t.Errorf("%s: got %v", tt.name, got)
		}
	}
}

type diskFixture struct {
	org, owner, admin, member, colleague, teamMember, teamAdmin, team uuid.UUID
	memberDisk, teamDisk, denyDisk                                    uuid.UUID
	vm                                                                uuid.UUID
	e                                                                 *Enforcer
}

func newDiskFixture(t *testing.T) diskFixture {
	f := diskFixture{org: uuid.New(), owner: uuid.New(), admin: uuid.New(), member: uuid.New(), colleague: uuid.New(),
		teamMember: uuid.New(), teamAdmin: uuid.New(), team: uuid.New(),
		memberDisk: uuid.New(), teamDisk: uuid.New(), denyDisk: uuid.New(), vm: uuid.New()}
	denyRole := uuid.New()
	f.e = mustNew(t, Policy{
		Memberships: []MemberRole{
			{OrgID: f.org, UserID: f.owner, Role: "owner"},
			{OrgID: f.org, UserID: f.admin, Role: "admin"},
			{OrgID: f.org, UserID: f.member, Role: "member"},
			{OrgID: f.org, UserID: f.colleague, Role: "member"},
			{OrgID: f.org, UserID: f.teamMember, Role: "member"},
			{OrgID: f.org, UserID: f.teamAdmin, Role: "member"},
		},
		TeamMembers: []TeamMember{
			{OrgID: f.org, TeamID: f.team, UserID: f.teamMember},
			{OrgID: f.org, TeamID: f.team, UserID: f.teamAdmin, Admin: true},
		},
		Teams: []TeamRef{{OrgID: f.org, ID: f.team}},
		VMs:   []VMRef{{OrgID: f.org, ID: f.vm, UserID: &f.colleague}},
		Disks: []DiskRef{
			{OrgID: f.org, ID: f.memberDisk, UserID: &f.member},
			{OrgID: f.org, ID: f.teamDisk, TeamID: &f.team},
			{OrgID: f.org, ID: f.denyDisk, UserID: &f.colleague},
		},
		Roles: []CustomRole{{ID: denyRole, OrgID: f.org, Name: "no-disk-delete", Rules: []Rule{
			{Effect: Deny, Object: ObjDisks + "/*", Action: "disk:delete"},
		}}},
		Bindings: []Binding{{OrgID: f.org, Subject: UserSubject(f.colleague), RoleID: denyRole}},
	})
	return f
}

func TestDiskPermissions(t *testing.T) {
	f := newDiskFixture(t)
	tests := []struct {
		name     string
		sub      uuid.UUID
		obj, act string
		want     bool
	}{
		{"member reads own disk", f.member, DiskObject(f.memberDisk), "disk:read", true},
		{"member updates own disk", f.member, DiskObject(f.memberDisk), "disk:update", true},
		{"member deletes own disk", f.member, DiskObject(f.memberDisk), "disk:delete", true},
		{"member uses own disk", f.member, DiskObject(f.memberDisk), "disk:use", true},
		{"colleague reads disk", f.colleague, DiskObject(f.memberDisk), "disk:read", false},
		{"colleague updates disk", f.colleague, DiskObject(f.memberDisk), "disk:update", false},
		{"colleague deletes disk", f.colleague, DiskObject(f.memberDisk), "disk:delete", false},
		{"colleague uses disk", f.colleague, DiskObject(f.memberDisk), "disk:use", false},
		{"member creates disk", f.member, ObjDisks, "disk:create", true},
		{"member creates team disk", f.member, TeamDisksObject(f.team), "disk:create", false},
		{"team member reads team disk", f.teamMember, DiskObject(f.teamDisk), "disk:read", true},
		{"team member uses team disk", f.teamMember, DiskObject(f.teamDisk), "disk:use", true},
		{"team member deletes team disk", f.teamMember, DiskObject(f.teamDisk), "disk:delete", false},
		{"team member updates team disk", f.teamMember, DiskObject(f.teamDisk), "disk:update", false},
		{"team member creates team disk", f.teamMember, TeamDisksObject(f.team), "disk:create", false},
		{"team admin reads team disk", f.teamAdmin, DiskObject(f.teamDisk), "disk:read", true},
		{"team admin updates team disk", f.teamAdmin, DiskObject(f.teamDisk), "disk:update", true},
		{"team admin deletes team disk", f.teamAdmin, DiskObject(f.teamDisk), "disk:delete", true},
		{"team admin uses team disk", f.teamAdmin, DiskObject(f.teamDisk), "disk:use", true},
		{"team admin creates team disk", f.teamAdmin, TeamDisksObject(f.team), "disk:create", true},
		{"team admin creates other team disk", f.teamAdmin, TeamDisksObject(uuid.New()), "disk:create", false},
		{"team admin reads member disk", f.teamAdmin, DiskObject(f.memberDisk), "disk:read", false},
		{"member reads team disk", f.member, DiskObject(f.teamDisk), "disk:read", false},
		{"admin creates team disk", f.admin, TeamDisksObject(f.team), "disk:create", true},
		{"member changes own vm disks", f.colleague, VMObject(f.vm), "vm:disks", true},
		{"member changes other vm disks", f.member, VMObject(f.vm), "vm:disks", false},
	}
	for _, tc := range tests {
		if got := f.e.Allowed(UserSubject(tc.sub), f.org, tc.obj, tc.act); got != tc.want {
			t.Errorf("%s: got %v", tc.name, got)
		}
	}
}

func TestAdminsNeverDiskUse(t *testing.T) {
	f := newDiskFixture(t)
	for name, u := range map[string]uuid.UUID{"admin": f.admin, "owner": f.owner} {
		for act, want := range map[string]bool{"disk:read": true, "disk:update": true, "disk:delete": true, "disk:use": false} {
			for _, d := range []uuid.UUID{f.memberDisk, f.teamDisk} {
				if got := f.e.Allowed(UserSubject(u), f.org, DiskObject(d), act); got != want {
					t.Errorf("%s %s: got %v", name, act, got)
				}
			}
		}
		if !f.e.Allowed(UserSubject(u), f.org, VMObject(f.vm), "vm:disks") {
			t.Errorf("%s: vm:disks denied", name)
		}
	}
}

func TestDenyDiskDeleteCoversUserDisks(t *testing.T) {
	f := newDiskFixture(t)
	if f.e.Allowed(UserSubject(f.colleague), f.org, DiskObject(f.denyDisk), "disk:delete") {
		t.Error("owner deleted own disk despite deny")
	}
	if !f.e.Allowed(UserSubject(f.colleague), f.org, DiskObject(f.denyDisk), "disk:use") {
		t.Error("owner lost disk:use")
	}
}

func TestNoDiskWildcardInBuiltins(t *testing.T) {
	f := newDiskFixture(t)
	for _, key := range []string{"owner", "admin", "member"} {
		for _, r := range BuiltinRules(key) {
			if strings.HasPrefix(r.Object, "org/disks") && (r.Action == "*" || r.Action == "disk:*") {
				t.Errorf("%s: wildcard %s@%s", key, r.Action, r.Object)
			}
		}
	}
	policy, err := f.e.ce.GetPolicy()
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range policy {
		if strings.HasPrefix(p[2], "org/disks") && p[4] == string(Allow) && (p[3] == "*" || p[3] == "disk:*") {
			t.Errorf("generated wildcard rule %v", p)
		}
	}
}

func TestEnforcerVMLifecycleGrants(t *testing.T) {
	f := newVMFixture()
	teamAdmin := uuid.New()
	p := f.policy()
	p.Memberships = append(p.Memberships, MemberRole{OrgID: f.org, UserID: teamAdmin, Role: "member"})
	p.TeamMembers = append(p.TeamMembers, TeamMember{OrgID: f.org, TeamID: f.team, UserID: teamAdmin, Admin: true})
	e := mustNew(t, p)
	for _, c := range []struct {
		name string
		sub  uuid.UUID
		vm   uuid.UUID
		want []string
	}{
		{"vm owner", f.a, f.userVM, []string{"vm:snapshot", "vm:restore", "vm:fork"}},
		{"team admin", teamAdmin, f.teamVM, []string{"vm:snapshot", "vm:restore", "vm:fork"}},
		{"team member", f.tm, f.teamVM, []string{"vm:snapshot"}},
		{"admin", f.admin, f.userVM, []string{"vm:snapshot", "vm:restore"}},
		{"org owner", f.owner, f.teamVM, []string{"vm:snapshot", "vm:restore"}},
		{"other member", f.b, f.userVM, nil},
	} {
		for _, act := range []string{"vm:snapshot", "vm:restore", "vm:fork"} {
			if got := e.Allowed(UserSubject(c.sub), f.org, VMObject(c.vm), act); got != slices.Contains(c.want, act) {
				t.Errorf("%s %s: got %v", c.name, act, got)
			}
		}
	}
	for _, key := range []string{"member", "admin", "owner"} {
		for _, r := range BuiltinRules(key) {
			if r.Action == "vm:*" || r.Action == "vm:fork" {
				t.Errorf("%s has %s", key, r.Action)
			}
		}
	}
	for _, act := range []string{"vm:snapshot", "vm:restore", "vm:fork"} {
		if !slices.Contains(ExpandActions("vm:*"), act) {
			t.Errorf("catalog lacks %s", act)
		}
	}
}

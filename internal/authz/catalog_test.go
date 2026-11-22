package authz

import (
	"slices"
	"testing"

	"github.com/google/uuid"
)

func computeCatalog(actions ...string) Catalog {
	c := Catalog{Service: "hub"}
	for _, a := range actions {
		c.Actions = append(c.Actions, Action{Name: a, Object: ObjVMs + "/*", Write: true})
	}
	return c
}

func TestBuiltinInheritance(t *testing.T) {
	org, owner, admin, member := uuid.New(), uuid.New(), uuid.New(), uuid.New()
	c := computeCatalog("compute.instances.get", "compute.instances.delete", "compute.instances.fork")
	c.RoleDefaults = []RoleDefault{
		{Role: "member", Object: ObjVMs + "/*", Action: "compute.instances.get"},
		{Role: "admin", Object: ObjVMs + "/*", Action: "compute.instances.delete"},
		{Role: "owner", Object: ObjVMs + "/*", Action: "compute.instances.fork"},
	}
	e := mustNew(t, Policy{Catalogs: []Catalog{c}, Memberships: []MemberRole{
		{OrgID: org, UserID: owner, Role: "owner"}, {OrgID: org, UserID: admin, Role: "admin"}, {OrgID: org, UserID: member, Role: "member"},
	}})
	vm := VMObject(uuid.New())
	for _, tc := range []struct {
		user uuid.UUID
		act  string
		want bool
	}{
		{member, "compute.instances.get", true}, {member, "compute.instances.delete", false}, {member, "compute.instances.fork", false},
		{admin, "compute.instances.get", true}, {admin, "compute.instances.delete", true}, {admin, "compute.instances.fork", false},
		{owner, "compute.instances.get", true}, {owner, "compute.instances.delete", true}, {owner, "compute.instances.fork", true},
	} {
		if got := e.Allowed(UserSubject(tc.user), org, vm, tc.act); got != tc.want {
			t.Errorf("%s %s: got %v", tc.user, tc.act, got)
		}
	}
	if got := e.BuiltinRules("admin"); len(got) != 2 || got[0].Action != "compute.instances.get" || got[1].Action != "compute.instances.delete" {
		t.Errorf("admin rules = %v, want member's then admin's", got)
	}
	if e.BuiltinRules("superuser") != nil {
		t.Error("unknown key must have no rules")
	}
	if _, err := New(Policy{Memberships: []MemberRole{{OrgID: org, UserID: owner, Role: "superuser"}}}); err == nil {
		t.Error("membership role outside owner/admin/member accepted")
	}
}

func TestUnknownAllowNeverGrants(t *testing.T) {
	org, u, role := uuid.New(), uuid.New(), uuid.New()
	vm := VMObject(uuid.New())
	e := mustNew(t, Policy{
		Memberships: []MemberRole{{OrgID: org, UserID: u, Role: "member"}},
		Roles: []CustomRole{{ID: role, OrgID: org, Name: "r", Rules: []Rule{
			{Allow, ObjVMs + "/*", "compute.instances.reboot"},
			{Allow, ObjVMs + "/*", "compute.instances.get"},
		}}},
		Bindings: []Binding{{OrgID: org, Subject: UserSubject(u), RoleID: role}},
	})
	if e.Allowed(UserSubject(u), org, vm, "compute.instances.reboot") {
		t.Error("an allow for an unregistered action granted it")
	}
	if !e.Allowed(UserSubject(u), org, vm, "compute.instances.get") {
		t.Error("the known allow of the same role was dropped")
	}
	if got := e.UnknownActions(org); !slices.Equal(got, []string{"compute.instances.reboot"}) {
		t.Errorf("UnknownActions = %v", got)
	}
	if got := e.UnknownActions(uuid.New()); len(got) != 0 {
		t.Errorf("UnknownActions of another org = %v", got)
	}
}

func TestUnknownDenyStillDenies(t *testing.T) {
	org, u, role := uuid.New(), uuid.New(), uuid.New()
	vm := VMObject(uuid.New())
	p := Policy{
		Memberships: []MemberRole{{OrgID: org, UserID: u, Role: "owner"}},
		Roles:       []CustomRole{{ID: role, OrgID: org, Name: "r", Rules: []Rule{{Deny, ObjVMs + "/*", "compute.instances.*"}}}},
		Bindings:    []Binding{{OrgID: org, Subject: UserSubject(u), RoleID: role}},
	}
	later := computeCatalog("compute.instances.get", "compute.instances.reboot")
	later.RoleDefaults = []RoleDefault{{Role: "member", Object: ObjVMs + "/*", Action: "compute.instances.reboot"}}
	p.Catalogs = []Catalog{later}
	e := mustNew(t, p)
	if e.Allowed(UserSubject(u), org, vm, "compute.instances.reboot") {
		t.Error("the deny wildcard must cut an action a catalog added later")
	}
	if got := e.UnknownActions(org); len(got) != 0 {
		t.Errorf("a deny wildcard matching known actions is not unknown: %v", got)
	}
}

func TestTemplatesNeedOwnedRefs(t *testing.T) {
	org, owner, team, teamAdmin, teamMember := uuid.New(), uuid.New(), uuid.New(), uuid.New(), uuid.New()
	userVM, teamVM, unowned, disk := uuid.New(), uuid.New(), uuid.New(), uuid.New()
	p := Policy{
		Memberships: []MemberRole{{OrgID: org, UserID: owner, Role: "member"}, {OrgID: org, UserID: teamAdmin, Role: "member"}, {OrgID: org, UserID: teamMember, Role: "member"}},
		TeamMembers: []TeamMember{{OrgID: org, TeamID: team, UserID: teamAdmin, Admin: true}, {OrgID: org, TeamID: team, UserID: teamMember}},
		Teams:       []TeamRef{{OrgID: org, ID: team}},
		Owned: []OwnedRef{
			{Kind: "vms", OrgID: org, ID: userVM, UserID: &owner},
			{Kind: "vms", OrgID: org, ID: teamVM, TeamID: &team},
			{Kind: "disks", OrgID: org, ID: disk, UserID: &owner},
		},
	}
	e := mustNew(t, p)
	for _, tc := range []struct {
		name     string
		sub      string
		obj, act string
		want     bool
	}{
		{"user owner", UserSubject(owner), VMObject(userVM), "compute.instances.fork", true},
		{"other user", UserSubject(teamMember), VMObject(userVM), "compute.instances.get", false},
		{"owning team", TeamSubject(team), VMObject(teamVM), "compute.instances.connect", true},
		{"owning team no delete", TeamSubject(team), VMObject(teamVM), "compute.instances.delete", false},
		{"owning team admin", UserSubject(teamAdmin), VMObject(teamVM), "compute.instances.delete", true},
		{"unowned vm", UserSubject(owner), VMObject(unowned), "compute.instances.get", false},
		{"vm template not on disk", UserSubject(owner), DiskObject(disk), "compute.instances.get", false},
		{"disk template", UserSubject(owner), DiskObject(disk), "compute.disks.use", true},
		{"disk template not on vm", UserSubject(owner), VMObject(userVM), "compute.disks.use", false},
	} {
		if got := e.Allowed(tc.sub, org, tc.obj, tc.act); got != tc.want {
			t.Errorf("%s: got %v", tc.name, got)
		}
	}
	if !e.CanConnect(owner, VMRef{OrgID: org, ID: userVM, UserID: &owner}) {
		t.Error("owner cannot connect to own vm")
	}
	if e.CanConnect(owner, VMRef{OrgID: org, ID: disk, UserID: &owner}) {
		t.Error("a disk ref must not count as a vm for connect")
	}
	p.Owned = nil
	e = mustNew(t, p)
	if e.Allowed(UserSubject(owner), org, VMObject(userVM), "compute.instances.fork") || e.Allowed(TeamSubject(team), org, VMObject(teamVM), "compute.instances.get") {
		t.Error("owner templates granted without an owned ref")
	}
}

func TestNoCatalogsGrantsOnlyOwnScope(t *testing.T) {
	org, u := uuid.New(), uuid.New()
	e, err := New(Policy{Memberships: []MemberRole{{OrgID: org, UserID: u, Role: "owner"}}})
	if err != nil {
		t.Fatal(err)
	}
	if !e.Allowed(UserSubject(u), org, UserSubject(u)+"/ssh-keys", "iam.sshKeys.create") {
		t.Error("own user scope must stay granted without catalogs")
	}
	for _, p := range legacyE().Actions() {
		if e.Allowed(UserSubject(u), org, concrete(p.Object), p.Action) {
			t.Errorf("%s@%s granted without catalogs", p.Action, p.Object)
		}
	}
	if got, err := e.EffectiveRules([]string{"owner"}, org); err != nil || len(got) != 0 {
		t.Errorf("EffectiveRules(owner) = %v, %v", got, err)
	}
	if len(e.Actions()) != 0 || len(e.ExpandActions("*")) != 0 {
		t.Error("no catalogs, yet actions")
	}
}

package authz

import (
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/google/uuid"
)

func TestCanGrant(t *testing.T) {
	org := uuid.New()
	owner, admin, deniedAdmin, member := uuid.New(), uuid.New(), uuid.New(), uuid.New()
	team, denyRole := uuid.New(), uuid.New()
	e := mustNew(t, Policy{
		Memberships: []MemberRole{
			{OrgID: org, UserID: owner, Role: "owner"},
			{OrgID: org, UserID: admin, Role: "admin"},
			{OrgID: org, UserID: deniedAdmin, Role: "admin"},
			{OrgID: org, UserID: member, Role: "member"},
		},
		Roles: []CustomRole{{ID: denyRole, OrgID: org, Name: "no-delete",
			Rules: []Rule{{Effect: Deny, Object: TeamObject(team), Action: "team:delete"}}}},
		Bindings: []Binding{{OrgID: org, Subject: UserSubject(deniedAdmin), RoleID: denyRole}},
	})
	var all []Rule
	for _, p := range Catalog {
		// Nobody holds every member's private env scope, vm:connect, disk:use or device:create on every object.
		if p.Object != ObjUserEnv+"/*" && p.Action != "vm:connect" && p.Action != "disk:use" && p.Action != "device:create" {
			all = append(all, Rule{Allow, p.Object, p.Action})
		}
	}
	allow := func(obj, act string) []Rule { return []Rule{{Allow, obj, act}} }
	deny := func(obj, act string) []Rule { return []Rule{{Deny, obj, act}} }
	tests := []struct {
		name    string
		actor   uuid.UUID
		isAdmin bool
		rules   []Rule
		want    error
	}{
		{"owner grants whole catalog", owner, true, all, nil},
		{"admin grants member:set-role", admin, true, allow(ObjMembers, "member:set-role"), ErrEscalation},
		{"admin grants team:* on all teams", admin, true, allow(ObjTeams+"/*", "team:*"), nil},
		{"denied admin grants team:delete on all teams", deniedAdmin, true, allow(ObjTeams+"/*", "team:delete"), ErrEscalation},
		{"denied admin grants team:* on denied team", deniedAdmin, true, allow(TeamObject(team), "team:*"), ErrEscalation},
		{"denied admin grants other action on denied team", deniedAdmin, true, allow(TeamObject(team), "team:update"), nil},
		{"denied admin grants team:delete on other team", deniedAdmin, true, allow(TeamObject(uuid.New()), "team:delete"), nil},
		{"admin grants org/*", admin, true, allow("org/*", "member:read"), ErrEscalation},
		{"member grants member:read", member, false, allow(ObjMembers, "member:read"), nil},
		{"member grants limit:set", member, false, allow(ObjLimits, "limit:set"), ErrEscalation},
		{"member writes deny", member, false, deny(ObjLimits, "limit:read"), ErrDenyNotAllowed},
		{"admin writes deny", admin, true, deny(ObjMembers, "member:set-role"), nil},
		{"invalid object pattern", owner, true, allow("org/vm*", "team:read"), ErrInvalidPattern},
		{"unknown action", owner, true, allow(ObjTeams, "nope:*"), ErrInvalidPattern},
		{"invalid deny pattern", owner, true, deny("org/vm*", "team:read"), ErrInvalidPattern},
		{"unknown effect", owner, true, []Rule{{"maybe", ObjTeams, "team:read"}}, ErrInvalidPattern},
		{"non-member grants", uuid.New(), false, allow(ObjMembers, "member:read"), ErrEscalation},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := e.CanGrant(UserSubject(tt.actor), org, tt.isAdmin, tt.rules)
			if tt.want == nil && err != nil || tt.want != nil && !errors.Is(err, tt.want) {
				t.Errorf("got %v, want %v", err, tt.want)
			}
		})
	}
}

func TestEffectiveRulesIncludesParents(t *testing.T) {
	org, other := uuid.New(), uuid.New()
	base, child, foreign := uuid.New(), uuid.New(), uuid.New()
	baseRule := Rule{Allow, ObjAudit, "audit:read"}
	childRule := Rule{Allow, ObjLimits, "limit:set"}
	e := mustNew(t, Policy{Roles: []CustomRole{
		{ID: base, OrgID: org, Parents: []string{"member"}, Rules: []Rule{baseRule}},
		{ID: child, OrgID: org, Parents: []string{base.String()},
			Rules: []Rule{childRule, {Deny, ObjMembers, "member:read"}}},
		{ID: foreign, OrgID: other, Rules: []Rule{{Allow, ObjSettings, "org:update"}}},
	}})
	got, err := e.EffectiveRules([]string{child.String()}, org)
	if err != nil {
		t.Fatal(err)
	}
	want := append([]Rule{childRule, baseRule}, BuiltinRules("member")...)
	if !slices.Equal(got, want) {
		t.Errorf("got %v, want %v (no deny)", got, want)
	}
	if got, err := e.EffectiveRules([]string{"owner"}, org); err != nil || !slices.Equal(got, BuiltinRules("owner")) {
		t.Errorf("owner: got %v, %v", got, err)
	}
	for _, key := range []string{"bogus", foreign.String(), strings.ToUpper(base.String()), uuid.NewString()} {
		if _, err := e.EffectiveRules([]string{"member", key}, org); err == nil {
			t.Errorf("%q: want error", key)
		}
	}
}

func TestCanGrantRole(t *testing.T) {
	org := uuid.New()
	owner, admin, member, adminRole := uuid.New(), uuid.New(), uuid.New(), uuid.New()
	e := mustNew(t, Policy{
		Memberships: []MemberRole{
			{OrgID: org, UserID: owner, Role: "owner"},
			{OrgID: org, UserID: admin, Role: "admin"},
			{OrgID: org, UserID: member, Role: "member"},
		},
		Roles: []CustomRole{{ID: adminRole, OrgID: org, Parents: []string{"admin"},
			Rules: []Rule{{Allow, ObjMembers, "member:set-role"}}}},
	})
	own := []Rule{{Allow, ObjMembers, "member:read"}}
	tests := []struct {
		name    string
		actor   uuid.UUID
		isAdmin bool
		parents []string
		want    error
	}{
		{"member, parent member", member, false, []string{"member"}, nil},
		{"member, parent admin", member, false, []string{"admin"}, ErrEscalation},
		{"admin, parent admin", admin, true, []string{"admin"}, nil},
		{"admin, parent with member:set-role", admin, true, []string{adminRole.String()}, ErrEscalation},
		{"admin, parent owner", admin, true, []string{"owner"}, ErrInvalidPattern},
		{"unknown parent", admin, true, []string{uuid.NewString()}, ErrInvalidPattern},
		{"owner, parent owner", owner, true, []string{"owner"}, ErrInvalidPattern},
		{"owner, non-canonical parent", owner, true, []string{strings.ToUpper(adminRole.String())}, ErrInvalidPattern},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := e.CanGrantRole(UserSubject(tt.actor), org, tt.isAdmin, own, tt.parents)
			if tt.want == nil && err != nil || tt.want != nil && !errors.Is(err, tt.want) {
				t.Errorf("got %v, want %v", err, tt.want)
			}
		})
	}
	if err := e.CanHold(UserSubject(admin), org, []Rule{{Allow, ObjTeams, "nope:*"}}); !errors.Is(err, ErrInvalidPattern) {
		t.Errorf("CanHold unknown action: got %v", err)
	}
}

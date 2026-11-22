package authz

import (
	"fmt"
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/google/uuid"
)

func fixedID(n int) uuid.UUID { return uuid.MustParse(fmt.Sprintf("00000000-0000-0000-0000-%012d", n)) }

func TestLegacyDecisions(t *testing.T) {
	orgA, orgB := fixedID(1), fixedID(2)
	ownerA, adminA, memberA, teamAdminA, teamMemberA := fixedID(11), fixedID(12), fixedID(13), fixedID(14), fixedID(15)
	ownerB, adminB, memberB, teamAdminB, teamMemberB := fixedID(21), fixedID(22), fixedID(23), fixedID(24), fixedID(25)
	teamA, emptyTeamA, teamB := fixedID(31), fixedID(32), fixedID(33)
	vmUserA, vmTeamA, vmUserB, vmTeamB := fixedID(41), fixedID(42), fixedID(43), fixedID(44)
	diskUserA, diskTeamA, diskUserB, diskTeamB := fixedID(51), fixedID(52), fixedID(53), fixedID(54)
	roleA := fixedID(61)

	ptr := func(id uuid.UUID) *uuid.UUID { return &id }
	policy := Policy{
		Memberships: []MemberRole{
			{orgA, ownerA, "owner"}, {orgA, adminA, "admin"}, {orgA, memberA, "member"},
			{orgA, teamAdminA, "member"}, {orgA, teamMemberA, "member"},
			{orgB, ownerB, "owner"}, {orgB, adminB, "admin"}, {orgB, memberB, "member"},
			{orgB, memberA, "member"}, {orgB, teamAdminB, "member"}, {orgB, teamMemberB, "member"},
		},
		TeamMembers: []TeamMember{
			{orgA, teamA, teamAdminA, true}, {orgA, teamA, teamMemberA, false},
			{orgB, teamB, memberB, true}, {orgB, teamB, teamAdminB, true}, {orgB, teamB, teamMemberB, false},
		},
		Teams: []TeamRef{{orgA, teamA}, {orgA, emptyTeamA}, {orgB, teamB}},
		Owned: []OwnedRef{
			{Kind: "vms", OrgID: orgA, ID: vmUserA, UserID: ptr(adminA)}, {Kind: "vms", OrgID: orgA, ID: vmTeamA, TeamID: ptr(teamA)},
			{Kind: "vms", OrgID: orgB, ID: vmUserB, UserID: ptr(memberB)}, {Kind: "vms", OrgID: orgB, ID: vmTeamB, TeamID: ptr(teamB)},
			{Kind: "disks", OrgID: orgA, ID: diskUserA, UserID: ptr(memberA)}, {Kind: "disks", OrgID: orgA, ID: diskTeamA, TeamID: ptr(teamA)},
			{Kind: "disks", OrgID: orgB, ID: diskUserB, UserID: ptr(memberB)}, {Kind: "disks", OrgID: orgB, ID: diskTeamB, TeamID: ptr(teamB)},
		},
		Roles: []CustomRole{{
			ID: roleA, OrgID: orgA, Name: "custom",
			Rules: []Rule{
				{Allow, ObjSettings, "iam.orgs.update"},
				{Allow, ObjVMs + "/*", "compute.instances.get"},
				{Deny, ObjVMs + "/*", "compute.instances.*"},
			},
			Parents: []string{"admin"},
		}},
		Bindings: []Binding{{orgA, TeamSubject(teamA), roleA}},
		Catalogs: LegacyCatalogs(),
	}
	e, err := New(policy)
	if err != nil {
		t.Fatal(err)
	}

	const path = "testdata/legacy_decisions.golden"
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	golden := strings.Split(strings.TrimSuffix(string(raw), "\n"), "\n")
	vms := map[string]VMRef{}
	for _, o := range policy.Owned {
		if o.Kind == "vms" {
			vms[o.ID.String()] = VMRef{OrgID: o.OrgID, ID: o.ID, UserID: o.UserID, TeamID: o.TeamID}
		}
	}
	grantRules := map[string][]Rule{
		"owner-rules":               e.BuiltinRules("owner"),
		"member-rules":              e.BuiltinRules("member"),
		"compute.instances.connect": {{Allow, ObjVMs + "/*", "compute.instances.connect"}},
	}
	actors := map[string]uuid.UUID{orgA.String(): adminA, orgB.String(): adminB}
	allowed := func(sub, org, obj, act string) bool { return e.Allowed(sub, uuid.MustParse(org), obj, act) }
	var got []string
	for _, l := range golden {
		f := strings.Fields(l)
		switch f[0] {
		case "catalog", "role":
			continue
		case "unruled":
			l = fmt.Sprintf("unruled %s %s %s %s %t", f[1], f[2], f[3], f[4], allowed(f[1], f[2], f[3], f[4]))
		case "connect":
			l = fmt.Sprintf("connect %s %s %t", f[1], f[2], e.CanConnect(uuid.MustParse(f[1]), vms[f[2]]))
		case "grant", "hold":
			name := strings.TrimSuffix(f[3], ":")
			org, actor, rules := uuid.MustParse(f[2]), UserSubject(actors[f[2]]), grantRules[name]
			err := e.CanHold(actor, org, rules)
			if f[0] == "grant" {
				err = e.CanGrantRole(actor, org, true, rules, nil)
			}
			verdict := "ok"
			if err != nil {
				verdict = err.Error()
			}
			l = fmt.Sprintf("%s admin %s %s: %s", f[0], f[2], name, verdict)
		default:
			verdict := "deny"
			if allowed(f[0], f[1], f[2], f[3]) {
				verdict = "allow"
			}
			l = fmt.Sprintf("%s %s %s %s %s", f[0], f[1], f[2], f[3], verdict)
		}
		got = append(got, l)
	}
	for _, p := range e.Actions() {
		got = append(got, "catalog "+p.Action+" "+p.Object)
	}
	for _, key := range []string{"owner", "admin", "member"} {
		for _, r := range e.BuiltinRules(key) {
			got = append(got, fmt.Sprintf("role %s %s %s %s", key, r.Effect, r.Object, r.Action))
		}
	}
	slices.Sort(got)
	for i := 0; i < len(got) || i < len(golden); i++ {
		if i >= len(got) || i >= len(golden) || got[i] != golden[i] {
			t.Fatalf("decisions differ from %s at line %d:\n got  %q\n want %q", path, i+1, at(got, i), at(golden, i))
		}
	}
}

func at(lines []string, i int) string {
	if i < len(lines) {
		return lines[i]
	}
	return "<end>"
}

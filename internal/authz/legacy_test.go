package authz

import (
	"flag"
	"fmt"
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/google/uuid"
)

var update = flag.Bool("update", false, "rewrite testdata golden files")

func fixedID(n int) uuid.UUID { return uuid.MustParse(fmt.Sprintf("00000000-0000-0000-0000-%012d", n)) }

func TestLegacyDecisions(t *testing.T) {
	orgA, orgB := fixedID(1), fixedID(2)
	ownerA, adminA, memberA, teamAdminA, teamMemberA := fixedID(11), fixedID(12), fixedID(13), fixedID(14), fixedID(15)
	ownerB, adminB, memberB := fixedID(21), fixedID(22), fixedID(23)
	teamA, emptyTeamA, teamB := fixedID(31), fixedID(32), fixedID(33)
	vmUserA, vmTeamA, vmUserB := fixedID(41), fixedID(42), fixedID(43)
	diskUserA, diskTeamA, diskUserB := fixedID(51), fixedID(52), fixedID(53)
	roleA, foreign := fixedID(61), fixedID(99)

	ptr := func(id uuid.UUID) *uuid.UUID { return &id }
	policy := Policy{
		Memberships: []MemberRole{
			{orgA, ownerA, "owner"}, {orgA, adminA, "admin"}, {orgA, memberA, "member"},
			{orgA, teamAdminA, "member"}, {orgA, teamMemberA, "member"},
			{orgB, ownerB, "owner"}, {orgB, adminB, "admin"}, {orgB, memberB, "member"},
			{orgB, memberA, "member"},
		},
		TeamMembers: []TeamMember{
			{orgA, teamA, teamAdminA, true}, {orgA, teamA, teamMemberA, false},
			{orgB, teamB, memberB, true},
		},
		Teams: []TeamRef{{orgA, teamA}, {orgA, emptyTeamA}, {orgB, teamB}},
		VMs: []VMRef{
			{OrgID: orgA, ID: vmUserA, UserID: ptr(adminA)}, {OrgID: orgA, ID: vmTeamA, TeamID: ptr(teamA)},
			{OrgID: orgB, ID: vmUserB, UserID: ptr(memberB)},
		},
		Disks: []DiskRef{
			{OrgID: orgA, ID: diskUserA, UserID: ptr(memberA)}, {OrgID: orgA, ID: diskTeamA, TeamID: ptr(teamA)},
			{OrgID: orgB, ID: diskUserB, UserID: ptr(memberB)},
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
	}
	e, err := New(policy)
	if err != nil {
		t.Fatal(err)
	}

	users := []uuid.UUID{ownerA, adminA, memberA, teamAdminA, teamMemberA, ownerB, adminB, memberB}
	teams := []uuid.UUID{teamA, emptyTeamA, teamB}
	var subjects []string
	for _, u := range users {
		subjects = append(subjects, UserSubject(u))
	}
	for _, tm := range teams {
		subjects = append(subjects, TeamSubject(tm))
	}
	ids := slices.Concat(
		[]uuid.UUID{orgA, orgB, roleA, foreign}, users, teams,
		[]uuid.UUID{vmUserA, vmTeamA, vmUserB, diskUserA, diskTeamA, diskUserB},
	)

	var lines []string
	for _, p := range Catalog {
		lines = append(lines, "catalog "+p.Action+" "+p.Object)
		objs := []string{p.Object}
		if base, ok := strings.CutSuffix(p.Object, "/*"); ok {
			objs = nil
			for _, id := range ids {
				objs = append(objs, base+"/"+id.String())
			}
		}
		for _, org := range []uuid.UUID{orgA, orgB} {
			for _, sub := range subjects {
				for _, obj := range objs {
					verdict := "deny"
					if e.Allowed(sub, org, obj, p.Action) {
						verdict = "allow"
					}
					lines = append(lines, fmt.Sprintf("%s %s %s %s %s", sub, org, obj, p.Action, verdict))
				}
			}
		}
	}
	for _, key := range []string{"owner", "admin", "member"} {
		for _, r := range BuiltinRules(key) {
			lines = append(lines, fmt.Sprintf("role %s %s %s %s", key, r.Effect, r.Object, r.Action))
		}
	}
	for _, vm := range policy.VMs {
		for _, u := range users {
			lines = append(lines, fmt.Sprintf("connect %s %s %t", u, vm.ID, e.CanConnect(u, vm)))
		}
	}
	grantOutcome := func(name string, rules []Rule) {
		for _, org := range []uuid.UUID{orgA, orgB} {
			actor := adminA
			if org == orgB {
				actor = adminB
			}
			verdict, holds := "ok", "ok"
			if err := e.CanGrantRole(UserSubject(actor), org, true, rules, nil); err != nil {
				verdict = err.Error()
			}
			if err := e.CanHold(UserSubject(actor), org, rules); err != nil {
				holds = err.Error()
			}
			lines = append(lines, fmt.Sprintf("grant admin %s %s: %s", org, name, verdict), fmt.Sprintf("hold admin %s %s: %s", org, name, holds))
		}
	}
	grantOutcome("owner-rules", BuiltinRules("owner"))
	grantOutcome("member-rules", BuiltinRules("member"))
	grantOutcome("compute.instances.connect", []Rule{{Allow, ObjVMs + "/*", "compute.instances.connect"}})

	slices.Sort(lines)
	got := strings.Join(slices.Compact(lines), "\n") + "\n"

	const path = "testdata/legacy_decisions.golden"
	if *update {
		if err := os.WriteFile(path, []byte(got), 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if got != string(want) {
		gotLines, wantLines := strings.Split(got, "\n"), strings.Split(string(want), "\n")
		for _, l := range gotLines {
			if _, found := slices.BinarySearch(wantLines, l); !found {
				t.Fatalf("decision not in golden: %s", l)
			}
		}
		for _, l := range wantLines {
			if _, found := slices.BinarySearch(gotLines, l); !found {
				t.Fatalf("golden decision missing: %s", l)
			}
		}
		t.Fatal("golden differs")
	}
}

package accounts

import (
	"slices"
	"testing"

	"github.com/google/uuid"

	"github.com/asterism-dev/observatory/internal/authz"
)

// withVMs adds a team VM and a VM of the team admin in default to the fixture, in both models.
func withVMs(f *fixture) (teamVM, adminVM uuid.UUID) {
	teamVM, adminVM = uuid.New(), uuid.New()
	f.policy.VMs = []authz.VMRef{{OrgID: f.org, ID: teamVM, TeamID: &f.team}, {OrgID: f.org, ID: adminVM, UserID: &f.teamAdmin}}
	f.snap.Resources["vm/"+teamVM.String()] = f.ml
	f.snap.Resources["vm/"+adminVM.String()] = f.def
	return teamVM, adminVM
}

func TestCompareCleanOrg(t *testing.T) {
	f := newFixture()
	withVMs(&f)
	rep, err := Compare(f.policy, f.snap)
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.Regressions) != 0 {
		t.Fatalf("regressions: %+v", rep.Regressions)
	}
	if rep.Widenings["compute:vm:connect"] == 0 {
		t.Error("member connecting to the team admin's VM in default must count as a widening")
	}
	if rep.Same == 0 || rep.Skipped == 0 {
		t.Errorf("counts: same=%d skipped=%d", rep.Same, rep.Skipped)
	}
}

func TestCompareFindsRegression(t *testing.T) {
	f := newFixture()
	teamVM, _ := withVMs(&f)
	f.snap.Accounts[1].TeamID = nil // the team account lost its team, so Derive binds nobody there
	rep, err := Compare(f.policy, f.snap)
	if err != nil {
		t.Fatal(err)
	}
	want := Diff{User: f.teamAdmin, Org: f.org, Object: "org/vms/" + teamVM.String(), Action: "vm:connect"}
	if !slices.ContainsFunc(rep.Regressions, func(d Diff) bool {
		return d.User == want.User && d.Org == want.Org && d.Object == want.Object && d.Action == want.Action
	}) {
		t.Fatalf("missing regression %+v in %+v", want, rep.Regressions)
	}
}

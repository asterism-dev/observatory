package authz

import (
	"slices"
	"testing"
)

// LegacyCatalogs is authztest.Legacy, set from package authz_test since authztest imports authz.
var LegacyCatalogs func() []Catalog

// legacyE is an empty policy over the legacy catalogs, for the catalog-derived methods.
func legacyE() *Enforcer {
	e, err := New(Policy{Catalogs: LegacyCatalogs()})
	if err != nil {
		panic(err)
	}
	return e
}

func mustNew(t *testing.T, p Policy) *Enforcer {
	t.Helper()
	if p.Catalogs == nil {
		p.Catalogs = LegacyCatalogs()
	}
	e, err := New(p)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return e
}

func vmsOwned(refs ...VMRef) []OwnedRef {
	out := make([]OwnedRef, 0, len(refs))
	for _, r := range refs {
		out = append(out, OwnedRef{Kind: "vms", OrgID: r.OrgID, ID: r.ID, UserID: r.UserID, TeamID: r.TeamID})
	}
	return out
}

func sorted(s []string) []string {
	s = slices.Clone(s)
	slices.Sort(s)
	return s
}

// allowAll is what a stored allow-everything rule looks like: allow wildcards are stored expanded.
func allowAll(obj string) []Rule {
	var out []Rule
	for _, a := range legacyE().ExpandActions("*") {
		out = append(out, Rule{Allow, obj, a})
	}
	return out
}

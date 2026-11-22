package authztest

import (
	"slices"
	"testing"

	"github.com/asterism-dev/observatory/internal/authz"
)

func TestLegacyMatchesHardcoded(t *testing.T) {
	var actions []authz.Permission
	defaults := map[string][]authz.Rule{}
	for _, c := range Legacy() {
		for _, a := range c.Actions {
			actions = append(actions, authz.Permission{Action: a.Name, Object: a.Object})
		}
		for _, d := range c.RoleDefaults {
			defaults[d.Role] = append(defaults[d.Role], authz.Rule{Effect: authz.Allow, Object: d.Object, Action: d.Action})
		}
	}
	if !sameSet(actions, authz.LegacyCatalog) {
		t.Errorf("actions differ from authz.LegacyCatalog:\n got  %v\n want %v", actions, authz.LegacyCatalog)
	}
	var cumulative []authz.Rule
	for _, key := range []string{"member", "admin", "owner"} {
		cumulative = append(cumulative, defaults[key]...)
		if !sameSet(cumulative, authz.BuiltinRules(key)) {
			t.Errorf("%s defaults differ:\n got  %v\n want %v", key, cumulative, authz.BuiltinRules(key))
		}
	}
}

func sameSet[T comparable](a, b []T) bool {
	in := func(s []T) func(T) bool { return func(x T) bool { return slices.Contains(s, x) } }
	for _, x := range a {
		if !in(b)(x) {
			return false
		}
	}
	for _, x := range b {
		if !in(a)(x) {
			return false
		}
	}
	return true
}

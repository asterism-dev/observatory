package main

import (
	"testing"
)

func TestPick(t *testing.T) {
	items := []ref{{"a1", "alpha"}, {"b2", "beta"}, {"b3", "beta"}}
	if r, err := pick("vm", "a1", items); err != nil || r.Name != "alpha" {
		t.Fatalf("by id: %v %v", r, err)
	}
	if r, err := pick("vm", "alpha", items); err != nil || r.ID != "a1" {
		t.Fatalf("by name: %v %v", r, err)
	}
	err := func() error { _, err := pick("vm", "nope", items); return err }()
	if exitCode(err) != 5 || err.Error() != `vm "nope" not found` {
		t.Fatalf("missing: %v", err)
	}
	err = func() error { _, err := pick("vm", "beta", items); return err }()
	if exitCode(err) != 2 || err.Error() != `vm "beta" is ambiguous: b2, b3; use the ID` {
		t.Fatalf("ambiguous: %v", err)
	}
}

func TestResolveScope(t *testing.T) {
	h := newFakeHub(t)
	meOneOrg(h)
	h.on(iamRPC+"ListTeams", 200, `{"teams":[{"id":"t1","name":"backend","memberCount":2}]}`)
	resolve := func(args ...string) (string, *string, error) {
		c, _, _ := newTestCLI(t, h)
		var s scopeFlags
		fs := c.flags("x")
		addScopeFlags(fs, &s, "scope")
		if _, err := c.parse(fs, args, 0, 0); err != nil {
			t.Fatal(err)
		}
		return c.resolveScope(s)
	}
	if sc, team, err := resolve(); err != nil || sc != "user" || team != nil {
		t.Fatalf("default: %v %v %v", sc, team, err)
	}
	if sc, team, err := resolve("--team", "backend"); err != nil || sc != "team" || team == nil || *team != "t1" {
		t.Fatalf("team: %v %v %v", sc, team, err)
	}
	if _, _, err := resolve("--scope", "team"); exitCode(err) != 2 {
		t.Fatalf("team without --team: %v", err)
	}
	if sc, team, err := resolve("--scope", "org"); err != nil || sc != "org" || team != nil {
		t.Fatalf("org: %v %v %v", sc, team, err)
	}
	if _, _, err := resolve("--scope", "bogus"); exitCode(err) != 2 {
		t.Fatalf("bogus: %v", err)
	}
	for in, want := range map[[2]string]string{{"user", ""}: "private", {"team", "backend"}: "team backend", {"org", ""}: "org"} {
		if got := scopeLabel(in[0], in[1]); got != want {
			t.Errorf("scopeLabel%v = %q want %q", in, got, want)
		}
	}
}

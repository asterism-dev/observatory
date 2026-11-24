package policy

import "testing"

const acc = "6f1c2b1e-0a4b-4c1e-9a77-1f2e3d4c5b6a"

func TestMatchResource(t *testing.T) {
	vm := "obs:compute:" + acc + ":vm/9b7e"
	cases := []struct {
		p    string
		want bool
	}{
		{vm, true}, {"obs:compute:" + acc + ":vm/*", true}, {"obs:*:" + acc + ":*", true},
		{"obs:compute:*:vm/9b7e", true}, {"obs:compute:" + acc + ":vm/9b7?", true},
		{"obs:*", false}, // * must not swallow the account part
		{"obs:compute:" + acc + ":disk/*", false}, {"obs:compute:" + acc + ":vm/9b7", false},
	}
	for _, c := range cases {
		if got := MatchResource(c.p, vm); got != c.want {
			t.Errorf("MatchResource(%q) = %v", c.p, got)
		}
	}
	if !MatchResource("obs:compute:"+acc+":vm/*", vm+"/snapshot/1") {
		t.Error("vm/* must cover sub-resources")
	}
}

func TestMatchAction(t *testing.T) {
	for _, c := range []struct {
		p, a string
		want bool
	}{
		{"*", "compute:vm:start", true}, {"compute:*", "compute:vm:start", true},
		{"compute:vm:*", "compute:vm:start", true}, {"compute:vm:st?rt", "compute:vm:start", true},
		{"compute:disk:*", "compute:vm:start", false}, {"compute:vm:start", "compute:vm:stop", false},
	} {
		if got := MatchAction(c.p, c.a); got != c.want {
			t.Errorf("MatchAction(%q,%q) = %v", c.p, c.a, got)
		}
	}
}

func TestCoversResource(t *testing.T) {
	p := "obs:compute:" + acc + ":"
	for _, c := range []struct {
		held, granted string
		want          bool
	}{
		{p + "vm/*", p + "vm/*", true}, {p + "vm/*", p + "vm/9b7e", true}, {p + "*", p + "vm/*", true},
		{"obs:*:" + acc + ":*", p + "vm/?", true}, {p + "vm/9b7e", p + "vm/*", false},
	} {
		if got := CoversResource(c.held, c.granted); got != c.want {
			t.Errorf("CoversResource(%q,%q) = %v", c.held, c.granted, got)
		}
	}
}

func TestCoversRefusesWiderGrant(t *testing.T) {
	p := "obs:compute:" + acc + ":"
	for _, c := range [][2]string{
		{p + "vm/*", p + "vm*"},  // vm* also matches "vmx/1"
		{p + "vm/?", p + "vm/*"}, // ? is one char, * is many
		{"obs:compute:*:vm/*", "obs:*:" + acc + ":vm/*"},
	} {
		if CoversResource(c[0], c[1]) {
			t.Errorf("held %q must not cover %q", c[0], c[1])
		}
	}
	if CoversAction("compute:vm:*", "compute:*") {
		t.Error("compute:vm:* must not cover compute:*")
	}
}

func TestOverlaps(t *testing.T) {
	p := "obs:compute:" + acc + ":"
	if !OverlapsResource(p+"vm/*", p+"vm/9b7e") || !OverlapsResource(p+"vm/9*", p+"vm/*e") {
		t.Error("expected overlap")
	}
	if OverlapsResource(p+"vm/*", p+"disk/*") || OverlapsAction("compute:vm:*", "compute:disk:*") {
		t.Error("expected no overlap")
	}
}

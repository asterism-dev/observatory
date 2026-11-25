package accounts

import (
	"testing"

	"github.com/google/uuid"

	"github.com/asterism-dev/observatory/internal/policy"
)

func TestTranslate(t *testing.T) {
	f := newFixture()
	teamVM, user := uuid.New(), uuid.New()
	f.snap.Resources["vm/"+teamVM.String()] = f.ml
	for _, c := range []struct {
		obj, act string
		want     policy.Request
	}{
		{"org/vms/" + teamVM.String(), "vm:power", policy.Request{Acting: f.ml, Action: "compute:vm:power", Resource: policy.NewID("compute", f.ml, "vm", teamVM.String())}},
		{"org/team-vms/" + f.team.String(), "vm:create", policy.Request{Acting: f.ml, Action: "compute:vm:create", Resource: policy.NewID("compute", f.ml, "vm", "-")}},
		{"org/user-env/" + user.String(), "env:set", policy.Request{Acting: f.def, Action: "secrets:env:set", Resource: policy.NewID("secrets", f.def, "env", "-")}},
		{"org/vms", "vm:create", policy.Request{Acting: f.def, Action: "compute:vm:create", Resource: policy.NewID("compute", f.def, "vm", "-")}},
		{"org/settings", "telemetry:token", policy.Request{Acting: f.def, Action: "telemetry:token:create", Resource: policy.NewID("telemetry", f.def, "token", "-")}},
	} {
		got, ok := Translate(f.snap, f.org, c.obj, c.act)
		if !ok || got.Acting != c.want.Acting || got.Action != c.want.Action || got.Resource != c.want.Resource {
			t.Errorf("Translate(%s, %s) = %+v, %v; want %+v", c.obj, c.act, got, ok, c.want)
		}
	}
	for _, c := range [][2]string{{"org/members", "member:read"}, {"org/settings", "org:update"}, {"org/user-devices/" + user.String(), "device:create"}} {
		if _, ok := Translate(f.snap, f.org, c[0], c[1]); ok {
			t.Errorf("Translate(%s, %s) must be skipped", c[0], c[1])
		}
	}
}

func TestTranslateUnknownResourceSkipped(t *testing.T) {
	f := newFixture()
	if got, ok := Translate(f.snap, f.org, "org/vms/"+uuid.NewString(), "vm:power"); ok {
		t.Fatalf("unknown VM translated to %+v", got)
	}
}

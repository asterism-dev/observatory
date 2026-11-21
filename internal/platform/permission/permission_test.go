package permission_test

import (
	"testing"

	testv1 "github.com/asterism-dev/observatory/gen/go/observatory/test/v1"
	"github.com/asterism-dev/observatory/internal/platform/permission"
)

func TestPermissionOf(t *testing.T) {
	md := testv1.File_observatory_test_v1_echo_proto.Services().ByName("EchoService").Methods().ByName("Echo")
	got, ok := permission.Of(md)
	want := permission.Permission{Name: "test.echoes.create", Object: "org/echoes", Write: true}
	if !ok || got != want {
		t.Fatalf("Of = %+v, %v; want %+v", got, ok, want)
	}
}

func TestPermissionOfRejectsUnspecifiedKind(t *testing.T) {
	md := testv1.File_observatory_test_v1_echo_proto.Services().ByName("EchoService").Methods().ByName("Peek")
	if got, ok := permission.Of(md); ok {
		t.Fatalf("Of = %+v, true; want false for KIND_UNSPECIFIED", got)
	}
}

func TestValidName(t *testing.T) {
	for s, want := range map[string]bool{
		"logging.logEntries.list": true,
		"compute.instances.start": true,
		"vm:power":                false,
		"Logging.x.y":             false,
		"a.b":                     false,
		"a.b.c.d":                 false,
	} {
		if got := permission.ValidName(s); got != want {
			t.Errorf("ValidName(%q) = %v, want %v", s, got, want)
		}
	}
}

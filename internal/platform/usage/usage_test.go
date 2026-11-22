package usage

import (
	"testing"

	"github.com/google/uuid"
	"google.golang.org/protobuf/types/known/structpb"
)

func TestEventRoundTrip(t *testing.T) {
	org, vm := uuid.New(), uuid.New()
	l := Lifecycle{Resource: "snapshot/" + uuid.NewString(), Parent: "vm/" + vm.String(), Owner: "user:" + uuid.NewString(), Amounts: Amounts{DiskGB: 10, SnapshotCount: 1}}
	e := Event(org, SnapshotCreated, "org/vms/"+vm.String(), l)
	if e.GetType() != SnapshotCreated || e.GetOrg() != org.String() || e.GetResource() != "org/vms/"+vm.String() || e.GetAudit() {
		t.Fatalf("event = %v", e)
	}
	got, err := Parse(e)
	if err != nil || got != l {
		t.Fatalf("Parse = %+v, %v; want %+v", got, err, l)
	}
}

func TestCommittedRoundTrip(t *testing.T) {
	ids := []uuid.UUID{uuid.New(), uuid.New()}
	e := Committed(uuid.New(), ids)
	if e.GetType() != ReservationsCommitted {
		t.Fatalf("type = %q", e.GetType())
	}
	got, err := ParseCommitted(e)
	if err != nil || len(got) != 2 || got[0] != ids[0] || got[1] != ids[1] {
		t.Fatalf("ParseCommitted = %v, %v", got, err)
	}
}

func TestParseRejectsBadDetails(t *testing.T) {
	good := Event(uuid.New(), InstanceCreated, "org/vms/x", Lifecycle{Resource: "vm/x", Owner: "user:x", Amounts: Amounts{VCPU: 2}})
	for name, mutate := range map[string]func(map[string]*structpb.Value){
		"no resource":   func(f map[string]*structpb.Value) { delete(f, "resource") },
		"negative":      func(f map[string]*structpb.Value) { f["vcpu"] = structpb.NewNumberValue(-1) },
		"fraction":      func(f map[string]*structpb.Value) { f["ramMb"] = structpb.NewNumberValue(1.5) },
		"too big":       func(f map[string]*structpb.Value) { f["diskGb"] = structpb.NewNumberValue(1 << 31) },
		"string amount": func(f map[string]*structpb.Value) { f["vmCount"] = structpb.NewStringValue("1") },
	} {
		e := Event(uuid.New(), InstanceCreated, "org/vms/x", Lifecycle{Resource: "vm/x", Owner: "user:x", Amounts: Amounts{VCPU: 2}})
		mutate(e.Details.Fields)
		if _, err := Parse(e); err == nil {
			t.Errorf("%s: Parse accepted %v", name, e.Details)
		}
	}
	if _, err := Parse(good); err != nil {
		t.Fatal(err)
	}
	if ids, err := ParseCommitted(Committed(uuid.New(), nil)); err != nil || len(ids) != 0 {
		t.Fatalf("empty list: %v, %v", ids, err)
	}
	none := Committed(uuid.New(), nil)
	delete(none.Details.Fields, "reservations")
	if _, err := ParseCommitted(none); err == nil {
		t.Fatal("ParseCommitted accepted an event without reservations")
	}
	bad := Committed(uuid.New(), nil)
	bad.Details.Fields["reservations"] = structpb.NewListValue(&structpb.ListValue{Values: []*structpb.Value{structpb.NewStringValue("nope")}})
	if _, err := ParseCommitted(bad); err == nil {
		t.Fatal("ParseCommitted accepted a bad id")
	}
}

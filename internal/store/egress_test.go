package store_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/asterism-dev/observatory/internal/store"
)

var egressT0 = time.Date(2026, 11, 18, 12, 0, 0, 0, time.UTC)

func egressVM(t *testing.T, e vmEnv, name string) store.VM {
	t.Helper()
	v, err := e.s.CreateVM(context.Background(), e.vm(name, nil, 1, 1, 1))
	if err != nil {
		t.Fatal(err)
	}
	return v
}

func mustEnsureCAs(t *testing.T, e vmEnv, vm uuid.UUID, now time.Time, hosts ...string) []store.EgressCA {
	t.Helper()
	cas, err := e.s.EnsureEgressCAs(context.Background(), e.org.ID, vm, hosts, now)
	if err != nil {
		t.Fatal(err)
	}
	return cas
}

func caRows(t *testing.T, e vmEnv, vm uuid.UUID) (n int) {
	t.Helper()
	if err := e.pool.QueryRow(context.Background(), "SELECT count(*) FROM vm_egress_cas WHERE vm_id = $1", vm).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestEnsureEgressCAsMintsOnce(t *testing.T) {
	e := newDiskEnv(t)
	vm := egressVM(t, e, "v")
	a := mustEnsureCAs(t, e, vm.ID, egressT0, "a.test")
	b := mustEnsureCAs(t, e, vm.ID, egressT0, "a.test")
	if len(a) != 1 || len(b) != 1 || a[0].ID != b[0].ID || a[0].KeyPEM == "" || caRows(t, e, vm.ID) != 1 {
		t.Fatalf("%v %v", a, b)
	}
}

func TestEnsureEgressCAsRotates(t *testing.T) {
	e := newDiskEnv(t)
	vm := egressVM(t, e, "v")
	first := mustEnsureCAs(t, e, vm.ID, egressT0, "a.test")[0]
	if got := mustEnsureCAs(t, e, vm.ID, egressT0.Add(14*24*time.Hour), "a.test"); len(got) != 1 {
		t.Fatalf("at 14d: %d", len(got))
	}
	got := mustEnsureCAs(t, e, vm.ID, egressT0.Add(15*24*time.Hour+time.Second), "a.test")
	if len(got) != 2 || got[0].ID != first.ID {
		t.Fatalf("at 15d: %v", got)
	}
	got = mustEnsureCAs(t, e, vm.ID, egressT0.Add(31*24*time.Hour), "a.test")
	for _, c := range got {
		if c.ID == first.ID {
			t.Fatalf("first CA still present at 31d")
		}
	}
}

func TestEnsureEgressCAsNewHost(t *testing.T) {
	e := newDiskEnv(t)
	vm := egressVM(t, e, "v")
	mustEnsureCAs(t, e, vm.ID, egressT0, "a.test")
	got := mustEnsureCAs(t, e, vm.ID, egressT0, "b.test", "a.test")
	if len(got) != 2 || len(got[1].Hosts) != 2 || got[1].Hosts[0] != "a.test" || len(got[0].Hosts) != 1 {
		t.Fatalf("%+v", got)
	}
}

func TestEnsureEgressCAsEmptyDeletes(t *testing.T) {
	e := newDiskEnv(t)
	vm := egressVM(t, e, "v")
	mustEnsureCAs(t, e, vm.ID, egressT0, "a.test")
	if got := mustEnsureCAs(t, e, vm.ID, egressT0); got != nil || caRows(t, e, vm.ID) != 0 {
		t.Fatalf("%v", got)
	}
}

func TestEgressCAKeySealed(t *testing.T) {
	ctx := context.Background()
	e := newDiskEnv(t)
	vm, other := egressVM(t, e, "v"), egressVM(t, e, "w")
	ca := mustEnsureCAs(t, e, vm.ID, egressT0, "a.test")[0]
	var ct []byte
	if err := e.pool.QueryRow(ctx, "SELECT key_ciphertext FROM vm_egress_cas WHERE id = $1", ca.ID).Scan(&ct); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(ct), "PRIVATE KEY") {
		t.Fatal("key stored in clear")
	}
	if _, err := e.pool.Exec(ctx, "UPDATE vm_egress_cas SET vm_id = $1 WHERE id = $2", other.ID, ca.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := e.s.EnsureEgressCAs(ctx, e.org.ID, other.ID, []string{"a.test"}, egressT0); err == nil {
		t.Fatal("moved row opened")
	}
}

func TestEgressCAInfosHaveNoKey(t *testing.T) {
	e := newDiskEnv(t)
	vm := egressVM(t, e, "v")
	mustEnsureCAs(t, e, vm.ID, egressT0, "a.test")
	got, err := e.s.EgressCAInfos(context.Background(), vm.ID, egressT0)
	if err != nil || len(got) != 1 || got[0].KeyPEM != "" || got[0].CertPEM == "" {
		t.Fatalf("%v %v", got, err)
	}
}

func TestEgressCAKeyNotPrinted(t *testing.T) {
	ca := store.EgressCA{KeyPEM: "SECRET"}
	if s := ca.String() + ca.GoString(); strings.Contains(s, "SECRET") {
		t.Fatal(s)
	}
}

func TestForkCopiesEgressCAs(t *testing.T) {
	ctx := context.Background()
	e := newDiskEnv(t)
	vm, _ := vmWithDisk(t, e, "v")
	src := mustEnsureCAs(t, e, vm.ID, time.Now(), "a.test")[0]
	snap := readySnapshot(t, e, vm.ID, "s")
	f, err := e.s.ForkVM(ctx, e.org.ID, snap, "f", false, e.user)
	if err != nil {
		t.Fatal(err)
	}
	got := mustEnsureCAs(t, e, f.ID, time.Now(), "a.test")
	if len(got) != 1 || got[0].CertPEM != src.CertPEM || got[0].ID == src.ID || got[0].KeyPEM != src.KeyPEM {
		t.Fatalf("%+v", got)
	}
}

func TestEgressCAOrgs(t *testing.T) {
	ctx := context.Background()
	e := newDiskEnv(t)
	vm := egressVM(t, e, "v")
	mustEnsureCAs(t, e, vm.ID, egressT0, "a.test")
	for daysLeft, want := range map[int]bool{10: true, 20: false} {
		orgs, err := e.s.EgressCAOrgs(ctx, egressT0.Add(time.Duration(30-daysLeft)*24*time.Hour))
		if err != nil || (len(orgs) == 1) != want {
			t.Fatalf("expires in %dd: %v %v", daysLeft, orgs, err)
		}
	}
}

package store_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/netip"
	"sync"
	"testing"

	"github.com/asterism-dev/observatory/internal/netaddr"
	"github.com/asterism-dev/observatory/internal/store"
)

func TestOrgSliceStableAndDistinct(t *testing.T) {
	ctx := context.Background()
	e := newVMEnv(t)
	other := mustCreateOrg(t, e.s, "B", e.user)
	a, err := e.s.OrgSlice(ctx, e.org.ID)
	if err != nil {
		t.Fatal(err)
	}
	b, err := e.s.OrgSlice(ctx, other.ID)
	if err != nil {
		t.Fatal(err)
	}
	if a == b || a.Bits() != netaddr.SliceBits || b.Bits() != netaddr.SliceBits || !netaddr.MgmtPool.Contains(a.Addr()) || !netaddr.MgmtPool.Contains(b.Addr()) {
		t.Fatalf("slices %s %s", a, b)
	}
	again, err := e.s.OrgSlice(ctx, e.org.ID)
	if err != nil || again != a {
		t.Fatalf("second call: %s %v, want %s", again, err, a)
	}
}

func TestConcurrentFirstVMsGetDistinctSlices(t *testing.T) {
	ctx := context.Background()
	e := newVMEnv(t)
	const n = 8
	vms := make([]store.VM, n)
	for i := range vms {
		org := mustCreateOrg(t, e.s, fmt.Sprintf("org%d", i), e.user)
		node, tok, err := e.s.CreateNode(ctx, store.Node{OrgID: org.ID, Name: "n", CreatedBy: e.user})
		if err != nil {
			t.Fatal(err)
		}
		if _, _, err := e.s.EnrollNode(ctx, tok); err != nil {
			t.Fatal(err)
		}
		vms[i] = store.VM{OrgID: org.ID, NodeID: node.ID, UserID: &e.user, Name: "vm", CPUs: 1, MemoryMB: 1, DiskGB: 1, CreatedBy: e.user}
	}
	var wg sync.WaitGroup
	errs := make([]error, n)
	for i := range vms {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, errs[i] = e.s.CreateVM(ctx, vms[i])
		}()
	}
	wg.Wait()
	slices := map[netip.Prefix]bool{}
	for i, v := range vms {
		if errs[i] != nil {
			t.Fatalf("vm %d: %v", i, errs[i])
		}
		p, err := e.s.OrgSlice(ctx, v.OrgID)
		if err != nil {
			t.Fatal(err)
		}
		slices[p] = true
	}
	if len(slices) != n {
		t.Fatalf("got %d distinct slices, want %d", len(slices), n)
	}
}

func TestCreateVMAssignsMgmtIP(t *testing.T) {
	ctx := context.Background()
	e := newVMEnv(t)
	slice, err := e.s.OrgSlice(ctx, e.org.ID)
	if err != nil {
		t.Fatal(err)
	}
	first := netaddr.Router(slice)
	for range 9 {
		first = first.Next()
	}
	v1, err := e.s.CreateVM(ctx, e.vm("v1", nil, 1, 1, 1))
	if err != nil {
		t.Fatal(err)
	}
	v2, err := e.s.CreateVM(ctx, e.vm("v2", nil, 1, 1, 1))
	if err != nil {
		t.Fatal(err)
	}
	if v1.MgmtIP != first || v2.MgmtIP != first.Next() {
		t.Fatalf("got %s %s, want %s and next", v1.MgmtIP, v2.MgmtIP, first)
	}
	if err := e.s.DeleteVM(ctx, e.org.ID, v1.ID, nil, store.DeleteReasonUser); err != nil {
		t.Fatal(err)
	}
	v3, err := e.s.CreateVM(ctx, e.vm("v3", nil, 1, 1, 1))
	if err != nil {
		t.Fatal(err)
	}
	if v3.MgmtIP != first {
		t.Fatalf("v3 got %s, want %s", v3.MgmtIP, first)
	}
	got, err := e.s.VM(ctx, e.org.ID, v2.ID)
	if err != nil || got.MgmtIP != v2.MgmtIP {
		t.Fatalf("reload: %s %v", got.MgmtIP, err)
	}
	for _, ip := range []netip.Addr{v2.MgmtIP, v3.MgmtIP} {
		if !slice.Contains(ip) {
			t.Fatalf("%s outside %s", ip, slice)
		}
	}
}

func TestBackfillMgmtIPs(t *testing.T) {
	ctx := context.Background()
	e := newVMEnv(t)
	v, err := e.s.CreateVM(ctx, e.vm("v1", nil, 1, 1, 1))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.pool.Exec(ctx, "UPDATE vms SET mgmt_ip = NULL WHERE id = $1", v.ID); err != nil {
		t.Fatal(err)
	}
	if n, err := e.s.BackfillMgmtIPs(ctx); err != nil || n != 1 {
		t.Fatalf("first backfill: %d %v", n, err)
	}
	got, err := e.s.VM(ctx, e.org.ID, v.ID)
	if err != nil || !got.MgmtIP.IsValid() {
		t.Fatalf("after backfill: %s %v", got.MgmtIP, err)
	}
	if n, err := e.s.BackfillMgmtIPs(ctx); err != nil || n != 0 {
		t.Fatalf("second backfill: %d %v", n, err)
	}
}

func TestEnrollChassisKeepsAddress(t *testing.T) {
	ctx := context.Background()
	e := newVMEnv(t)
	ip1, err := e.s.EnrollChassis(ctx, e.node.ID, "key1")
	if err != nil {
		t.Fatal(err)
	}
	again, err := e.s.EnrollChassis(ctx, e.node.ID, "key2")
	if err != nil || again != ip1 {
		t.Fatalf("re-enroll: %s %v, want %s", again, err, ip1)
	}
	var key string
	if err := e.pool.QueryRow(ctx, "SELECT chassis_key FROM nodes WHERE id = $1", e.node.ID).Scan(&key); err != nil || key != "key2" {
		t.Fatalf("chassis_key %q %v", key, err)
	}
	n2, _, err := e.s.CreateNode(ctx, store.Node{OrgID: e.org.ID, Name: "n2", CreatedBy: e.user})
	if err != nil {
		t.Fatal(err)
	}
	ip2, err := e.s.EnrollChassis(ctx, n2.ID, "key3")
	if err != nil {
		t.Fatal(err)
	}
	low := netip.MustParseAddr("10.198.0.10")
	for _, ip := range []netip.Addr{ip1, ip2} {
		if !netaddr.UnderlayPool.Contains(ip) || ip.Less(low) {
			t.Fatalf("underlay %s out of range", ip)
		}
	}
	if ip1 == ip2 {
		t.Fatalf("both nodes got %s", ip1)
	}
	node, err := e.s.Node(ctx, e.org.ID, n2.ID)
	if err != nil || node.UnderlayIP != ip2 {
		t.Fatalf("Node.UnderlayIP %s %v", node.UnderlayIP, err)
	}
}

func TestHubSecretSealed(t *testing.T) {
	ctx := context.Background()
	pool := newVMEnv(t).pool
	s := store.New(pool).WithMasterKey(testKey(1))
	plain := []byte("super-secret-hub-key-material")
	calls := 0
	gen := func() ([]byte, error) { calls++; return plain, nil }
	a, err := s.HubSecret(ctx, "ovn-ca", gen)
	if err != nil {
		t.Fatal(err)
	}
	b, err := s.HubSecret(ctx, "ovn-ca", gen)
	if err != nil {
		t.Fatal(err)
	}
	if calls != 1 || !bytes.Equal(a, plain) || !bytes.Equal(b, plain) {
		t.Fatalf("calls %d, a %q, b %q", calls, a, b)
	}
	var sealed []byte
	if err := pool.QueryRow(ctx, "SELECT sealed FROM hub_secrets WHERE name = 'ovn-ca'").Scan(&sealed); err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(sealed, plain) {
		t.Fatal("hub secret stored in plaintext")
	}
	if _, err := store.New(pool).WithMasterKey(testKey(2)).HubSecret(ctx, "ovn-ca", gen); err == nil {
		t.Fatal("other master key opened the secret")
	}
	if _, err := store.New(pool).HubSecret(ctx, "ovn-ca", gen); err == nil {
		t.Fatal("no master key returned a secret")
	}
}

func TestWireGuardKeysUniqueAcrossNodesAndDevices(t *testing.T) {
	ctx := context.Background()
	e := newVMEnv(t)
	if _, err := e.s.EnrollChassis(ctx, e.node.ID, "chassis"); err != nil {
		t.Fatal(err)
	}
	n2, _, err := e.s.CreateNode(ctx, store.Node{OrgID: e.org.ID, Name: "n2", CreatedBy: e.user})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.s.EnrollChassis(ctx, n2.ID, "chassis"); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("other node's chassis key: %v", err)
	}
	if _, err := e.s.CreateDevice(ctx, store.Device{OrgID: e.org.ID, UserID: e.user, Name: "d", PublicKey: "chassis"}); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("device with chassis key: %v", err)
	}
	mustCreateDevice(t, e.s, e.org.ID, e.user, "d", "device")
	if _, err := e.s.EnrollChassis(ctx, n2.ID, "device"); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("chassis with device key: %v", err)
	}
	if _, err := e.s.EnrollChassis(ctx, e.node.ID, "chassis"); err != nil {
		t.Fatalf("re-enroll with own key: %v", err)
	}
}

func TestMgmtSliceInHubOrgs(t *testing.T) {
	ctx := context.Background()
	e := newVMEnv(t)
	slice, err := e.s.OrgSlice(ctx, e.org.ID)
	if err != nil {
		t.Fatal(err)
	}
	var stored netip.Prefix
	if err := e.pool.QueryRow(ctx, "SELECT mgmt_slice FROM hub_orgs WHERE org_id = $1", e.org.ID).Scan(&stored); err != nil || stored != slice {
		t.Fatalf("hub_orgs: %s %v, want %s", stored, err, slice)
	}
	var cols int
	if err := e.pool.QueryRow(ctx, "SELECT count(*) FROM information_schema.columns WHERE table_name = 'orgs' AND column_name = 'mgmt_slice'").Scan(&cols); err != nil || cols != 0 {
		t.Fatalf("orgs.mgmt_slice still there: %d %v", cols, err)
	}
}

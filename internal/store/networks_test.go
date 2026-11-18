package store_test

import (
	"context"
	"errors"
	"net/netip"
	"testing"

	"github.com/google/uuid"

	"github.com/asterism-dev/observatory/internal/netaddr"
	"github.com/asterism-dev/observatory/internal/proto"
	"github.com/asterism-dev/observatory/internal/store"
)

func mustCreateNetwork(t *testing.T, s *store.Store, org, user uuid.UUID, name, subnet string) store.Network {
	t.Helper()
	n := store.Network{OrgID: org, Name: name, CreatedBy: user}
	if subnet != "" {
		n.Subnet = netip.MustParsePrefix(subnet)
	}
	out, err := s.CreateNetwork(context.Background(), n)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func TestCreateNetworkSubnets(t *testing.T) {
	ctx := context.Background()
	e := newVMEnv(t)
	other := mustCreateOrg(t, e.s, "B", e.user)

	n1 := mustCreateNetwork(t, e.s, e.org.ID, e.user, "one", "")
	n2 := mustCreateNetwork(t, e.s, other.ID, e.user, "two", "")
	if n1.Subnet != netip.MustParsePrefix("10.210.0.0/24") || n2.Subnet != netip.MustParsePrefix("10.210.1.0/24") {
		t.Fatalf("subnets %s %s", n1.Subnet, n2.Subnet)
	}
	mustCreateNetwork(t, e.s, e.org.ID, e.user, "custom", "172.20.0.0/24")
	if _, err := e.s.CreateNetwork(ctx, store.Network{OrgID: other.ID, Name: "x", Subnet: netip.MustParsePrefix("172.20.0.128/25"), CreatedBy: e.user}); !errors.Is(err, store.ErrSubnetOverlap) {
		t.Fatalf("overlap: %v", err)
	}
	if _, err := e.s.CreateNetwork(ctx, store.Network{OrgID: e.org.ID, Name: "one", CreatedBy: e.user}); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("duplicate name: %v", err)
	}
	mustCreateNetwork(t, e.s, other.ID, e.user, "one", "")
	if _, err := e.s.CreateNetwork(ctx, store.Network{OrgID: e.org.ID, Name: "bad", Subnet: netip.MustParsePrefix("10.64.0.0/24"), CreatedBy: e.user}); !errors.Is(err, netaddr.ErrInvalidSubnet) {
		t.Fatalf("reserved subnet: %v", err)
	}

	got, err := e.s.Network(ctx, e.org.ID, n1.ID)
	if err != nil || got.Subnet != n1.Subnet || got.Name != "one" || got.CreatedBy != e.user {
		t.Fatalf("reload %+v %v", got, err)
	}
	if _, err := e.s.Network(ctx, other.ID, n1.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("foreign org: %v", err)
	}
	list, err := e.s.Networks(ctx, e.org.ID)
	if err != nil || len(list) != 2 {
		t.Fatalf("networks %+v %v", list, err)
	}
}

func TestSetVMNetworksRestart(t *testing.T) {
	ctx := context.Background()
	e := newVMEnv(t)
	vm, err := e.s.CreateVM(ctx, e.vm("v", nil, 1, 1, 1))
	if err != nil {
		t.Fatal(err)
	}
	n := mustCreateNetwork(t, e.s, e.org.ID, e.user, "net", "")

	if _, err := e.s.SetVMNetworks(ctx, e.org.ID, vm.ID, []uuid.UUID{n.ID}, false); !errors.Is(err, store.ErrRestartRequired) {
		t.Fatalf("running without flag: %v", err)
	}
	for _, p := range []string{"paused", "hibernated"} {
		execSQL(t, e.pool, "UPDATE vms SET power = $2 WHERE id = $1", vm.ID, p)
		if _, err := e.s.SetVMNetworks(ctx, e.org.ID, vm.ID, []uuid.UUID{n.ID}, false); !errors.Is(err, store.ErrRestartRequired) {
			t.Fatalf("%s without flag: %v", p, err)
		}
	}
	execSQL(t, e.pool, "UPDATE vms SET power = 'running' WHERE id = $1", vm.ID)
	if nics, err := e.s.VMNetworks(ctx, vm.ID); err != nil || len(nics) != 0 {
		t.Fatalf("nics after refusal %+v %v", nics, err)
	}
	if got, _ := e.s.VM(ctx, e.org.ID, vm.ID); got.Generation != vm.Generation {
		t.Fatalf("generation bumped on refusal: %d", got.Generation)
	}

	updated, err := e.s.SetVMNetworks(ctx, e.org.ID, vm.ID, []uuid.UUID{n.ID, n.ID}, true)
	if err != nil {
		t.Fatal(err)
	}
	if updated.Generation != vm.Generation+1 {
		t.Fatalf("generation %d, want %d", updated.Generation, vm.Generation+1)
	}
	nics, err := e.s.VMNetworks(ctx, vm.ID)
	if err != nil || len(nics) != 1 || nics[0].NetworkID != n.ID || nics[0].IP != netip.MustParseAddr("10.210.0.10") {
		t.Fatalf("nics %+v %v", nics, err)
	}
	if got, _ := e.s.Network(ctx, e.org.ID, n.ID); got.VMCount != 1 {
		t.Fatalf("vm count %d", got.VMCount)
	}

	same, err := e.s.SetVMNetworks(ctx, e.org.ID, vm.ID, []uuid.UUID{n.ID}, false)
	if err != nil || same.Generation != updated.Generation {
		t.Fatalf("same set: gen %d %v", same.Generation, err)
	}

	if _, err := e.s.SetVMPower(ctx, e.org.ID, vm.ID, proto.PowerStopped, proto.StopHard); err != nil {
		t.Fatal(err)
	}
	n2 := mustCreateNetwork(t, e.s, e.org.ID, e.user, "net2", "")
	if _, err := e.s.SetVMNetworks(ctx, e.org.ID, vm.ID, []uuid.UUID{n.ID, n2.ID}, false); err != nil {
		t.Fatalf("stopped VM: %v", err)
	}
	nics, err = e.s.VMNetworks(ctx, vm.ID)
	if err != nil || len(nics) != 2 {
		t.Fatalf("nics %+v %v", nics, err)
	}
	for _, nic := range nics {
		if nic.NetworkID == n.ID && nic.IP != netip.MustParseAddr("10.210.0.10") {
			t.Fatalf("retained NIC changed IP: %s", nic.IP)
		}
	}

	foreignOrg := mustCreateOrg(t, e.s, "B", e.user)
	foreign := mustCreateNetwork(t, e.s, foreignOrg.ID, e.user, "f", "")
	if _, err := e.s.SetVMNetworks(ctx, e.org.ID, vm.ID, []uuid.UUID{foreign.ID}, true); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("foreign network: %v", err)
	}
}

func TestDeleteNetworkWithMembers(t *testing.T) {
	ctx := context.Background()
	e := newVMEnv(t)
	vm, err := e.s.CreateVM(ctx, e.vm("v", nil, 1, 1, 1))
	if err != nil {
		t.Fatal(err)
	}
	n := mustCreateNetwork(t, e.s, e.org.ID, e.user, "net", "")
	if _, err := e.s.SetVMNetworks(ctx, e.org.ID, vm.ID, []uuid.UUID{n.ID}, true); err != nil {
		t.Fatal(err)
	}
	if err := e.s.DeleteNetwork(ctx, e.org.ID, n.ID); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("attached: %v", err)
	}
	if _, err := e.s.SetVMNetworks(ctx, e.org.ID, vm.ID, nil, true); err != nil {
		t.Fatal(err)
	}
	if err := e.s.DeleteNetwork(ctx, e.org.ID, n.ID); err != nil {
		t.Fatal(err)
	}
	if err := e.s.DeleteNetwork(ctx, e.org.ID, n.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("second delete: %v", err)
	}
}

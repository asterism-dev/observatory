package network

import (
	"context"
	"log/slog"
	"net/netip"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/asterism-dev/observatory/internal/authz"
	"github.com/asterism-dev/observatory/internal/netaddr"
	"github.com/asterism-dev/observatory/internal/proto"
	"github.com/asterism-dev/observatory/internal/store"
)

// ChassisStatus is the gateway's last view of a node's chassis.
type ChassisStatus struct {
	IP            string
	Registered    bool
	LastHandshake *time.Time
}

type gatewayStatus struct {
	mu      sync.Mutex
	chassis map[uuid.UUID]ChassisStatus
}

// State builds the gateway's desired state, including its private keys; callers must never log it.
func (h *Hub) State(ctx context.Context) (proto.GatewayState, error) {
	snap, err := h.store.NetworkSnapshot(ctx)
	if err != nil {
		return proto.GatewayState{}, err
	}
	e := h.authz.Enforcer()
	st := proto.GatewayState{
		UnderlayKey: h.underlay.priv,
		ClientsKey:  h.clients.priv,
		SB:          proto.GatewaySB{CACert: h.ca.CertPEM(), Cert: h.sbCert, Key: h.sbKey},
		GuestMTU:    netaddr.GuestMTU,
		Chassis:     []proto.GatewayChassis{},
		Devices:     []proto.GatewayDevice{},
		Orgs:        []proto.GatewayOrg{},
	}
	for _, c := range snap.Chassis {
		st.Chassis = append(st.Chassis, proto.GatewayChassis{NodeID: c.NodeID.String(), OrgID: c.OrgID.String(), PublicKey: c.PublicKey, IP: c.IP.String()})
	}
	devices := map[uuid.UUID][]store.Device{}
	for _, d := range snap.Devices {
		st.Devices = append(st.Devices, proto.GatewayDevice{PublicKey: d.PublicKey, IP: d.IP.String(), OrgID: d.OrgID.String()})
		devices[d.OrgID] = append(devices[d.OrgID], d)
	}
	orgs := map[uuid.UUID]*proto.GatewayOrg{}
	for _, o := range snap.Orgs {
		st.Orgs = append(st.Orgs, proto.GatewayOrg{ID: o.ID.String(), Slice: o.Slice.String(), Ports: []proto.GatewayPort{}, Networks: []proto.GatewayNetwork{}, Records: map[string]string{}})
	}
	for i := range st.Orgs {
		orgs[snap.Orgs[i].ID] = &st.Orgs[i]
	}
	vms := map[uuid.UUID]store.VM{}
	for _, vm := range snap.VMs {
		o := orgs[vm.OrgID]
		if o == nil {
			continue
		}
		vms[vm.ID] = vm
		o.Ports = append(o.Ports, port(vm.ID.String()+"-mgmt", vm, vm.MgmtIP, allowFrom(e, vm, devices[vm.OrgID])))
		o.Records[strings.ToLower(vm.Name)+".internal"] = vm.MgmtIP.String()
	}
	type netRef struct {
		org *proto.GatewayOrg
		idx int
		net store.Network
	}
	nets := map[uuid.UUID]netRef{}
	for _, n := range snap.Networks {
		o := orgs[n.OrgID]
		if o == nil {
			continue
		}
		o.Networks = append(o.Networks, proto.GatewayNetwork{ID: n.ID.String(), Subnet: n.Subnet.String(), Ports: []proto.GatewayPort{}})
		nets[n.ID] = netRef{o, len(o.Networks) - 1, n}
	}
	for _, nic := range snap.NICs {
		vm, ok := vms[nic.VMID]
		ref, found := nets[nic.NetworkID]
		if !ok || !found || ref.net.OrgID != vm.OrgID {
			continue
		}
		gn := &ref.org.Networks[ref.idx]
		gn.Ports = append(gn.Ports, port(vm.ID.String()+"-"+nic.NetworkID.String(), vm, nic.IP, nil))
		ref.org.Records[strings.ToLower(vm.Name+"."+ref.net.Name)+".internal"] = nic.IP.String()
	}
	return st, nil
}

func port(name string, vm store.VM, ip netip.Addr, allow []string) proto.GatewayPort {
	return proto.GatewayPort{Name: name, MAC: netaddr.MAC(ip), IP: ip.String(), Chassis: vm.NodeID.String(), AllowFrom: allow}
}

// ponytail: devices × VMs CanConnect per poll; index devices by user if orgs grow large.
func allowFrom(e *authz.Enforcer, vm store.VM, devices []store.Device) []string {
	ref := authz.VMRef{OrgID: vm.OrgID, ID: vm.ID, UserID: vm.UserID, TeamID: vm.TeamID}
	var out []string
	for _, d := range devices {
		if d.OrgID == vm.OrgID && e.CanConnect(d.UserID, ref) {
			out = append(out, d.IP.String())
		}
	}
	slices.Sort(out)
	return out
}

// SetStatus stores device handshakes and keeps chassis registration and handshakes in memory.
func (h *Hub) SetStatus(ctx context.Context, st proto.GatewayStatus) error {
	if err := h.store.SetDeviceHandshakes(ctx, st.Handshakes); err != nil {
		return err
	}
	chassis, err := h.store.Chassis(ctx)
	if err != nil {
		return err
	}
	next := make(map[uuid.UUID]ChassisStatus, len(chassis))
	for _, c := range chassis {
		cs := ChassisStatus{IP: c.IP.String(), Registered: slices.Contains(st.Chassis, c.NodeID.String())}
		if t, ok := st.Handshakes[c.PublicKey]; ok && !t.IsZero() {
			cs.LastHandshake = &t
		}
		next[c.NodeID] = cs
	}
	if len(st.Rebound) > 0 {
		slog.Warn("gateway cleared chassis bindings that did not match requested-chassis", "ports", st.Rebound)
	}
	h.status.mu.Lock()
	h.status.chassis = next
	h.status.mu.Unlock()
	return nil
}

func (h *Hub) ChassisStatus(node uuid.UUID) (ChassisStatus, bool) {
	h.status.mu.Lock()
	defer h.status.mu.Unlock()
	cs, ok := h.status.chassis[node]
	return cs, ok
}

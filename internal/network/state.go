package network

import (
	"context"
	"log/slog"
	"maps"
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
	Endpoint      string
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
	// Revocations reach the allow lists on the next successful authz reload, which may trail this snapshot.
	guestMTU, err := h.store.GuestMTU(ctx)
	if err != nil {
		return proto.GatewayState{}, err
	}
	connect := h.authz.Enforcer().ConnectChecker()
	st := proto.GatewayState{
		UnderlayKey:    h.underlay.priv,
		ClientsKey:     h.clients.priv,
		SB:             proto.GatewaySB{CACert: h.ca.CertPEM(), Cert: h.sbCert, Key: h.sbKey},
		GuestMTU:       guestMTU,
		Chassis:        []proto.GatewayChassis{},
		Devices:        []proto.GatewayDevice{},
		Orgs:           []proto.GatewayOrg{},
		GatewayChassis: proto.GatewayChassisRef{Name: "hub-gw", IP: netaddr.GatewayUnderlay.String()},
	}
	for _, c := range snap.Chassis {
		if c.NodeID == h.cfg.GatewayNode {
			// Until the node enrolls its chassis there is no IP, so the gateway keeps its own.
			st.GatewayChassis = proto.GatewayChassisRef{Name: c.NodeID.String(), IP: c.IP.String()}
		}
		st.Chassis = append(st.Chassis, proto.GatewayChassis{NodeID: c.NodeID.String(), OrgID: c.OrgID.String(), PublicKey: c.PublicKey, IP: c.IP.String(), Quarantined: c.Quarantined})
	}
	devices := map[uuid.UUID]map[uuid.UUID][]store.Device{}
	for _, d := range snap.Devices {
		st.Devices = append(st.Devices, proto.GatewayDevice{PublicKey: d.PublicKey, IP: d.IP.String(), OrgID: d.OrgID.String()})
		if devices[d.OrgID] == nil {
			devices[d.OrgID] = map[uuid.UUID][]store.Device{}
		}
		devices[d.OrgID][d.UserID] = append(devices[d.OrgID][d.UserID], d)
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
		if vm.Kind == store.VMKindBuilder {
			// A builder holds real secrets: its port keeps it connected, but no device may reach it and it gets no name.
			o.Ports = append(o.Ports, port(MgmtPort(vm.ID), vm, vm.MgmtIP, nil))
			continue
		}
		vms[vm.ID] = vm
		o.Ports = append(o.Ports, port(MgmtPort(vm.ID), vm, vm.MgmtIP, allowFrom(connect, vm, devices[vm.OrgID])))
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
		gn.Ports = append(gn.Ports, port(NICPort(vm.ID, nic.NetworkID), vm, nic.IP, nil))
		ref.org.Records[strings.ToLower(vm.Name+"."+ref.net.Name)+".internal"] = nic.IP.String()
	}
	return st, nil
}

// MgmtPort and NICPort name a VM's logical switch ports; the node uses the same name as the tap's iface-id.
func MgmtPort(vm uuid.UUID) string { return vm.String() + "-mgmt" }

func NICPort(vm, network uuid.UUID) string { return vm.String() + "-" + network.String() }

func port(name string, vm store.VM, ip netip.Addr, allow []string) proto.GatewayPort {
	return proto.GatewayPort{Name: name, MAC: netaddr.MAC(ip), IP: ip.String(), Chassis: vm.NodeID.String(), AllowFrom: allow}
}

// allowFrom checks CanConnect once per user: a user VM only for its owner, a team VM for each user with a device in the org.
func allowFrom(c *authz.ConnectChecker, vm store.VM, byUser map[uuid.UUID][]store.Device) []string {
	ref := authz.VMRef{OrgID: vm.OrgID, ID: vm.ID, UserID: vm.UserID, TeamID: vm.TeamID}
	var out []string
	add := func(user uuid.UUID) {
		if c.CanConnect(user, ref) {
			for _, d := range byUser[user] {
				out = append(out, d.IP.String())
			}
		}
	}
	if vm.UserID != nil {
		add(*vm.UserID)
	} else {
		for user := range byUser {
			add(user)
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
	var ids, added []uuid.UUID
	for _, s := range st.Quarantined {
		if id, err := uuid.Parse(s); err == nil {
			ids = append(ids, id)
		}
	}
	if len(ids) > 0 {
		var err error
		if added, err = h.store.QuarantineNodes(ctx, ids); err != nil {
			return err
		}
		if len(added) > 0 {
			slog.Warn("gateway quarantined nodes that held another node's port; re-enroll to release", "nodes", added)
		}
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
		cs.Endpoint = st.Endpoints[c.PublicKey]
		next[c.NodeID] = cs
	}
	if len(st.Rebound) > 0 {
		slog.Warn("gateway cleared chassis bindings that did not match requested-chassis", "ports", st.Rebound)
	}
	changed := map[uuid.UUID]bool{}
	h.status.mu.Lock()
	for _, c := range chassis {
		if slices.Contains(added, c.NodeID) || next[c.NodeID].Endpoint != h.status.chassis[c.NodeID].Endpoint {
			changed[c.OrgID] = true
		}
	}
	h.status.chassis = next
	h.status.mu.Unlock()
	h.changed(slices.Collect(maps.Keys(changed))...)
	return nil
}

func (h *Hub) changed(orgs ...uuid.UUID) {
	if h.onChange == nil {
		return
	}
	for _, org := range orgs {
		h.onChange(org)
	}
}

// ChassisStatus is the node's chassis as of the gateway's last status report; false if not enrolled then.
func (h *Hub) ChassisStatus(node uuid.UUID) (ChassisStatus, bool) {
	h.status.mu.Lock()
	defer h.status.mu.Unlock()
	cs, ok := h.status.chassis[node]
	return cs, ok
}

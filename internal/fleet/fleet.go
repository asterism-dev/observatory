// Package fleet tracks connected nodes and pushes each its desired VM list, always rebuilt from Postgres.
package fleet

import (
	"context"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"

	"github.com/asterism-dev/observatory/internal/authz"
	"github.com/asterism-dev/observatory/internal/netaddr"
	"github.com/asterism-dev/observatory/internal/network"
	"github.com/asterism-dev/observatory/internal/proto"
	"github.com/asterism-dev/observatory/internal/store"
)

type Fleet struct {
	store *store.Store
	authz *authz.Service
	net   *network.Hub

	// seq numbers every desired list hub-wide; starting at the clock (or above any recorded release_seq) keeps it increasing across hub restarts.
	seq atomic.Uint64

	mu       sync.Mutex
	conns    map[uuid.UUID]*conn
	diskErrs map[uuid.UUID]map[string]string // VM → disk ID → error, from the latest status
}

func New(ctx context.Context, s *store.Store, z *authz.Service, n *network.Hub) (*Fleet, error) {
	maxSeq, err := s.MaxReleaseSeq(ctx)
	if err != nil {
		return nil, err
	}
	f := &Fleet{store: s, authz: z, net: n, conns: map[uuid.UUID]*conn{}, diskErrs: map[uuid.UUID]map[string]string{}}
	f.seq.Store(max(uint64(time.Now().UnixNano()), maxSeq+1))
	return f, nil
}

// DiskErrors returns the VM's attachment errors by disk ID as its node last reported them; callers must not modify it.
func (f *Fleet) DiskErrors(vm uuid.UUID) map[string]string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.diskErrs[vm]
}

// noteDiskErrors keeps the disk errors of st, but only for a VM in c's last desired list.
func (f *Fleet) noteDiskErrors(c *conn, st proto.VMStatus) {
	id, err := uuid.Parse(st.ID)
	errs := map[string]string{}
	for _, d := range st.Disks {
		if d.Error != "" {
			errs[d.Name] = d.Error
		}
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	switch {
	case err != nil || !c.vms[st.ID]:
	case len(errs) == 0:
		delete(f.diskErrs, id)
	default:
		f.diskErrs[id] = errs
	}
}

func (f *Fleet) Online(node uuid.UUID) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.conns[node] != nil
}

// NotifyOrg schedules a fresh desired list for every connected node of the org; it never waits on a node.
func (f *Fleet) NotifyOrg(org uuid.UUID) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, c := range f.conns {
		if c.org == org {
			c.kick()
		}
	}
}

func (f *Fleet) Disconnect(node uuid.UUID) {
	f.mu.Lock()
	c := f.conns[node]
	delete(f.conns, node)
	f.mu.Unlock()
	if c != nil {
		c.sess.Close()
	}
}

// attach makes c the node's live connection, closing the one it replaces.
func (f *Fleet) attach(c *conn) {
	f.mu.Lock()
	old := f.conns[c.node]
	f.conns[c.node] = c
	f.mu.Unlock()
	if old != nil {
		old.sess.Close()
	}
}

func (f *Fleet) detach(c *conn) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.conns[c.node] == c {
		delete(f.conns, c.node)
	}
}

// desired builds the node's list and returns the releasing disks it no longer carries.
func (f *Fleet) desired(ctx context.Context, org, node uuid.UUID) ([]proto.DesiredVM, []uuid.UUID, error) {
	// Read first: a detach committed after this read is still in the list below, so its export is never marked by it.
	releasing, err := f.store.ReleasingDisks(ctx, node)
	if err != nil {
		return nil, nil, err
	}
	vms, err := f.store.VMsOnNode(ctx, node)
	if err != nil {
		return nil, nil, err
	}
	atts, err := f.store.NodeAttachments(ctx, node)
	if err != nil {
		return nil, nil, err
	}
	nets, err := f.store.Networks(ctx, org)
	if err != nil {
		return nil, nil, err
	}
	rank := make(map[uuid.UUID]int, len(nets))
	for i, n := range nets {
		rank[n.ID] = i
	}
	out := make([]proto.DesiredVM, 0, len(vms))
	for _, v := range vms {
		boot, err := f.boot(ctx, org, v)
		if err != nil {
			return nil, nil, err
		}
		nics, err := f.nics(ctx, v, rank)
		if err != nil {
			return nil, nil, err
		}
		out = append(out, proto.DesiredVM{
			Spec:  proto.VMSpec{ID: v.ID.String(), Name: v.Name, CPUs: v.CPUs, MemoryMB: v.MemoryMB, DiskGB: v.DiskGB, NICs: nics, Attachments: atts[v.ID]},
			Power: v.Power, StopMode: v.StopMode, Generation: v.Generation, Provisioned: v.Provisioned, Boot: boot,
		})
	}
	return out, releasing, nil
}

// nics lists the management NIC, then network NICs by network name (rank); names and MACs match the gateway's ports.
func (f *Fleet) nics(ctx context.Context, v store.VM, rank map[uuid.UUID]int) ([]proto.NIC, error) {
	if !v.MgmtIP.IsValid() {
		return []proto.NIC{}, nil
	}
	attached, err := f.store.VMNetworks(ctx, v.ID)
	if err != nil {
		return nil, err
	}
	// A network missing from rank was created after the list was read; the next push picks it up.
	attached = slices.DeleteFunc(attached, func(n store.VMNIC) bool { _, ok := rank[n.NetworkID]; return !ok })
	slices.SortFunc(attached, func(a, b store.VMNIC) int { return rank[a.NetworkID] - rank[b.NetworkID] })
	out := []proto.NIC{{Network: "mgmt", MAC: netaddr.MAC(v.MgmtIP), IP: v.MgmtIP.String(), Port: network.MgmtPort(v.ID)}}
	for _, n := range attached {
		out = append(out, proto.NIC{Network: n.NetworkID.String(), MAC: netaddr.MAC(n.IP), IP: n.IP.String(), Port: network.NICPort(v.ID, n.NetworkID)})
	}
	return out, nil
}

// ponytail: keys and env resolved per VM; cache per owner if nodes carry many VMs of one owner.
func (f *Fleet) boot(ctx context.Context, org uuid.UUID, v store.VM) (*proto.Boot, error) {
	var owner string
	var users []uuid.UUID
	switch {
	case v.UserID != nil:
		owner, users = authz.UserSubject(*v.UserID), []uuid.UUID{*v.UserID}
	case v.TeamID != nil:
		owner = authz.TeamSubject(*v.TeamID)
		members, err := f.store.TeamMembers(ctx, org, *v.TeamID)
		if err != nil {
			return nil, err
		}
		for _, m := range members {
			users = append(users, m.User.ID)
		}
	}
	keys, err := f.store.SSHKeysOf(ctx, users)
	if err != nil {
		return nil, err
	}
	env, err := f.store.ResolveEnv(ctx, f.authz.Enforcer(), org, owner)
	if err != nil {
		return nil, err
	}
	if keys == nil {
		keys = []string{}
	}
	return &proto.Boot{SSHPublicKeys: keys, Env: env}, nil
}

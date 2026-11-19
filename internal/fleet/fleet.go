// Package fleet tracks connected nodes and pushes each its desired VM list, always rebuilt from Postgres.
package fleet

import (
	"context"
	"sync"

	"github.com/google/uuid"

	"github.com/asterism-dev/observatory/internal/authz"
	"github.com/asterism-dev/observatory/internal/proto"
	"github.com/asterism-dev/observatory/internal/store"
)

type Fleet struct {
	store *store.Store
	authz *authz.Service

	mu    sync.Mutex
	conns map[uuid.UUID]*conn
}

func New(s *store.Store, z *authz.Service) *Fleet {
	return &Fleet{store: s, authz: z, conns: map[uuid.UUID]*conn{}}
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

func (f *Fleet) desired(ctx context.Context, org, node uuid.UUID) ([]proto.DesiredVM, error) {
	vms, err := f.store.VMsOnNode(ctx, node)
	if err != nil {
		return nil, err
	}
	out := make([]proto.DesiredVM, 0, len(vms))
	for _, v := range vms {
		boot, err := f.boot(ctx, org, v)
		if err != nil {
			return nil, err
		}
		out = append(out, proto.DesiredVM{
			Spec:  proto.VMSpec{ID: v.ID.String(), Name: v.Name, CPUs: v.CPUs, MemoryMB: v.MemoryMB, DiskGB: v.DiskGB, NICs: []proto.NIC{}},
			Power: v.Power, StopMode: v.StopMode, Generation: v.Generation, Provisioned: v.Provisioned, Boot: boot,
		})
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

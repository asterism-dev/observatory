package iam

import (
	"cmp"
	"context"
	"errors"
	"slices"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/types/known/timestamppb"

	iamv1 "github.com/asterism-dev/observatory/gen/go/observatory/iam/v1"
	"github.com/asterism-dev/observatory/internal/authz"
	"github.com/asterism-dev/observatory/internal/store"
)

func toOrgInfo(o store.Org, role store.Role) *iamv1.OrgInfo {
	return &iamv1.OrgInfo{Id: o.ID.String(), Name: o.Name, Personal: o.Personal, Role: string(role)}
}

func (s *server) GetMe(ctx context.Context, req *connect.Request[iamv1.GetMeRequest]) (*connect.Response[iamv1.GetMeResponse], error) {
	p := caller(ctx)
	u, err := s.Store.User(ctx, p.User)
	if errors.Is(err, store.ErrNotFound) {
		return nil, connect.NewError(connect.CodeUnauthenticated, errors.New("unauthorized"))
	}
	if err != nil {
		return nil, internal("user", err)
	}
	e := s.Policy.Enforcer()
	var orgs []*iamv1.OrgInfo
	for _, id := range e.OrgsOf(p.User) {
		role, ok := e.Role(id, p.User)
		if !ok {
			continue
		}
		o, err := s.Store.Org(ctx, id)
		// Deleted since the snapshot; the next reload drops it.
		if errors.Is(err, store.ErrNotFound) {
			continue
		}
		if err != nil {
			return nil, internal("org", err)
		}
		orgs = append(orgs, toOrgInfo(o, store.Role(role)))
	}
	// Personal org first, then by name, as 0.x listed them.
	slices.SortFunc(orgs, func(a, b *iamv1.OrgInfo) int {
		if a.Personal != b.Personal {
			if a.Personal {
				return -1
			}
			return 1
		}
		return cmp.Compare(a.Name, b.Name)
	})
	return connect.NewResponse(&iamv1.GetMeResponse{
		User: &iamv1.UserInfo{Id: u.ID.String(), Email: u.Email, Name: u.Name, PlatformAdmin: p.PlatformAdmin, CreatedAt: timestamppb.New(u.CreatedAt)},
		Orgs: orgs,
	}), nil
}

func (s *server) ListSessions(ctx context.Context, req *connect.Request[iamv1.ListSessionsRequest]) (*connect.Response[iamv1.ListSessionsResponse], error) {
	p := caller(ctx)
	ss, err := s.Store.Sessions(ctx, p.User)
	if err != nil {
		return nil, internal("sessions", err)
	}
	out := make([]*iamv1.Session, 0, len(ss))
	for _, x := range ss {
		out = append(out, &iamv1.Session{Id: x.ID.String(), Kind: string(x.Kind), UserAgent: x.UserAgent, Ip: x.IP,
			CreatedAt: timestamppb.New(x.CreatedAt), LastUsedAt: timestamppb.New(x.LastUsedAt), ExpiresAt: timestamppb.New(x.ExpiresAt),
			Current: x.ID == p.Session})
	}
	return connect.NewResponse(&iamv1.ListSessionsResponse{Sessions: out}), nil
}

func (s *server) RevokeSession(ctx context.Context, req *connect.Request[iamv1.RevokeSessionRequest]) (*connect.Response[iamv1.RevokeSessionResponse], error) {
	id, err := parseID("session", req.Msg.Session)
	if err != nil {
		return nil, err
	}
	user := caller(ctx).User
	err = s.Store.InTx(ctx, func(tx *store.Store) error {
		if err := tx.RevokeSession(ctx, user, id); err != nil {
			return err
		}
		return tx.Audit(ctx, auditEntry(ctx, req.Header(), nil, store.ActionSessionRevoke, "session/"+id.String(), store.ResultAllowed))
	})
	if err != nil {
		return nil, toConnect("revoke session", err)
	}
	return connect.NewResponse(&iamv1.RevokeSessionResponse{}), nil
}

func toSSHKeyInfo(k store.SSHKey) *iamv1.SSHKeyInfo {
	return &iamv1.SSHKeyInfo{Id: k.ID.String(), Name: k.Name, Fingerprint: k.Fingerprint, CreatedAt: timestamppb.New(k.CreatedAt)}
}

func sshKeyAudit(ctx context.Context, req connect.AnyRequest, action string, k store.SSHKey) store.AuditEntry {
	e := auditEntry(ctx, req.Header(), nil, action, authz.UserSubject(k.UserID)+"/ssh-keys/"+k.ID.String(), store.ResultAllowed)
	e.Details = map[string]any{"name": k.Name, "fingerprint": k.Fingerprint}
	return e
}

func (s *server) ListSSHKeys(ctx context.Context, req *connect.Request[iamv1.IAMServiceListSSHKeysRequest]) (*connect.Response[iamv1.IAMServiceListSSHKeysResponse], error) {
	ks, err := s.Store.SSHKeys(ctx, caller(ctx).User)
	if err != nil {
		return nil, internal("ssh keys", err)
	}
	out := make([]*iamv1.SSHKeyInfo, 0, len(ks))
	for _, k := range ks {
		out = append(out, toSSHKeyInfo(k))
	}
	return connect.NewResponse(&iamv1.IAMServiceListSSHKeysResponse{Keys: out}), nil
}

func (s *server) CreateSSHKey(ctx context.Context, req *connect.Request[iamv1.CreateSSHKeyRequest]) (*connect.Response[iamv1.CreateSSHKeyResponse], error) {
	name, err := validName(req.Msg.Name)
	if err != nil {
		return nil, err
	}
	user := caller(ctx).User
	k := store.SSHKey{UserID: user, Name: name, PublicKey: req.Msg.PublicKey}
	if err := s.commit(ctx, func(tx *store.Store) error {
		var err error
		if k, err = tx.AddSSHKey(ctx, k); err != nil {
			return err
		}
		return tx.Audit(ctx, sshKeyAudit(ctx, req, store.ActionSSHKeyAdd, k))
	}); err != nil {
		return nil, err
	}
	return connect.NewResponse(&iamv1.CreateSSHKeyResponse{Key: toSSHKeyInfo(k)}), nil
}

func (s *server) DeleteSSHKey(ctx context.Context, req *connect.Request[iamv1.DeleteSSHKeyRequest]) (*connect.Response[iamv1.DeleteSSHKeyResponse], error) {
	id, err := parseID("key", req.Msg.Key)
	if err != nil {
		return nil, err
	}
	user := caller(ctx).User
	if err := s.commit(ctx, func(tx *store.Store) error {
		ks, err := tx.SSHKeys(ctx, user)
		if err != nil {
			return err
		}
		for _, k := range ks {
			if k.ID == id {
				if err := tx.DeleteSSHKey(ctx, user, id); err != nil {
					return err
				}
				return tx.Audit(ctx, sshKeyAudit(ctx, req, store.ActionSSHKeyDelete, k))
			}
		}
		return store.ErrNotFound
	}); err != nil {
		return nil, err
	}
	return connect.NewResponse(&iamv1.DeleteSSHKeyResponse{}), nil
}

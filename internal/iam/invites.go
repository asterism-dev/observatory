package iam

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/types/known/timestamppb"

	iamv1 "github.com/asterism-dev/observatory/gen/go/observatory/iam/v1"
	"github.com/asterism-dev/observatory/internal/authz"
	"github.com/asterism-dev/observatory/internal/iam/store"
)

const inviteTTL = 7 * 24 * time.Hour

func toInvite(i store.Invite) *iamv1.Invite {
	return &iamv1.Invite{Id: i.ID.String(), Role: string(i.Role), CreatedBy: i.CreatedBy.String(),
		CreatedAt: timestamppb.New(i.CreatedAt), ExpiresAt: timestamppb.New(i.ExpiresAt)}
}

func (s *server) ListInvites(ctx context.Context, req *connect.Request[iamv1.ListInvitesRequest]) (*connect.Response[iamv1.ListInvitesResponse], error) {
	org, err := parseID("org", req.Msg.Org)
	if err != nil {
		return nil, err
	}
	invs, err := s.Store.Invites(ctx, org)
	if err != nil {
		return nil, internal("invites", err)
	}
	out := make([]*iamv1.Invite, 0, len(invs))
	for _, i := range invs {
		out = append(out, toInvite(i))
	}
	return connect.NewResponse(&iamv1.ListInvitesResponse{Invites: out}), nil
}

func (s *server) CreateInvite(ctx context.Context, req *connect.Request[iamv1.CreateInviteRequest]) (*connect.Response[iamv1.CreateInviteResponse], error) {
	org, err := parseID("org", req.Msg.Org)
	if err != nil {
		return nil, err
	}
	role, err := validRole(req.Msg.Role)
	if err != nil {
		return nil, err
	}
	user := caller(ctx).User
	if privileged(role) && !s.Policy.Enforcer().Allowed(authz.UserSubject(user), org, authz.ObjMembers, store.ActionMemberSetRole) {
		s.auditDenied(ctx, auditEntry(ctx, req.Header(), &org, store.ActionMemberSetRole, authz.ObjMembers, store.ResultDenied), nil)
		return nil, connect.NewError(connect.CodePermissionDenied, errForbidden)
	}
	var (
		token string
		inv   store.Invite
	)
	if err := s.commit(ctx, func(tx *store.Store) error {
		var err error
		if token, inv, err = tx.CreateInvite(ctx, org, role, user, inviteTTL); err != nil {
			return err
		}
		e := auditEntry(ctx, req.Header(), &org, store.ActionMemberInvite, "org/invites/"+inv.ID.String(), store.ResultAllowed)
		e.Details = map[string]any{"role": role}
		return tx.Audit(ctx, e)
	}); err != nil {
		return nil, err
	}
	return connect.NewResponse(&iamv1.CreateInviteResponse{Invite: toInvite(inv), Token: token, Url: s.PublicURL.JoinPath("invite", token).String()}), nil
}

func (s *server) DeleteInvite(ctx context.Context, req *connect.Request[iamv1.DeleteInviteRequest]) (*connect.Response[iamv1.DeleteInviteResponse], error) {
	org, err := parseID("org", req.Msg.Org)
	if err != nil {
		return nil, err
	}
	id, err := parseID("invite", req.Msg.Invite)
	if err != nil {
		return nil, err
	}
	if err := s.commit(ctx, func(tx *store.Store) error {
		if err := tx.RevokeInvite(ctx, org, id); err != nil {
			return err
		}
		return tx.Audit(ctx, auditEntry(ctx, req.Header(), &org, store.ActionInviteRevoke, "org/invites/"+id.String(), store.ResultAllowed))
	}); err != nil {
		return nil, err
	}
	return connect.NewResponse(&iamv1.DeleteInviteResponse{}), nil
}

func (s *server) AcceptInvite(ctx context.Context, req *connect.Request[iamv1.AcceptInviteRequest]) (*connect.Response[iamv1.AcceptInviteResponse], error) {
	user := caller(ctx).User
	var (
		org       store.Org
		inv       store.Invite
		acceptErr error
	)
	if err := s.commit(ctx, func(tx *store.Store) error {
		if org, inv, acceptErr = tx.AcceptInvite(ctx, req.Msg.Token, user); acceptErr != nil {
			return acceptErr
		}
		if acceptErr = checkInviter(ctx, tx, inv); acceptErr != nil {
			return acceptErr
		}
		e := auditEntry(ctx, req.Header(), &org.ID, store.ActionInviteAccept, authz.ObjMembers, store.ResultAllowed)
		e.Details = map[string]any{"role": inv.Role}
		return tx.Audit(ctx, e)
	}); err != nil {
		// The rejected attempt is audited outside the rolled-back transaction; the token never goes into the log.
		if errors.Is(acceptErr, store.ErrInviteInvalid) || errors.Is(acceptErr, store.ErrAlreadyMember) {
			e := auditEntry(ctx, req.Header(), nil, store.ActionInviteAccept, authz.ObjMembers, store.ResultFailed)
			e.Details = map[string]any{"reason": acceptErr.Error()}
			if err := s.Store.Audit(ctx, e); err != nil {
				slog.Error("audit failed invite accept", "user", user, "err", err)
			}
		}
		return nil, err
	}
	return connect.NewResponse(&iamv1.AcceptInviteResponse{OrgId: org.ID.String(), Role: string(inv.Role)}), nil
}

// checkInviter rejects an invite whose creator could no longer issue it, so lost rights can't be regained through a kept link.
func checkInviter(ctx context.Context, tx *store.Store, inv store.Invite) error {
	errInviter := fmt.Errorf("%w: inviter lacks permission", store.ErrInviteInvalid)
	sub := authz.UserSubject(inv.CreatedBy)
	// The share lock makes a concurrent removal or role change of the creator wait for this accept.
	if err := tx.LockSubject(ctx, inv.OrgID, sub); errors.Is(err, store.ErrNotMember) {
		return errInviter
	} else if err != nil {
		return err
	}
	e, err := policyEnforcer(ctx, tx)
	if err != nil {
		return err
	}
	if !e.Allowed(sub, inv.OrgID, authz.ObjMembers, "iam.invites.create") ||
		privileged(inv.Role) && !e.Allowed(sub, inv.OrgID, authz.ObjMembers, store.ActionMemberSetRole) {
		return errInviter
	}
	return nil
}

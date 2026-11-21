package iam

import (
	"context"
	"errors"

	"connectrpc.com/connect"
	"github.com/google/uuid"
	"google.golang.org/protobuf/types/known/timestamppb"

	hubv1 "github.com/asterism-dev/observatory/gen/go/observatory/hub/v1"
	iamv1 "github.com/asterism-dev/observatory/gen/go/observatory/iam/v1"
	"github.com/asterism-dev/observatory/internal/authz"
	"github.com/asterism-dev/observatory/internal/store"
)

func (s *server) ListMembers(ctx context.Context, req *connect.Request[iamv1.ListMembersRequest]) (*connect.Response[iamv1.ListMembersResponse], error) {
	org, err := parseID("org", req.Msg.Org)
	if err != nil {
		return nil, err
	}
	ms, err := s.Store.Members(ctx, org)
	if err != nil {
		return nil, internal("members", err)
	}
	out := make([]*iamv1.Member, 0, len(ms))
	for _, m := range ms {
		out = append(out, &iamv1.Member{Id: m.User.ID.String(), Email: m.User.Email, Name: m.User.Name, Role: string(m.Role), JoinedAt: timestamppb.New(m.JoinedAt)})
	}
	return connect.NewResponse(&iamv1.ListMembersResponse{Members: out}), nil
}

func (s *server) UpdateMember(ctx context.Context, req *connect.Request[iamv1.UpdateMemberRequest]) (*connect.Response[iamv1.UpdateMemberResponse], error) {
	org, err := parseID("org", req.Msg.Org)
	if err != nil {
		return nil, err
	}
	target, err := parseID("user", req.Msg.User)
	if err != nil {
		return nil, err
	}
	role, err := validRole(req.Msg.Role)
	if err != nil {
		return nil, err
	}
	if err := s.commit(ctx, func(tx *store.Store) error {
		if err := tx.SetMemberRole(ctx, org, target, role); err != nil {
			return err
		}
		e := auditEntry(ctx, req.Header(), &org, store.ActionMemberSetRole, "org/members/"+target.String(), store.ResultAllowed)
		e.Details = map[string]any{"role": role}
		return tx.Audit(ctx, e)
	}); err != nil {
		return nil, err
	}
	s.Notify(org)
	return connect.NewResponse(&iamv1.UpdateMemberResponse{}), nil
}

func (s *server) DeleteMember(ctx context.Context, req *connect.Request[iamv1.DeleteMemberRequest]) (*connect.Response[iamv1.DeleteMemberResponse], error) {
	org, err := parseID("org", req.Msg.Org)
	if err != nil {
		return nil, err
	}
	target, err := parseID("user", req.Msg.User)
	if err != nil {
		return nil, err
	}
	if target == caller(ctx).User {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("use LeaveOrg"))
	}
	if err := s.removeMember(ctx, req, org, target); err != nil {
		return nil, err
	}
	return connect.NewResponse(&iamv1.DeleteMemberResponse{}), nil
}

// LeaveOrg needs membership only: no org rule, not even a deny, keeps a member in.
func (s *server) LeaveOrg(ctx context.Context, req *connect.Request[iamv1.LeaveOrgRequest]) (*connect.Response[iamv1.LeaveOrgResponse], error) {
	org, err := parseID("org_id", req.Msg.OrgId)
	if err != nil {
		return nil, err
	}
	if _, err := s.callerRole(ctx, org); err != nil {
		return nil, err
	}
	if err := s.removeMember(ctx, req, org, caller(ctx).User); err != nil {
		return nil, err
	}
	return connect.NewResponse(&iamv1.LeaveOrgResponse{}), nil
}

// removeMember removes target from org; the caller leaves when target is the caller.
func (s *server) removeMember(ctx context.Context, req connect.AnyRequest, org, target uuid.UUID) error {
	actor := caller(ctx).User
	leaving := target == actor
	action := store.ActionMemberLeave
	if !leaving {
		action = store.ActionMemberRemove
	}
	if err := s.ownsNothing(ctx, org, authz.UserSubject(target)); err != nil {
		return err
	}
	resource := "org/members/" + target.String()
	var guard store.AuditEntry
	var guardErr error
	err := s.commit(ctx, func(tx *store.Store) error {
		callerRole, e, err := lockedPolicy(ctx, tx, org)
		if err != nil {
			return err
		}
		// The target's role is only stable under the lock, so the extra check for admins/owners happens here.
		current, err := tx.LockMember(ctx, org, target)
		if err != nil {
			return err
		}
		if !leaving && privileged(current) && !e.Allowed(authz.UserSubject(actor), org, authz.ObjMembers, store.ActionMemberSetRole) {
			guard, guardErr = auditEntry(ctx, req.Header(), &org, store.ActionMemberSetRole, authz.ObjMembers, store.ResultDenied), nil
			return errForbidden
		}
		// Removal drops the target's bindings and teams, lifting their denies, which only owners and admins may do.
		if !leaving && !privileged(callerRole) {
			ds, err := e.DenyRulesFor(authz.UserSubject(target), org)
			if err != nil {
				return err
			}
			if len(ds) > 0 {
				guardErr = denyRuleError{authz.ErrDenyNotAllowed, ds[0]}
				guard = auditEntry(ctx, req.Header(), &org, action, resource, store.ResultDenied)
				return guardErr
			}
		}
		if err := tx.RemoveMember(ctx, org, target); err != nil {
			return err
		}
		// Revoked here, not on next use, so an idle token cannot revive when its creator is re-added.
		toks, err := tx.RevokeTelemetryTokensBy(ctx, org, target)
		if err != nil {
			return err
		}
		for _, t := range toks {
			if err := tx.Audit(ctx, lostTokenAudit(t, "creator left the organization")); err != nil {
				return err
			}
		}
		return tx.Audit(ctx, auditEntry(ctx, req.Header(), &org, action, resource, store.ResultAllowed))
	})
	if err != nil {
		if guard.Action != "" {
			s.auditDenied(ctx, guard, guardErr)
		}
		return err
	}
	s.Notify(org)
	return nil
}

// ownsNothing refuses while subject owns VMs or live disks in org; ponytail: checked before and not under the org lock, so a concurrent create can orphan a resource.
func (s *server) ownsNothing(ctx context.Context, org uuid.UUID, subject string) error {
	res, err := s.Resources.CheckOwnership(ctx, connect.NewRequest(&hubv1.CheckOwnershipRequest{OrgId: org.String(), Subject: subject}))
	if err != nil {
		return internal("check ownership", err)
	}
	if res.Msg.OwnsVms || res.Msg.OwnsDisks {
		return connect.NewError(connect.CodeFailedPrecondition, store.ErrOwnsResources)
	}
	return nil
}

func lostTokenAudit(tok store.TelemetryToken, reason string) store.AuditEntry {
	return store.AuditEntry{ActorKind: store.ActorSystem, OrgID: &tok.OrgID, Action: store.ActionGrafanaTokenRevoke,
		Resource: "telemetry-token/" + tok.ID.String(), Result: store.ResultAllowed,
		Details: map[string]any{"name": tok.Name, "reason": reason}}
}

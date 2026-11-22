package iam

import (
	"context"
	"errors"
	"fmt"
	"slices"

	"connectrpc.com/connect"
	"github.com/google/uuid"
	"google.golang.org/protobuf/types/known/timestamppb"

	iamv1 "github.com/asterism-dev/observatory/gen/go/observatory/iam/v1"
	"github.com/asterism-dev/observatory/internal/authz"
	"github.com/asterism-dev/observatory/internal/iam/store"
)

func toTeamInfo(t store.Team) *iamv1.TeamInfo {
	return &iamv1.TeamInfo{Id: t.ID.String(), Name: t.Name, MemberCount: int32(t.MemberCount), CreatedAt: timestamppb.New(t.CreatedAt)}
}

// teamName refuses the qualifiers of image refs, which a team name would shadow.
func teamName(s string) (string, error) {
	name, err := validName(s)
	if err == nil && slices.Contains([]string{"me", "org", "observatory"}, name) {
		return "", connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("%q is reserved for image references", name))
	}
	return name, err
}

// orgTeam parses the org and team fields of a team request.
func orgTeam(org, team string) (uuid.UUID, uuid.UUID, error) {
	o, err := parseID("org", org)
	if err != nil {
		return uuid.Nil, uuid.Nil, err
	}
	t, err := parseID("team", team)
	return o, t, err
}

func (s *server) ListTeams(ctx context.Context, req *connect.Request[iamv1.IAMServiceListTeamsRequest]) (*connect.Response[iamv1.IAMServiceListTeamsResponse], error) {
	org, err := parseID("org", req.Msg.Org)
	if err != nil {
		return nil, err
	}
	ts, err := s.Store.Teams(ctx, org)
	if err != nil {
		return nil, internal("teams", err)
	}
	out := make([]*iamv1.TeamInfo, 0, len(ts))
	for _, t := range ts {
		out = append(out, toTeamInfo(t))
	}
	return connect.NewResponse(&iamv1.IAMServiceListTeamsResponse{Teams: out}), nil
}

func (s *server) GetTeam(ctx context.Context, req *connect.Request[iamv1.GetTeamRequest]) (*connect.Response[iamv1.GetTeamResponse], error) {
	org, id, err := orgTeam(req.Msg.Org, req.Msg.Team)
	if err != nil {
		return nil, err
	}
	team, err := s.Store.Team(ctx, org, id)
	if err != nil {
		return nil, toConnect("team", err)
	}
	members, err := s.Store.TeamMembers(ctx, org, id)
	if err != nil {
		return nil, internal("team members", err)
	}
	ms := make([]*iamv1.TeamMembership, 0, len(members))
	for _, tm := range members {
		ms = append(ms, &iamv1.TeamMembership{Id: tm.User.ID.String(), Email: tm.User.Email, Name: tm.User.Name, Admin: tm.Admin})
	}
	e, sub, obj := s.Policy.Enforcer(), authz.UserSubject(caller(ctx).User), authz.TeamObject(id)
	return connect.NewResponse(&iamv1.GetTeamResponse{
		Team: toTeamInfo(team), Members: ms,
		CanManageMembers: e.Allowed(sub, org, obj, "iam.teams.manageMembers"),
		CanUpdate:        e.Allowed(sub, org, obj, "iam.teams.update"),
		CanDelete:        e.Allowed(sub, org, obj, "iam.teams.delete"),
		CanLeave:         e.Allowed(sub, org, obj, "iam.teams.leave"),
	}), nil
}

func (s *server) CreateTeam(ctx context.Context, req *connect.Request[iamv1.CreateTeamRequest]) (*connect.Response[iamv1.CreateTeamResponse], error) {
	org, err := parseID("org", req.Msg.Org)
	if err != nil {
		return nil, err
	}
	name, err := teamName(req.Msg.Name)
	if err != nil {
		return nil, err
	}
	var team store.Team
	if err := s.commit(ctx, func(tx *store.Store) error {
		var err error
		if team, err = tx.CreateTeam(ctx, org, name); err != nil {
			return err
		}
		e := auditEntry(ctx, req.Header(), &org, store.ActionTeamCreate, authz.TeamObject(team.ID), store.ResultAllowed)
		e.Details = map[string]any{"name": name}
		return tx.Audit(ctx, e)
	}); err != nil {
		return nil, err
	}
	return connect.NewResponse(&iamv1.CreateTeamResponse{Team: toTeamInfo(team)}), nil
}

func (s *server) UpdateTeam(ctx context.Context, req *connect.Request[iamv1.UpdateTeamRequest]) (*connect.Response[iamv1.UpdateTeamResponse], error) {
	org, id, err := orgTeam(req.Msg.Org, req.Msg.Team)
	if err != nil {
		return nil, err
	}
	name, err := teamName(req.Msg.Name)
	if err != nil {
		return nil, err
	}
	var team store.Team
	if err := s.commit(ctx, func(tx *store.Store) error {
		if err := tx.RenameTeam(ctx, org, id, name); err != nil {
			return err
		}
		var err error
		if team, err = tx.Team(ctx, org, id); err != nil {
			return err
		}
		e := auditEntry(ctx, req.Header(), &org, store.ActionTeamUpdate, authz.TeamObject(id), store.ResultAllowed)
		e.Details = map[string]any{"name": name}
		return tx.Audit(ctx, e)
	}); err != nil {
		return nil, err
	}
	return connect.NewResponse(&iamv1.UpdateTeamResponse{Team: toTeamInfo(team)}), nil
}

func (s *server) DeleteTeam(ctx context.Context, req *connect.Request[iamv1.DeleteTeamRequest]) (*connect.Response[iamv1.DeleteTeamResponse], error) {
	org, id, err := orgTeam(req.Msg.Org, req.Msg.Team)
	if err != nil {
		return nil, err
	}
	if err := s.ownsNothing(ctx, org, authz.TeamSubject(id)); err != nil {
		return nil, err
	}
	actor, obj := authz.UserSubject(caller(ctx).User), authz.TeamObject(id)
	var denied error
	if err := s.commit(ctx, func(tx *store.Store) error {
		role, e, err := lockedPolicy(ctx, tx, org)
		if err != nil {
			return err
		}
		if !privileged(role) {
			if denied = teamCarriesDeny(ctx, tx, e, org, id); denied != nil {
				return denied
			}
		}
		if denied = guardOwnDenies(ctx, tx, org, role, actor, func() error { return tx.DeleteTeam(ctx, org, id) }); denied != nil {
			return denied
		}
		return tx.Audit(ctx, auditEntry(ctx, req.Header(), &org, store.ActionTeamDelete, obj, store.ResultAllowed))
	}); err != nil {
		s.auditGuardDenial(ctx, req.Header(), org, store.ActionTeamDelete, obj, denied)
		return nil, err
	}
	return connect.NewResponse(&iamv1.DeleteTeamResponse{}), nil
}

func (s *server) SetTeamMember(ctx context.Context, req *connect.Request[iamv1.SetTeamMemberRequest]) (*connect.Response[iamv1.SetTeamMemberResponse], error) {
	org, id, err := orgTeam(req.Msg.Org, req.Msg.Team)
	if err != nil {
		return nil, err
	}
	user, err := parseID("user", req.Msg.User)
	if err != nil {
		return nil, err
	}
	if req.Msg.Admin == nil {
		return nil, errAdminRequired
	}
	admin := *req.Msg.Admin
	actor, obj := authz.UserSubject(caller(ctx).User), authz.TeamObject(id)+"/members/"+user.String()
	var (
		needsUpdate bool
		guardErr    error
	)
	if err := s.commit(ctx, func(tx *store.Store) error {
		role, e, err := lockedPolicy(ctx, tx, org)
		if err != nil {
			return err
		}
		// Making someone a team admin, like changing one, needs iam.teams.update on top of manageMembers.
		if admin && !e.Allowed(actor, org, authz.TeamObject(id), "iam.teams.update") {
			needsUpdate = true
			return errForbidden
		}
		if needsUpdate, err = demotesTeamAdmin(ctx, tx, e, org, id, user); err != nil {
			return err
		} else if needsUpdate {
			return errForbidden
		}
		if guardErr = s.checkTeamRoles(ctx, tx, e, org, role, actor, id); guardErr != nil {
			return guardErr
		}
		if err := tx.SetTeamMember(ctx, org, id, user, admin); err != nil {
			return err
		}
		ae := auditEntry(ctx, req.Header(), &org, store.ActionTeamMemberSet, obj, store.ResultAllowed)
		ae.Details = map[string]any{"admin": admin}
		return tx.Audit(ctx, ae)
	}); err != nil {
		if needsUpdate {
			s.auditDenied(ctx, auditEntry(ctx, req.Header(), &org, store.ActionTeamUpdate, authz.TeamObject(id), store.ResultDenied), nil)
		}
		s.auditGuardDenial(ctx, req.Header(), org, store.ActionTeamMemberSet, obj, guardErr)
		return nil, err
	}
	return connect.NewResponse(&iamv1.SetTeamMemberResponse{}), nil
}

func (s *server) RemoveTeamMember(ctx context.Context, req *connect.Request[iamv1.RemoveTeamMemberRequest]) (*connect.Response[iamv1.RemoveTeamMemberResponse], error) {
	org, id, err := orgTeam(req.Msg.Org, req.Msg.Team)
	if err != nil {
		return nil, err
	}
	user, err := parseID("user", req.Msg.User)
	if err != nil {
		return nil, err
	}
	if user == caller(ctx).User {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("use LeaveTeam"))
	}
	actor, obj := authz.UserSubject(caller(ctx).User), authz.TeamObject(id)+"/members/"+user.String()
	var (
		needsUpdate bool
		guardErr    error
	)
	if err := s.commit(ctx, func(tx *store.Store) error {
		role, e, err := lockedPolicy(ctx, tx, org)
		if err != nil {
			return err
		}
		if needsUpdate, err = demotesTeamAdmin(ctx, tx, e, org, id, user); err != nil {
			return err
		} else if needsUpdate {
			return errForbidden
		}
		// Removing someone from a team lifts its deny rules from them, which only org owners and admins may do.
		if !privileged(role) {
			if guardErr = teamCarriesDeny(ctx, tx, e, org, id); guardErr != nil {
				return guardErr
			}
		}
		if guardErr = guardOwnDenies(ctx, tx, org, role, actor, func() error { return tx.RemoveTeamMember(ctx, org, id, user) }); guardErr != nil {
			return guardErr
		}
		return tx.Audit(ctx, auditEntry(ctx, req.Header(), &org, store.ActionTeamMemberRemove, obj, store.ResultAllowed))
	}); err != nil {
		if needsUpdate {
			s.auditDenied(ctx, auditEntry(ctx, req.Header(), &org, store.ActionTeamUpdate, authz.TeamObject(id), store.ResultDenied), nil)
		}
		s.auditGuardDenial(ctx, req.Header(), org, store.ActionTeamMemberRemove, obj, guardErr)
		return nil, err
	}
	return connect.NewResponse(&iamv1.RemoveTeamMemberResponse{}), nil
}

// LeaveTeam needs iam.teams.leave instead of manageMembers, so a team admin can leave too; the own-deny guard still applies.
func (s *server) LeaveTeam(ctx context.Context, req *connect.Request[iamv1.LeaveTeamRequest]) (*connect.Response[iamv1.LeaveTeamResponse], error) {
	org, id, err := orgTeam(req.Msg.Org, req.Msg.Team)
	if err != nil {
		return nil, err
	}
	me := caller(ctx).User
	actor, obj := authz.UserSubject(me), authz.TeamObject(id)+"/members/"+me.String()
	var (
		leaveDenied bool
		guardErr    error
	)
	if err := s.commit(ctx, func(tx *store.Store) error {
		role, e, err := lockedPolicy(ctx, tx, org)
		if err != nil {
			return err
		}
		if leaveDenied = !e.Allowed(actor, org, authz.TeamObject(id), "iam.teams.leave"); leaveDenied {
			return errForbidden
		}
		if guardErr = guardOwnDenies(ctx, tx, org, role, actor, func() error { return tx.RemoveTeamMember(ctx, org, id, me) }); guardErr != nil {
			return guardErr
		}
		return tx.Audit(ctx, auditEntry(ctx, req.Header(), &org, store.ActionTeamLeave, obj, store.ResultAllowed))
	}); err != nil {
		if leaveDenied {
			s.auditDenied(ctx, auditEntry(ctx, req.Header(), &org, store.ActionTeamLeave, authz.TeamObject(id), store.ResultDenied), nil)
		}
		s.auditGuardDenial(ctx, req.Header(), org, store.ActionTeamLeave, obj, guardErr)
		return nil, err
	}
	return connect.NewResponse(&iamv1.LeaveTeamResponse{}), nil
}

// demotesTeamAdmin reports whether the target is a team admin (read under lock) and the caller lacks iam.teams.update, which changing an admin requires.
func demotesTeamAdmin(ctx context.Context, tx *store.Store, e *authz.Enforcer, org, team, user uuid.UUID) (bool, error) {
	admin, err := tx.LockTeamMember(ctx, org, team, user)
	if errors.Is(err, store.ErrNotFound) {
		return false, nil
	}
	if err != nil || !admin {
		return false, err
	}
	return !e.Allowed(authz.UserSubject(caller(ctx).User), org, authz.TeamObject(team), "iam.teams.update"), nil
}

// teamRoles returns the IDs of the custom roles bound to team.
func teamRoles(ctx context.Context, tx *store.Store, org, team uuid.UUID) ([]uuid.UUID, error) {
	bs, err := tx.Bindings(ctx, org)
	if err != nil {
		return nil, err
	}
	var ids []uuid.UUID
	for _, b := range bs {
		if b.Subject == authz.TeamSubject(team) {
			ids = append(ids, b.RoleID)
		}
	}
	return ids, nil
}

// checkTeamRoles applies the bind guard to every role bound to team, since adding a member grants them those roles.
func (s *server) checkTeamRoles(ctx context.Context, tx *store.Store, e *authz.Enforcer, org uuid.UUID, role store.Role, actor string, team uuid.UUID) error {
	ids, err := teamRoles(ctx, tx, org, team)
	if err != nil || len(ids) == 0 {
		return err
	}
	keys := make([]string, 0, len(ids))
	for _, id := range ids {
		keys = append(keys, id.String())
	}
	if err := s.canGrant(ctx, e, actor, org, privileged(role), nil, keys); err != nil {
		return err
	}
	if privileged(role) {
		return nil
	}
	for _, id := range ids {
		if err := carriesDeny(e, org, id); err != nil {
			return err
		}
	}
	return nil
}

// teamCarriesDeny fails with ErrDenyNotAllowed if a role bound to team holds deny rules.
func teamCarriesDeny(ctx context.Context, tx *store.Store, e *authz.Enforcer, org, team uuid.UUID) error {
	ids, err := teamRoles(ctx, tx, org, team)
	if err != nil {
		return err
	}
	for _, id := range ids {
		if err := carriesDeny(e, org, id); err != nil {
			return err
		}
	}
	return nil
}

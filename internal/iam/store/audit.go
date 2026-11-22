package store

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"google.golang.org/protobuf/types/known/structpb"

	eventsv1 "github.com/asterism-dev/observatory/gen/go/observatory/events/v1"
	"github.com/asterism-dev/observatory/internal/platform/outbox"
	"github.com/asterism-dev/observatory/internal/platform/textx"
)

type ActorKind string

const (
	ActorUser   ActorKind = "user"
	ActorSystem ActorKind = "system"
)

type Result string

const (
	ResultAllowed Result = "allowed"
	ResultDenied  Result = "denied"
	ResultFailed  Result = "failed"
)

const (
	ActionAuthLogin           = "iam.sessions.create"
	ActionAuthCLILogin        = "iam.sessions.createCli"
	ActionAuthLogout          = "iam.sessions.delete"
	ActionSessionRevoke       = "iam.sessions.revoke"
	ActionOrgCreate           = "iam.orgs.create"
	ActionPlatformGrantAdmin  = "iam.platformAdmins.create"
	ActionOrgUpdate           = "iam.orgs.update"
	ActionMemberInvite        = "iam.invites.create"
	ActionInviteRevoke        = "iam.invites.delete"
	ActionInviteAccept        = "iam.invites.accept"
	ActionMemberRemove        = "iam.members.delete"
	ActionMemberLeave         = "iam.members.leave"
	ActionMemberSetRole       = "iam.members.setRole"
	ActionTeamCreate          = "iam.teams.create"
	ActionTeamUpdate          = "iam.teams.update"
	ActionTeamDelete          = "iam.teams.delete"
	ActionTeamMemberSet       = "iam.teams.setMember"
	ActionTeamMemberRemove    = "iam.teams.removeMember"
	ActionTeamLeave           = "iam.teams.leave"
	ActionRoleCreate          = "iam.roles.create"
	ActionRoleUpdate          = "iam.roles.update"
	ActionRoleDelete          = "iam.roles.delete"
	ActionRoleBind            = "iam.roleBindings.manage"
	ActionRoleUnbind          = "iam.roleBindings.delete"
	ActionPlatformRevokeAdmin = "iam.platformAdmins.delete"
	ActionAdminRead           = "iam.platform.read"
	ActionSSHKeyAdd           = "iam.sshKeys.create"
	ActionSSHKeyDelete        = "iam.sshKeys.delete"
)

type AuditEntry struct {
	ActorKind ActorKind
	ActorID   string
	OrgID     *uuid.UUID
	Action    string
	Resource  string
	Result    Result
	IP        string
	UserAgent string
	Details   map[string]any
}

var (
	actorKinds = map[ActorKind]eventsv1.ActorKind{ActorUser: eventsv1.ActorKind_ACTOR_KIND_USER, ActorSystem: eventsv1.ActorKind_ACTOR_KIND_SYSTEM}
	results    = map[Result]eventsv1.Result{ResultAllowed: eventsv1.Result_RESULT_ALLOWED, ResultDenied: eventsv1.Result_RESULT_DENIED, ResultFailed: eventsv1.Result_RESULT_FAILED}
)

// Audit writes e as an audit event to the outbox, in s's transaction if it has one, so it is published only if that commits.
func (s *Store) Audit(ctx context.Context, e AuditEntry) error {
	kind, ok := actorKinds[e.ActorKind]
	if !ok {
		return fmt.Errorf("audit %s: unknown actor kind %q", e.Action, e.ActorKind)
	}
	result, ok := results[e.Result]
	if !ok {
		return fmt.Errorf("audit %s: unknown result %q", e.Action, e.Result)
	}
	details := make(map[string]any, len(e.Details))
	for k, v := range e.Details {
		if str, ok := v.(string); ok {
			v = textx.Sanitize(str)
		}
		details[k] = v
	}
	// A JSON round trip turns UUIDs, slices and structs into values a Struct can hold, as the jsonb column did.
	b, err := json.Marshal(details)
	if err != nil {
		return fmt.Errorf("audit details: %w", err)
	}
	var plain map[string]any
	if err := json.Unmarshal(b, &plain); err != nil {
		return fmt.Errorf("audit details: %w", err)
	}
	st, err := structpb.NewStruct(plain)
	if err != nil {
		return fmt.Errorf("audit details: %w", err)
	}
	ev := &eventsv1.Event{Type: e.Action, Actor: &eventsv1.Actor{Kind: kind, Id: e.ActorID}, Resource: e.Resource,
		Result: result, ClientIp: e.IP, UserAgent: textx.Sanitize(e.UserAgent), Details: st, Audit: true}
	if e.OrgID != nil {
		ev.Org = e.OrgID.String()
	}
	return pgx.BeginFunc(ctx, s.q, func(tx pgx.Tx) error { return outbox.Write(ctx, tx, ev) })
}

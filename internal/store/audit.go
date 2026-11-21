package store

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"unicode"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"google.golang.org/protobuf/types/known/structpb"

	eventsv1 "github.com/asterism-dev/observatory/gen/go/observatory/events/v1"
	"github.com/asterism-dev/observatory/internal/platform/outbox"
)

type ActorKind string

const (
	ActorUser   ActorKind = "user"
	ActorNode   ActorKind = "node"
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
	ActionLimitSet            = "quota.limits.update"
	ActionPlatformRevokeAdmin = "iam.platformAdmins.delete"
	ActionAdminRead           = "iam.platform.read"
	ActionRegistryGC          = "artifacts.registry.collectGarbage"
	ActionPlatformGuestMTU    = "network.settings.update"
	ActionEnvSet              = "secrets.variables.set"
	ActionEnvMove             = "secrets.variables.move"
	ActionEnvDelete           = "secrets.variables.delete"
	ActionIntegrationSet      = "secrets.integrations.set"
	ActionIntegrationDelete   = "secrets.integrations.delete"
	ActionNodeCreate          = "compute.nodes.create"
	ActionNodeUpdate          = "compute.nodes.update"
	ActionNodeMetrics         = "compute.nodes.getMetrics"
	ActionNodeEnroll          = "compute.nodes.enroll"
	ActionNodeRevoke          = "compute.nodes.revokeCredential"
	ActionNodeDelete          = "compute.nodes.delete"
	ActionVMCreate            = "compute.instances.create"
	ActionVMPower             = "compute.instances.setPower"
	ActionVMDelete            = "compute.instances.delete"
	ActionVMDeleteAfter       = "compute.instances.setDeleteAfter"
	ActionVMNetwork           = "compute.instances.setNetworks"
	ActionVMDisks             = "compute.instances.setDisks"
	ActionVMSnapshot          = "compute.instances.snapshot"
	ActionVMSnapshotDelete    = "compute.snapshots.delete"
	ActionVMRestore           = "compute.instances.restore"
	ActionVMFork              = "compute.instances.fork"
	ActionVMMetrics           = "compute.instances.getMetrics"
	ActionDiskCreate          = "compute.disks.create"
	ActionDiskCopy            = "compute.disks.copy"
	ActionDiskGrow            = "compute.disks.grow"
	ActionDiskDelete          = "compute.disks.delete"
	ActionDiskRelease         = "compute.disks.release"
	ActionDeviceCreate        = "network.devices.create"
	ActionDeviceDelete        = "network.devices.delete"
	ActionNetworkCreate       = "network.networks.create"
	ActionNetworkDelete       = "network.networks.delete"
	ActionSSHKeyAdd           = "iam.sshKeys.create"
	ActionSSHKeyDelete        = "iam.sshKeys.delete"
	ActionImageCreate         = "artifacts.images.create"
	ActionImageUpdate         = "artifacts.images.update"
	ActionImageDelete         = "artifacts.images.delete"
	ActionImageRecipe         = "build.recipes.update"
	ActionImageVersionDelete  = "artifacts.versions.delete"
	ActionImageTag            = "artifacts.images.tag"
	ActionImageDefault        = "artifacts.images.setDefault"
	ActionImageBuild          = "build.builds.create"
	ActionImageBuildCancel    = "build.builds.cancel"
	ActionGrafanaTokenCreate  = "telemetry.tokens.create"
	ActionGrafanaTokenRevoke  = "telemetry.tokens.delete"
	ActionDashboardCreate     = "telemetry.dashboards.create"
	ActionDashboardUpdate     = "telemetry.dashboards.update"
	ActionDashboardDelete     = "telemetry.dashboards.delete"
	ActionLogsRead            = "telemetry.logEntries.list"
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

const maxTextBytes = 512

// SanitizeText makes client-controlled text storable: valid UTF-8, no control characters (Postgres rejects NUL), at most 512 bytes.
func SanitizeText(s string) string {
	s = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return -1
		}
		return r
	}, strings.ToValidUTF8(s, ""))
	if len(s) > maxTextBytes {
		s = strings.ToValidUTF8(s[:maxTextBytes], "")
	}
	return s
}

var (
	actorKinds = map[ActorKind]eventsv1.ActorKind{ActorUser: eventsv1.ActorKind_ACTOR_KIND_USER, ActorNode: eventsv1.ActorKind_ACTOR_KIND_NODE, ActorSystem: eventsv1.ActorKind_ACTOR_KIND_SYSTEM}
	results    = map[Result]eventsv1.Result{ResultAllowed: eventsv1.Result_RESULT_ALLOWED, ResultDenied: eventsv1.Result_RESULT_DENIED, ResultFailed: eventsv1.Result_RESULT_FAILED}
)

// Audit writes e as an audit event to the outbox, in s's transaction if it has one, so it is published only if that commits.
func (s *Store) Audit(ctx context.Context, e AuditEntry) error {
	details := make(map[string]any, len(e.Details))
	for k, v := range e.Details {
		if str, ok := v.(string); ok {
			v = SanitizeText(str)
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
	ev := &eventsv1.Event{Type: e.Action, Actor: &eventsv1.Actor{Kind: actorKinds[e.ActorKind], Id: e.ActorID}, Resource: e.Resource,
		Result: results[e.Result], ClientIp: e.IP, UserAgent: SanitizeText(e.UserAgent), Details: st, Audit: true}
	if e.OrgID != nil {
		ev.Org = e.OrgID.String()
	}
	return pgx.BeginFunc(ctx, s.q, func(tx pgx.Tx) error { return outbox.Write(ctx, tx, ev) })
}

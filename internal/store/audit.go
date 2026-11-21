package store

import (
	"context"
	"fmt"
	"strings"
	"time"
	"unicode"

	"github.com/google/uuid"
)

type ActorKind string

const (
	ActorUser   ActorKind = "user"
	ActorVM     ActorKind = "vm"
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
	ActionAuditRead           = "audit.events.list"
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
	ID        int64
	At        time.Time
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

type AuditFilter struct {
	ActorID, Action string
	OrgID           *uuid.UUID
	From, To        time.Time
	Before          int64
	Limit           int
}

const (
	defaultAuditLimit = 50
	maxAuditLimit     = 200
	maxTextBytes      = 512
)

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

func (s *Store) Audit(ctx context.Context, e AuditEntry) error {
	details := make(map[string]any, len(e.Details))
	for k, v := range e.Details {
		if str, ok := v.(string); ok {
			v = SanitizeText(str)
		}
		details[k] = v
	}
	e.Details = details
	e.UserAgent = SanitizeText(e.UserAgent)
	_, err := s.q.Exec(ctx, `
		INSERT INTO audit_log (actor_kind, actor_id, org_id, action, resource, result, ip, user_agent, details)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)`,
		e.ActorKind, e.ActorID, e.OrgID, e.Action, e.Resource, e.Result, e.IP, e.UserAgent, e.Details)
	return err
}

func (s *Store) AuditEntries(ctx context.Context, f AuditFilter) ([]AuditEntry, error) {
	var (
		where []string
		args  []any
	)
	add := func(cond string, v any) {
		args = append(args, v)
		where = append(where, fmt.Sprintf(cond, len(args)))
	}
	if f.ActorID != "" {
		add("actor_id = $%d", f.ActorID)
	}
	if f.Action != "" {
		add("action = $%d", f.Action)
	}
	if f.OrgID != nil {
		add("org_id = $%d", *f.OrgID)
	}
	if !f.From.IsZero() {
		add("at >= $%d", f.From)
	}
	if !f.To.IsZero() {
		add("at < $%d", f.To)
	}
	if f.Before > 0 {
		add("id < $%d", f.Before)
	}
	limit := f.Limit
	if limit <= 0 {
		limit = defaultAuditLimit
	}
	limit = min(limit, maxAuditLimit)
	args = append(args, limit)

	query := "SELECT id, at, actor_kind, actor_id, org_id, action, resource, result, ip, user_agent, details FROM audit_log"
	for i, w := range where {
		if i == 0 {
			query += " WHERE " + w
		} else {
			query += " AND " + w
		}
	}
	query += fmt.Sprintf(" ORDER BY id DESC LIMIT $%d", len(args))

	rows, err := s.q.Query(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []AuditEntry
	for rows.Next() {
		var e AuditEntry
		if err := rows.Scan(&e.ID, &e.At, &e.ActorKind, &e.ActorID, &e.OrgID, &e.Action, &e.Resource, &e.Result, &e.IP, &e.UserAgent, &e.Details); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

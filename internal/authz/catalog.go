// Package authz holds the org permission model: catalog, built-in roles and the Casbin enforcer.
package authz

import (
	"errors"
	"slices"
	"strings"

	"github.com/casbin/casbin/v3/util"
	"github.com/google/uuid"
)

const (
	ObjSettings         = "org/settings"
	ObjMembers          = "org/members"
	ObjTeams            = "org/teams"
	ObjRoles            = "org/roles"
	ObjLimits           = "org/limits"
	ObjAudit            = "org/audit"
	ObjLogs             = "org/logs"
	ObjEnv              = "org/env"
	ObjTeamEnv          = "org/team-env"
	ObjUserEnv          = "org/user-env"
	ObjIntegrations     = "org/integrations"
	ObjTeamIntegrations = "org/team-integrations"
	ObjUserIntegrations = "org/user-integrations"
	ObjImages           = "org/images"
	ObjTeamImages       = "org/team-images"
	ObjUserImages       = "org/user-images"
	ObjDashboards       = "org/dashboards"
	ObjTeamDashboards   = "org/team-dashboards"
	ObjUserDashboards   = "org/user-dashboards"
	ObjNodes            = "org/nodes"
	ObjVMs              = "org/vms"
	ObjTeamVMs          = "org/team-vms"

	ObjUserDevices = "org/user-devices"
	ObjNetworks    = "org/networks"
	ObjDisks       = "org/disks"
	ObjTeamDisks   = "org/team-disks"
)

// Subjects; "vm:<id>" is reserved for VM identities.
func UserSubject(id uuid.UUID) string { return "user:" + id.String() }
func TeamSubject(id uuid.UUID) string { return "team:" + id.String() }

// RoleSubject takes a built-in key (owner|admin|member) or a custom role UUID string.
func RoleSubject(key string) string { return "role:" + key }

func TeamObject(id uuid.UUID) string { return ObjTeams + "/" + id.String() }
func RoleObject(id uuid.UUID) string { return ObjRoles + "/" + id.String() }

func TeamEnvObject(id uuid.UUID) string { return ObjTeamEnv + "/" + id.String() }

// UserEnvObject is a member's private env scope; no built-in role reaches it, only its user.
func UserEnvObject(id uuid.UUID) string { return ObjUserEnv + "/" + id.String() }

func TeamIntegrationsObject(id uuid.UUID) string { return ObjTeamIntegrations + "/" + id.String() }

// UserIntegrationsObject is a member's private integration scope; no built-in role reaches it, only its user.
func UserIntegrationsObject(id uuid.UUID) string { return ObjUserIntegrations + "/" + id.String() }

func TeamImagesObject(id uuid.UUID) string { return ObjTeamImages + "/" + id.String() }

// UserImagesObject is a member's private image scope; no built-in role reaches it, only its user.
func UserImagesObject(id uuid.UUID) string { return ObjUserImages + "/" + id.String() }

func TeamDashboardsObject(id uuid.UUID) string { return ObjTeamDashboards + "/" + id.String() }

// UserDashboardsObject is a member's private dashboard scope; no built-in role reaches it, only its user.
func UserDashboardsObject(id uuid.UUID) string { return ObjUserDashboards + "/" + id.String() }

func NodeObject(id uuid.UUID) string { return ObjNodes + "/" + id.String() }

// VMObject is org-scoped for user and team VMs alike, so org-wide rules cover every VM.
func VMObject(id uuid.UUID) string { return ObjVMs + "/" + id.String() }

func TeamVMsObject(teamID uuid.UUID) string { return ObjTeamVMs + "/" + teamID.String() }

func UserDevicesObject(user uuid.UUID) string { return ObjUserDevices + "/" + user.String() }

func NetworkObject(id uuid.UUID) string { return ObjNetworks + "/" + id.String() }

// DiskObject is org-scoped for user and team disks alike, so org-wide rules cover every disk.
func DiskObject(id uuid.UUID) string { return ObjDisks + "/" + id.String() }

func TeamDisksObject(team uuid.UUID) string { return ObjTeamDisks + "/" + team.String() }

type Effect string

const (
	Allow Effect = "allow"
	Deny  Effect = "deny"
)

type Rule struct {
	Effect Effect
	Object string
	Action string
}

type Permission struct {
	Action string
	Object string
}

var Catalog = []Permission{
	{"iam.orgs.update", ObjSettings},
	{"telemetry.tokens.create", ObjSettings},
	{"iam.members.list", ObjMembers},
	{"iam.invites.create", ObjMembers},
	{"iam.members.delete", ObjMembers},
	{"iam.members.setRole", ObjMembers},
	{"iam.teams.list", ObjTeams},
	{"iam.teams.create", ObjTeams},
	{"iam.teams.update", ObjTeams + "/*"},
	{"iam.teams.delete", ObjTeams + "/*"},
	{"iam.teams.manageMembers", ObjTeams + "/*"},
	{"iam.teams.leave", ObjTeams + "/*"},
	{"iam.roles.list", ObjRoles},
	{"iam.roles.manage", ObjRoles},
	{"iam.roleBindings.manage", ObjRoles},
	{"quota.limits.get", ObjLimits},
	{"quota.limits.update", ObjLimits},
	{"audit.events.list", ObjAudit},
	{"audit.settings.get", ObjAudit},
	{"audit.settings.update", ObjAudit},
	{"telemetry.logEntries.list", ObjLogs},
	{"secrets.variables.list", ObjEnv},
	{"secrets.variables.set", ObjEnv},
	{"secrets.variables.delete", ObjEnv},
	{"secrets.variables.use", ObjEnv},
	{"secrets.variables.list", ObjTeamEnv + "/*"},
	{"secrets.variables.set", ObjTeamEnv + "/*"},
	{"secrets.variables.delete", ObjTeamEnv + "/*"},
	{"secrets.variables.use", ObjTeamEnv + "/*"},
	{"secrets.variables.list", ObjUserEnv + "/*"},
	{"secrets.variables.set", ObjUserEnv + "/*"},
	{"secrets.variables.delete", ObjUserEnv + "/*"},
	{"secrets.variables.use", ObjUserEnv + "/*"},
	{"secrets.integrations.list", ObjIntegrations},
	{"secrets.integrations.set", ObjIntegrations},
	{"secrets.integrations.delete", ObjIntegrations},
	{"secrets.integrations.use", ObjIntegrations},
	{"secrets.integrations.list", ObjTeamIntegrations + "/*"},
	{"secrets.integrations.set", ObjTeamIntegrations + "/*"},
	{"secrets.integrations.delete", ObjTeamIntegrations + "/*"},
	{"secrets.integrations.use", ObjTeamIntegrations + "/*"},
	{"secrets.integrations.list", ObjUserIntegrations + "/*"},
	{"secrets.integrations.set", ObjUserIntegrations + "/*"},
	{"secrets.integrations.delete", ObjUserIntegrations + "/*"},
	{"secrets.integrations.use", ObjUserIntegrations + "/*"},
	{"artifacts.images.list", ObjImages},
	{"artifacts.images.use", ObjImages},
	{"artifacts.images.update", ObjImages},
	{"build.builds.create", ObjImages},
	{"artifacts.images.setDefault", ObjImages},
	{"artifacts.images.list", ObjTeamImages + "/*"},
	{"artifacts.images.use", ObjTeamImages + "/*"},
	{"artifacts.images.update", ObjTeamImages + "/*"},
	{"build.builds.create", ObjTeamImages + "/*"},
	{"artifacts.images.setDefault", ObjTeamImages + "/*"},
	{"artifacts.images.list", ObjUserImages + "/*"},
	{"artifacts.images.use", ObjUserImages + "/*"},
	{"artifacts.images.update", ObjUserImages + "/*"},
	{"build.builds.create", ObjUserImages + "/*"},
	{"artifacts.images.setDefault", ObjUserImages + "/*"},
	{"telemetry.dashboards.get", ObjDashboards},
	{"telemetry.dashboards.update", ObjDashboards},
	{"telemetry.dashboards.create", ObjDashboards},
	{"telemetry.dashboards.get", ObjTeamDashboards + "/*"},
	{"telemetry.dashboards.update", ObjTeamDashboards + "/*"},
	{"telemetry.dashboards.create", ObjTeamDashboards + "/*"},
	{"telemetry.dashboards.get", ObjUserDashboards + "/*"},
	{"telemetry.dashboards.update", ObjUserDashboards + "/*"},
	{"telemetry.dashboards.create", ObjUserDashboards + "/*"},
	{"compute.nodes.get", ObjNodes},
	{"compute.nodes.create", ObjNodes},
	{"compute.nodes.get", ObjNodes + "/*"},
	{"compute.nodes.update", ObjNodes + "/*"},
	{"compute.nodes.delete", ObjNodes + "/*"},
	{"compute.nodes.use", ObjNodes + "/*"},
	{"compute.nodes.getMetrics", ObjNodes + "/*"},
	{"compute.instances.create", ObjVMs},
	{"compute.instances.create", ObjTeamVMs + "/*"},
	{"compute.instances.get", ObjVMs + "/*"},
	{"compute.instances.setPower", ObjVMs + "/*"},
	{"compute.instances.delete", ObjVMs + "/*"},
	{"compute.instances.connect", ObjVMs + "/*"},
	{"compute.instances.setNetworks", ObjVMs + "/*"},
	{"compute.instances.setDisks", ObjVMs + "/*"},
	{"compute.instances.snapshot", ObjVMs + "/*"},
	{"compute.instances.restore", ObjVMs + "/*"},
	{"compute.instances.fork", ObjVMs + "/*"},
	{"compute.instances.getMetrics", ObjVMs + "/*"},
	{"network.devices.create", ObjUserDevices + "/*"},
	{"network.devices.list", ObjUserDevices + "/*"},
	{"network.devices.delete", ObjUserDevices + "/*"},
	{"network.networks.get", ObjNetworks},
	{"network.networks.create", ObjNetworks},
	{"network.networks.use", ObjNetworks + "/*"},
	{"network.networks.delete", ObjNetworks + "/*"},
	{"compute.disks.create", ObjDisks},
	{"compute.disks.create", ObjTeamDisks + "/*"},
	{"compute.disks.get", ObjDisks + "/*"},
	{"compute.disks.update", ObjDisks + "/*"},
	{"compute.disks.delete", ObjDisks + "/*"},
	{"compute.disks.use", ObjDisks + "/*"},
}

// Unruled are actions no org rule decides (membership alone, the caller's own user:<id> objects, platform admin), so they stay out of Catalog and the role editor.
var Unruled = []Permission{
	{"iam.orgs.get", "org"},
	{"iam.users.get", "user:*"},
	{"iam.orgs.create", "user:*"},
	{"iam.invites.accept", "user:*"},
	{"iam.members.leave", "user:*"},
	{"iam.sessions.list", "user:*/sessions"},
	{"iam.sessions.revoke", "user:*/sessions/*"},
	{"iam.sshKeys.list", "user:*/ssh-keys"},
	{"iam.sshKeys.create", "user:*/ssh-keys"},
	{"iam.sshKeys.delete", "user:*/ssh-keys/*"},
	{"iam.platform.read", "platform/orgs"},
	{"iam.platform.read", "platform/users"},
	{"iam.platformAdmins.update", "platform/users"},
}

// BuiltinRules returns the allow rules of a built-in role, or nil for an unknown key.
func BuiltinRules(key string) []Rule {
	member := []Rule{
		{Allow, ObjMembers, "iam.members.list"},
		{Allow, ObjTeams + "*", "iam.teams.list"},
		{Allow, ObjTeams + "*", "iam.teams.leave"},
		{Allow, ObjRoles, "iam.roles.list"},
		{Allow, ObjLimits, "quota.limits.get"},
		{Allow, ObjEnv, "secrets.variables.list"},
		{Allow, ObjEnv, "secrets.variables.use"},
		{Allow, ObjIntegrations, "secrets.integrations.list"},
		{Allow, ObjIntegrations, "secrets.integrations.use"},
		{Allow, ObjImages, "artifacts.images.list"},
		{Allow, ObjImages, "artifacts.images.use"},
		{Allow, ObjDashboards, "telemetry.dashboards.get"},
		{Allow, ObjNodes, "compute.nodes.get"},
		{Allow, ObjNodes + "/*", "compute.nodes.get"},
		{Allow, ObjNodes + "/*", "compute.nodes.use"},
		{Allow, ObjVMs, "compute.instances.create"},
		{Allow, ObjNetworks, "network.networks.get"},
		{Allow, ObjNetworks, "network.networks.create"},
		{Allow, ObjNetworks + "/*", "network.networks.use"},
		{Allow, ObjDisks, "compute.disks.create"},
	}
	admin := append(member,
		Rule{Allow, ObjSettings, "iam.orgs.update"},
		Rule{Allow, ObjSettings, "telemetry.tokens.create"},
		Rule{Allow, ObjMembers, "iam.invites.create"},
		Rule{Allow, ObjMembers, "iam.members.delete"},
		Rule{Allow, ObjTeams + "*", "iam.teams.*"},
		Rule{Allow, ObjRoles, "iam.roles.manage"},
		Rule{Allow, ObjRoles, "iam.roleBindings.manage"},
		Rule{Allow, ObjLimits, "quota.limits.update"},
		Rule{Allow, ObjAudit, "audit.events.list"},
		Rule{Allow, ObjAudit, "audit.settings.get"},
		Rule{Allow, ObjAudit, "audit.settings.update"},
		Rule{Allow, ObjLogs, "telemetry.logEntries.list"},
		Rule{Allow, ObjEnv, "secrets.variables.*"},
		Rule{Allow, ObjTeamEnv + "/*", "secrets.variables.*"},
		Rule{Allow, ObjIntegrations, "secrets.integrations.*"},
		Rule{Allow, ObjTeamIntegrations + "/*", "secrets.integrations.*"},
		Rule{Allow, ObjImages, "artifacts.images.*"},
		Rule{Allow, ObjImages, "build.builds.create"},
		Rule{Allow, ObjTeamImages + "/*", "artifacts.images.*"},
		Rule{Allow, ObjTeamImages + "/*", "build.builds.create"},
		Rule{Allow, ObjDashboards, "telemetry.dashboards.*"},
		Rule{Allow, ObjTeamDashboards + "/*", "telemetry.dashboards.*"},
		Rule{Allow, ObjNodes, "compute.nodes.*"},
		Rule{Allow, ObjNodes + "/*", "compute.nodes.*"},
		// Explicit VM actions, never compute.instances.*: admins must not gain connect or future VM actions.
		Rule{Allow, ObjTeamVMs + "/*", "compute.instances.create"},
		Rule{Allow, ObjVMs + "/*", "compute.instances.get"},
		Rule{Allow, ObjVMs + "/*", "compute.instances.setPower"},
		Rule{Allow, ObjVMs + "/*", "compute.instances.delete"},
		Rule{Allow, ObjVMs + "/*", "compute.instances.setNetworks"},
		Rule{Allow, ObjVMs + "/*", "compute.instances.snapshot"},
		Rule{Allow, ObjVMs + "/*", "compute.instances.restore"},
		Rule{Allow, ObjVMs + "/*", "compute.instances.getMetrics"},
		Rule{Allow, ObjUserDevices + "/*", "network.devices.list"},
		Rule{Allow, ObjUserDevices + "/*", "network.devices.delete"},
		Rule{Allow, ObjNetworks + "/*", "network.networks.delete"},
		// Never compute.disks.use or compute.disks.*: admins detach and delete a member's disk but cannot read it.
		Rule{Allow, ObjTeamDisks + "/*", "compute.disks.create"},
		Rule{Allow, ObjDisks + "/*", "compute.disks.get"},
		Rule{Allow, ObjDisks + "/*", "compute.disks.update"},
		Rule{Allow, ObjDisks + "/*", "compute.disks.delete"},
		Rule{Allow, ObjVMs + "/*", "compute.instances.setDisks"},
	)
	switch key {
	case "member":
		return member
	case "admin":
		return admin
	case "owner":
		return append(admin, Rule{Allow, ObjMembers, "iam.members.setRole"})
	}
	return nil
}

var errPattern = errors.New("invalid object pattern")

// ValidatePattern accepts catalog objects and org/user-scoped patterns with `*` only as a whole last segment.
func ValidatePattern(obj string) error {
	if slices.ContainsFunc(Catalog, func(p Permission) bool { return p.Object == obj }) {
		return nil
	}
	scope, rest, ok := strings.Cut(obj, "/")
	if !ok {
		return errPattern
	}
	if scope != "org" {
		id, found := strings.CutPrefix(scope, "user:")
		if !found {
			return errPattern
		}
		// Only the canonical form matches the objects built with id.String().
		if parsed, err := uuid.Parse(id); err != nil || parsed.String() != id {
			return errPattern
		}
	}
	if rest == "*" {
		return nil
	}
	typ, id, ok := strings.Cut(rest, "/")
	if !ok || !validSegment(typ) || (id != "*" && !validSegment(id)) {
		return errPattern
	}
	return nil
}

func validSegment(s string) bool {
	return s != "" && !strings.ContainsAny(s, "*/")
}

// ExpandActions returns the distinct catalog actions matched by a keyMatch action pattern; `*` may only end a pattern as a whole segment.
func ExpandActions(pattern string) []string {
	if i := strings.Index(pattern, "*"); i >= 0 && (i != len(pattern)-1 || i > 0 && pattern[i-1] != '.') {
		return nil
	}
	var out []string
	for _, p := range Catalog {
		if util.KeyMatch(p.Action, pattern) && !slices.Contains(out, p.Action) {
			out = append(out, p.Action)
		}
	}
	return out
}

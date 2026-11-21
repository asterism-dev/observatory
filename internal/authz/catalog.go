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
	{"org:update", ObjSettings},
	{"telemetry:token", ObjSettings},
	{"member:read", ObjMembers},
	{"member:invite", ObjMembers},
	{"member:remove", ObjMembers},
	{"member:set-role", ObjMembers},
	{"team:read", ObjTeams},
	{"team:create", ObjTeams},
	{"team:update", ObjTeams + "/*"},
	{"team:delete", ObjTeams + "/*"},
	{"team:manage-members", ObjTeams + "/*"},
	{"team:leave", ObjTeams + "/*"},
	{"role:read", ObjRoles},
	{"role:write", ObjRoles},
	{"role:bind", ObjRoles},
	{"limit:read", ObjLimits},
	{"limit:set", ObjLimits},
	{"audit:read", ObjAudit},
	{"logs:read", ObjLogs},
	{"env:list", ObjEnv},
	{"env:set", ObjEnv},
	{"env:delete", ObjEnv},
	{"env:use", ObjEnv},
	{"env:list", ObjTeamEnv + "/*"},
	{"env:set", ObjTeamEnv + "/*"},
	{"env:delete", ObjTeamEnv + "/*"},
	{"env:use", ObjTeamEnv + "/*"},
	{"env:list", ObjUserEnv + "/*"},
	{"env:set", ObjUserEnv + "/*"},
	{"env:delete", ObjUserEnv + "/*"},
	{"env:use", ObjUserEnv + "/*"},
	{"integration:list", ObjIntegrations},
	{"integration:set", ObjIntegrations},
	{"integration:delete", ObjIntegrations},
	{"integration:use", ObjIntegrations},
	{"integration:list", ObjTeamIntegrations + "/*"},
	{"integration:set", ObjTeamIntegrations + "/*"},
	{"integration:delete", ObjTeamIntegrations + "/*"},
	{"integration:use", ObjTeamIntegrations + "/*"},
	{"integration:list", ObjUserIntegrations + "/*"},
	{"integration:set", ObjUserIntegrations + "/*"},
	{"integration:delete", ObjUserIntegrations + "/*"},
	{"integration:use", ObjUserIntegrations + "/*"},
	{"image:list", ObjImages},
	{"image:use", ObjImages},
	{"image:edit", ObjImages},
	{"image:build", ObjImages},
	{"image:default", ObjImages},
	{"image:list", ObjTeamImages + "/*"},
	{"image:use", ObjTeamImages + "/*"},
	{"image:edit", ObjTeamImages + "/*"},
	{"image:build", ObjTeamImages + "/*"},
	{"image:default", ObjTeamImages + "/*"},
	{"image:list", ObjUserImages + "/*"},
	{"image:use", ObjUserImages + "/*"},
	{"image:edit", ObjUserImages + "/*"},
	{"image:build", ObjUserImages + "/*"},
	{"image:default", ObjUserImages + "/*"},
	{"dashboard:read", ObjDashboards},
	{"dashboard:edit", ObjDashboards},
	{"dashboard:create", ObjDashboards},
	{"dashboard:read", ObjTeamDashboards + "/*"},
	{"dashboard:edit", ObjTeamDashboards + "/*"},
	{"dashboard:create", ObjTeamDashboards + "/*"},
	{"dashboard:read", ObjUserDashboards + "/*"},
	{"dashboard:edit", ObjUserDashboards + "/*"},
	{"dashboard:create", ObjUserDashboards + "/*"},
	{"node:read", ObjNodes},
	{"node:create", ObjNodes},
	{"node:read", ObjNodes + "/*"},
	{"node:update", ObjNodes + "/*"},
	{"node:delete", ObjNodes + "/*"},
	{"node:use", ObjNodes + "/*"},
	{"node:metrics", ObjNodes + "/*"},
	{"vm:create", ObjVMs},
	{"vm:create", ObjTeamVMs + "/*"},
	{"vm:read", ObjVMs + "/*"},
	{"vm:power", ObjVMs + "/*"},
	{"vm:delete", ObjVMs + "/*"},
	{"vm:connect", ObjVMs + "/*"},
	{"vm:network", ObjVMs + "/*"},
	{"vm:disks", ObjVMs + "/*"},
	{"vm:snapshot", ObjVMs + "/*"},
	{"vm:restore", ObjVMs + "/*"},
	{"vm:fork", ObjVMs + "/*"},
	{"vm:metrics", ObjVMs + "/*"},
	{"device:create", ObjUserDevices + "/*"},
	{"device:list", ObjUserDevices + "/*"},
	{"device:delete", ObjUserDevices + "/*"},
	{"network:read", ObjNetworks},
	{"network:create", ObjNetworks},
	{"network:use", ObjNetworks + "/*"},
	{"network:delete", ObjNetworks + "/*"},
	{"disk:create", ObjDisks},
	{"disk:create", ObjTeamDisks + "/*"},
	{"disk:read", ObjDisks + "/*"},
	{"disk:update", ObjDisks + "/*"},
	{"disk:delete", ObjDisks + "/*"},
	{"disk:use", ObjDisks + "/*"},
}

// BuiltinRules returns the allow rules of a built-in role, or nil for an unknown key.
func BuiltinRules(key string) []Rule {
	member := []Rule{
		{Allow, ObjMembers, "member:read"},
		{Allow, ObjTeams + "*", "team:read"},
		{Allow, ObjTeams + "*", "team:leave"},
		{Allow, ObjRoles, "role:read"},
		{Allow, ObjLimits, "limit:read"},
		{Allow, ObjEnv, "env:list"},
		{Allow, ObjEnv, "env:use"},
		{Allow, ObjIntegrations, "integration:list"},
		{Allow, ObjIntegrations, "integration:use"},
		{Allow, ObjImages, "image:list"},
		{Allow, ObjImages, "image:use"},
		{Allow, ObjDashboards, "dashboard:read"},
		{Allow, ObjNodes, "node:read"},
		{Allow, ObjNodes + "/*", "node:read"},
		{Allow, ObjNodes + "/*", "node:use"},
		{Allow, ObjVMs, "vm:create"},
		{Allow, ObjNetworks, "network:read"},
		{Allow, ObjNetworks, "network:create"},
		{Allow, ObjNetworks + "/*", "network:use"},
		{Allow, ObjDisks, "disk:create"},
	}
	admin := append(member,
		Rule{Allow, ObjSettings, "org:update"},
		Rule{Allow, ObjSettings, "telemetry:token"},
		Rule{Allow, ObjMembers, "member:invite"},
		Rule{Allow, ObjMembers, "member:remove"},
		Rule{Allow, ObjTeams + "*", "team:*"},
		Rule{Allow, ObjRoles, "role:write"},
		Rule{Allow, ObjRoles, "role:bind"},
		Rule{Allow, ObjLimits, "limit:set"},
		Rule{Allow, ObjAudit, "audit:read"},
		Rule{Allow, ObjLogs, "logs:read"},
		Rule{Allow, ObjEnv, "env:*"},
		Rule{Allow, ObjTeamEnv + "/*", "env:*"},
		Rule{Allow, ObjIntegrations, "integration:*"},
		Rule{Allow, ObjTeamIntegrations + "/*", "integration:*"},
		Rule{Allow, ObjImages, "image:*"},
		Rule{Allow, ObjTeamImages + "/*", "image:*"},
		Rule{Allow, ObjDashboards, "dashboard:*"},
		Rule{Allow, ObjTeamDashboards + "/*", "dashboard:*"},
		Rule{Allow, ObjNodes, "node:*"},
		Rule{Allow, ObjNodes + "/*", "node:*"},
		// Explicit VM actions, never vm:*: admins must not gain vm:connect or future VM actions.
		Rule{Allow, ObjTeamVMs + "/*", "vm:create"},
		Rule{Allow, ObjVMs + "/*", "vm:read"},
		Rule{Allow, ObjVMs + "/*", "vm:power"},
		Rule{Allow, ObjVMs + "/*", "vm:delete"},
		Rule{Allow, ObjVMs + "/*", "vm:network"},
		Rule{Allow, ObjVMs + "/*", "vm:snapshot"},
		Rule{Allow, ObjVMs + "/*", "vm:restore"},
		Rule{Allow, ObjVMs + "/*", "vm:metrics"},
		Rule{Allow, ObjUserDevices + "/*", "device:list"},
		Rule{Allow, ObjUserDevices + "/*", "device:delete"},
		Rule{Allow, ObjNetworks + "/*", "network:delete"},
		// Never disk:use or disk:*: admins detach and delete a member's disk but cannot read it.
		Rule{Allow, ObjTeamDisks + "/*", "disk:create"},
		Rule{Allow, ObjDisks + "/*", "disk:read"},
		Rule{Allow, ObjDisks + "/*", "disk:update"},
		Rule{Allow, ObjDisks + "/*", "disk:delete"},
		Rule{Allow, ObjVMs + "/*", "vm:disks"},
	)
	switch key {
	case "member":
		return member
	case "admin":
		return admin
	case "owner":
		return append(admin, Rule{Allow, ObjMembers, "member:set-role"})
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

// ExpandActions returns the distinct catalog actions matched by a keyMatch action pattern.
func ExpandActions(pattern string) []string {
	var out []string
	for _, p := range Catalog {
		if util.KeyMatch(p.Action, pattern) && !slices.Contains(out, p.Action) {
			out = append(out, p.Action)
		}
	}
	return out
}

// Package authz holds the org permission model: catalogs, built-in roles and the Casbin enforcer.
package authz

import (
	"cmp"
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

// Unruled are actions no org rule decides (membership alone, the caller's own user:<id> objects, platform admin), so they stay out of org catalogs and the role editor.
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

var errPattern = errors.New("invalid object pattern")

// ValidatePattern accepts catalog objects and org/user-scoped patterns with `*` only as a whole last segment.
func (e *Enforcer) ValidatePattern(obj string) error {
	if slices.ContainsFunc(e.actions, func(p Permission) bool { return p.Object == obj }) {
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

// ExpandActions returns the distinct catalog actions, sorted, matched by a keyMatch action pattern; `*` may only end a pattern as a whole segment.
func (e *Enforcer) ExpandActions(pattern string) []string {
	if i := strings.Index(pattern, "*"); i >= 0 && (i != len(pattern)-1 || i > 0 && pattern[i-1] != '.') {
		return nil
	}
	var out []string
	for _, p := range e.actions {
		if util.KeyMatch(p.Action, pattern) && !slices.Contains(out, p.Action) {
			out = append(out, p.Action)
		}
	}
	return out
}

// builtinRoles are ordered lowest first: each role also gets the defaults of the roles before it.
var builtinRoles = []string{"member", "admin", "owner"}

// ownedVMs is the OwnedRef kind CanConnect reads.
const ownedVMs = "vms"

func (e *Enforcer) indexCatalogs() {
	var cumulative []Rule
	for _, key := range builtinRoles {
		for _, c := range e.policy.Catalogs {
			for _, d := range c.RoleDefaults {
				if d.Role == key {
					cumulative = append(cumulative, Rule{Allow, d.Object, d.Action})
				}
			}
		}
		e.builtin[key] = slices.Clone(cumulative)
	}
	for _, c := range e.policy.Catalogs {
		for _, a := range c.Actions {
			e.actions = append(e.actions, Permission{Action: a.Name, Object: a.Object})
			e.known[a.Name] = true
		}
	}
	slices.SortFunc(e.actions, func(a, b Permission) int {
		return cmp.Or(strings.Compare(a.Action, b.Action), strings.Compare(a.Object, b.Object))
	})
	e.actions = slices.Compact(e.actions)
}

// BuiltinRules returns the allow rules of a built-in role, inherited ones included, or nil for an unknown key.
func (e *Enforcer) BuiltinRules(key string) []Rule {
	if !slices.Contains(builtinRoles, key) {
		return nil
	}
	return slices.Clone(e.builtin[key])
}

// Actions returns every registered action and object, sorted by action, then object.
func (e *Enforcer) Actions() []Permission { return slices.Clone(e.actions) }

// UnknownActions returns the actions org's custom rules name that no registered catalog matches, sorted.
func (e *Enforcer) UnknownActions(org uuid.UUID) []string {
	var out []string
	for _, cr := range e.policy.Roles {
		if cr.OrgID != org {
			continue
		}
		for _, r := range cr.Rules {
			if len(e.ExpandActions(r.Action)) == 0 {
				out = append(out, r.Action)
			}
		}
	}
	slices.Sort(out)
	return slices.Compact(out)
}

// Catalogs returns the registered catalogs the enforcer was built from; callers must not modify them.
func (e *Enforcer) Catalogs() []Catalog { return e.policy.Catalogs }

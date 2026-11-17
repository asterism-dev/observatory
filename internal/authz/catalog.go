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
	ObjSettings = "org/settings"
	ObjMembers  = "org/members"
	ObjTeams    = "org/teams"
	ObjRoles    = "org/roles"
	ObjLimits   = "org/limits"
	ObjAudit    = "org/audit"
	ObjEnv      = "org/env"
	ObjTeamEnv  = "org/team-env"
	ObjUserEnv  = "org/user-env"
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
	}
	admin := append(member,
		Rule{Allow, ObjSettings, "org:update"},
		Rule{Allow, ObjMembers, "member:invite"},
		Rule{Allow, ObjMembers, "member:remove"},
		Rule{Allow, ObjTeams + "*", "team:*"},
		Rule{Allow, ObjRoles, "role:write"},
		Rule{Allow, ObjRoles, "role:bind"},
		Rule{Allow, ObjLimits, "limit:set"},
		Rule{Allow, ObjAudit, "audit:read"},
		Rule{Allow, ObjEnv, "env:*"},
		Rule{Allow, ObjTeamEnv + "/*", "env:*"},
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

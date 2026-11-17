package authz

import (
	_ "embed"
	"log/slog"
	"slices"

	"github.com/casbin/casbin/v3"
	"github.com/casbin/casbin/v3/model"
	"github.com/google/uuid"
)

//go:embed model.conf
var modelConf string

// allDomains is the policy domain of built-in role rules; the matcher applies it to every org.
const allDomains = "*"

type MemberRole struct {
	OrgID, UserID uuid.UUID
	Role          string
}

type TeamMember struct {
	OrgID, TeamID, UserID uuid.UUID
	Admin                 bool
}

type CustomRole struct {
	ID, OrgID         uuid.UUID
	Name, Description string
	Rules             []Rule
	// Parents are "member", "admin" or a custom role UUID of the same org.
	Parents []string
}

type Binding struct {
	OrgID   uuid.UUID
	Subject string
	RoleID  uuid.UUID
}

// Policy is the full snapshot of org permissions the enforcer is built from.
type Policy struct {
	Memberships []MemberRole
	TeamMembers []TeamMember
	Roles       []CustomRole
	Bindings    []Binding
}

type Enforcer struct {
	policy Policy
	ce     *casbin.Enforcer
}

func New(p Policy) (*Enforcer, error) {
	m, err := model.NewModelFromString(modelConf)
	if err != nil {
		return nil, err
	}
	ce, err := casbin.NewEnforcer(m)
	if err != nil {
		return nil, err
	}
	var rules, groups [][]string
	for _, key := range []string{"owner", "admin", "member"} {
		for _, r := range BuiltinRules(key) {
			rules = append(rules, []string{RoleSubject(key), allDomains, r.Object, r.Action, string(r.Effect)})
		}
	}
	for _, mr := range p.Memberships {
		u, org := UserSubject(mr.UserID), mr.OrgID.String()
		groups = append(groups, []string{u, RoleSubject(mr.Role), org})
		// Users may do everything with their own resources; org deny rules still win.
		rules = append(rules, []string{u, org, u + "/*", "*", string(Allow)})
	}
	for _, tm := range p.TeamMembers {
		u, org := UserSubject(tm.UserID), tm.OrgID.String()
		groups = append(groups, []string{u, TeamSubject(tm.TeamID), org})
		if tm.Admin {
			rules = append(rules, []string{u, org, TeamObject(tm.TeamID), "team:manage-members", string(Allow)})
		}
	}
	for _, cr := range p.Roles {
		sub, org := RoleSubject(cr.ID.String()), cr.OrgID.String()
		for _, r := range cr.Rules {
			rules = append(rules, []string{sub, org, r.Object, r.Action, string(r.Effect)})
		}
		for _, parent := range cr.Parents {
			groups = append(groups, []string{sub, RoleSubject(parent), org})
		}
	}
	for _, b := range p.Bindings {
		groups = append(groups, []string{b.Subject, RoleSubject(b.RoleID.String()), b.OrgID.String()})
	}
	// The Ex variants skip duplicate rows instead of rejecting the whole batch.
	if len(rules) > 0 {
		if _, err := ce.AddPoliciesEx(rules); err != nil {
			return nil, err
		}
	}
	if len(groups) > 0 {
		if _, err := ce.AddGroupingPoliciesEx(groups); err != nil {
			return nil, err
		}
	}
	return &Enforcer{policy: p, ce: ce}, nil
}

func (e *Enforcer) Allowed(sub string, org uuid.UUID, obj, act string) bool {
	ok, err := e.ce.Enforce(sub, org.String(), obj, act)
	if err != nil {
		slog.Error("authz enforce", "sub", sub, "org", org, "obj", obj, "act", act, "err", err)
		return false
	}
	return ok
}

// DenyRulesFor returns the deny rules of sub and of every team and role it reaches in org.
func (e *Enforcer) DenyRulesFor(sub string, org uuid.UUID) []Rule {
	dom := org.String()
	subjects, err := e.ce.GetImplicitRolesForUser(sub, dom)
	if err != nil {
		slog.Error("authz implicit roles", "sub", sub, "org", org, "err", err)
		return nil
	}
	subjects = append(subjects, sub)
	policy, err := e.ce.GetPolicy()
	if err != nil {
		slog.Error("authz get policy", "err", err)
		return nil
	}
	var out []Rule
	for _, p := range policy {
		if p[4] == string(Deny) && (p[1] == dom || p[1] == allDomains) && slices.Contains(subjects, p[0]) {
			out = append(out, Rule{Effect: Deny, Object: p[2], Action: p[3]})
		}
	}
	return out
}

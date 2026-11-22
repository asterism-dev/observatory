package authz

import (
	_ "embed"
	"fmt"
	"log/slog"
	"slices"
	"strings"

	"github.com/casbin/casbin/v3"
	"github.com/casbin/casbin/v3/model"
	"github.com/casbin/casbin/v3/util"
	"github.com/google/uuid"
)

//go:embed model.conf
var modelConf string

// allDomains is the policy domain of built-in role rules; the matcher applies it to every org.
const allDomains = "*"

// MaxRoleDepth bounds the longest subject→team→role→parent chain; Casbin stops resolving at 10 hops.
const MaxRoleDepth = 8

type MemberRole struct {
	OrgID, UserID uuid.UUID
	Role          string
}

type TeamMember struct {
	OrgID, TeamID, UserID uuid.UUID
	Admin                 bool
}

type TeamRef struct {
	OrgID, ID uuid.UUID
}

// VMRef is a VM's owner: exactly one of UserID and TeamID is set.
type VMRef struct {
	OrgID, ID      uuid.UUID
	UserID, TeamID *uuid.UUID
}

// OwnedRef is a service-owned resource's owner: Kind is its object type segment (vms, disks), and exactly one of UserID and TeamID is set.
type OwnedRef struct {
	Kind           string
	OrgID, ID      uuid.UUID
	UserID, TeamID *uuid.UUID
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
	// Teams lists every team, including those without members, so team subjects get their env rules.
	Teams []TeamRef
	Owned []OwnedRef
	// Catalogs are the registered service catalogs: actions, built-in role defaults and owner-grant templates.
	Catalogs []Catalog
	// Revision is iam's stream revision this policy came from, 0 when it was not streamed.
	Revision uint64
}

type Enforcer struct {
	policy  Policy
	ce      *casbin.Enforcer
	vms     map[uuid.UUID]VMRef
	teams   map[teamMember]bool
	roles   map[userOrg]string
	orgsOf  map[uuid.UUID][]uuid.UUID
	usersOf map[uuid.UUID][]uuid.UUID
	// members maps every known team, empty ones included, to its user IDs.
	members map[TeamRef][]uuid.UUID
	actions []Permission
	known   map[string]bool
	builtin map[string][]Rule
}

type teamMember struct{ org, team, user uuid.UUID }

type userOrg struct{ user, org uuid.UUID }

func (e *Enforcer) Revision() uint64 { return e.policy.Revision }

func New(p Policy) (*Enforcer, error) {
	if err := validate(p); err != nil {
		return nil, err
	}
	m, err := model.NewModelFromString(modelConf)
	if err != nil {
		return nil, err
	}
	ce, err := casbin.NewEnforcer(m)
	if err != nil {
		return nil, err
	}
	e := &Enforcer{policy: p, vms: map[uuid.UUID]VMRef{}, teams: make(map[teamMember]bool, len(p.TeamMembers)),
		roles: make(map[userOrg]string, len(p.Memberships)), orgsOf: map[uuid.UUID][]uuid.UUID{}, usersOf: map[uuid.UUID][]uuid.UUID{}, members: make(map[TeamRef][]uuid.UUID, len(p.Teams)),
		known: map[string]bool{}, builtin: map[string][]Rule{}}
	e.indexCatalogs()
	var rules, groups [][]string
	allow := func(sub, org, obj string, acts ...string) {
		for _, act := range acts {
			rules = append(rules, []string{sub, org, obj, act, string(Allow)})
		}
	}
	for _, key := range builtinRoles {
		for _, r := range e.builtin[key] {
			rules = append(rules, []string{RoleSubject(key), allDomains, r.Object, r.Action, string(r.Effect)})
		}
	}
	templates := map[Relation][]OwnerTemplate{}
	for _, c := range p.Catalogs {
		for _, t := range c.Templates {
			templates[t.Relation] = append(templates[t.Relation], t)
		}
	}
	fill := func(rel Relation, sub, org, placeholder, value string) {
		for _, t := range templates[rel] {
			allow(sub, org, strings.ReplaceAll(t.Object, placeholder, value), t.Actions...)
		}
	}
	teamAdmins := map[uuid.UUID][]uuid.UUID{}
	for _, mr := range p.Memberships {
		u, org := UserSubject(mr.UserID), mr.OrgID.String()
		groups = append(groups, []string{u, RoleSubject(mr.Role), org})
		// Users may do everything with their own resources; org deny rules still win.
		allow(u, org, u+"/*", "*")
		fill(RelMember, u, org, "{user}", mr.UserID.String())
	}
	for _, tm := range p.TeamMembers {
		u, org := UserSubject(tm.UserID), tm.OrgID.String()
		groups = append(groups, []string{u, TeamSubject(tm.TeamID), org})
		if tm.Admin {
			fill(RelTeamAdmin, u, org, "{team}", tm.TeamID.String())
			teamAdmins[tm.TeamID] = append(teamAdmins[tm.TeamID], tm.UserID)
		}
	}
	for _, t := range p.Teams {
		fill(RelTeam, TeamSubject(t.ID), t.OrgID.String(), "{team}", t.ID.String())
	}
	for _, o := range p.Owned {
		org := o.OrgID.String()
		owned := func(rel Relation, sub string) {
			for _, t := range templates[rel] {
				if t.Object == "org/"+o.Kind+"/{id}" {
					allow(sub, org, "org/"+o.Kind+"/"+o.ID.String(), t.Actions...)
				}
			}
		}
		switch {
		case o.UserID != nil:
			owned(RelOwnerUser, UserSubject(*o.UserID))
		case o.TeamID != nil:
			owned(RelOwnerTeam, TeamSubject(*o.TeamID))
			for _, admin := range teamAdmins[*o.TeamID] {
				owned(RelOwnerTeamAdmin, UserSubject(admin))
			}
		}
		if o.Kind == ownedVMs {
			e.vms[o.ID] = VMRef{OrgID: o.OrgID, ID: o.ID, UserID: o.UserID, TeamID: o.TeamID}
		}
	}
	for _, cr := range p.Roles {
		sub, org := RoleSubject(cr.ID.String()), cr.OrgID.String()
		for _, r := range cr.Rules {
			// An allow naming no registered action grants nothing, so a later catalog can't revive it; denies keep covering new actions.
			if r.Effect == Allow && !e.known[r.Action] {
				continue
			}
			rules = append(rules, []string{sub, org, r.Object, r.Action, string(r.Effect)})
		}
		for _, parent := range cr.Parents {
			groups = append(groups, []string{sub, RoleSubject(parent), org})
		}
	}
	for _, b := range p.Bindings {
		groups = append(groups, []string{b.Subject, RoleSubject(b.RoleID.String()), b.OrgID.String()})
	}
	if err := checkDepth(groups); err != nil {
		return nil, err
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
	e.ce = ce
	for _, mr := range p.Memberships {
		e.roles[userOrg{mr.UserID, mr.OrgID}] = mr.Role
		e.orgsOf[mr.UserID] = append(e.orgsOf[mr.UserID], mr.OrgID)
		e.usersOf[mr.OrgID] = append(e.usersOf[mr.OrgID], mr.UserID)
	}
	for _, t := range p.Teams {
		e.members[TeamRef{OrgID: t.OrgID, ID: t.ID}] = nil
	}
	for _, tm := range p.TeamMembers {
		e.teams[teamMember{tm.OrgID, tm.TeamID, tm.UserID}] = true
		ref := TeamRef{OrgID: tm.OrgID, ID: tm.TeamID}
		e.members[ref] = append(e.members[ref], tm.UserID)
	}
	return e, nil
}

// Role returns user's built-in role in org; false means not a member.
func (e *Enforcer) Role(org, user uuid.UUID) (string, bool) {
	r, ok := e.roles[userOrg{user, org}]
	return r, ok
}

func (e *Enforcer) OrgsOf(user uuid.UUID) []uuid.UUID { return slices.Clone(e.orgsOf[user]) }

// UsersOf returns the user IDs of org's members.
func (e *Enforcer) UsersOf(org uuid.UUID) []uuid.UUID { return slices.Clone(e.usersOf[org]) }

func (e *Enforcer) TeamMembers(org, team uuid.UUID) []uuid.UUID {
	return slices.Clone(e.members[TeamRef{OrgID: org, ID: team}])
}

func (e *Enforcer) HasTeam(org, team uuid.UUID) bool {
	_, ok := e.members[TeamRef{OrgID: org, ID: team}]
	return ok
}

// HasSubject reports whether sub, user:<id> or team:<id>, is a member or team of org.
func (e *Enforcer) HasSubject(org uuid.UUID, sub string) bool {
	kind, raw, _ := strings.Cut(sub, ":")
	id, err := uuid.Parse(raw)
	switch {
	case err != nil:
		return false
	case kind == "team":
		return e.HasTeam(org, id)
	case kind == "user":
		_, ok := e.Role(org, id)
		return ok
	}
	return false
}

func (e *Enforcer) Allowed(sub string, org uuid.UUID, obj, act string) bool {
	ok, err := e.ce.Enforce(sub, org.String(), obj, act)
	if err != nil {
		slog.Error("authz enforce", "sub", sub, "org", org, "obj", obj, "act", act, "err", err)
		return false
	}
	return ok
}

// CanConnect holds only through the owner relation (user VM owner, owning team's members) of a VM this enforcer knows, never custom grants; denies still cut.
func (e *Enforcer) CanConnect(user uuid.UUID, vm VMRef) bool {
	return e.ConnectChecker().CanConnect(user, vm)
}

// ConnectChecker answers CanConnect for many pairs, reading each user's deny rules once; use one per batch.
type ConnectChecker struct {
	e      *Enforcer
	denies map[userOrg][]Rule
}

func (e *Enforcer) ConnectChecker() *ConnectChecker {
	return &ConnectChecker{e: e, denies: map[userOrg][]Rule{}}
}

func (c *ConnectChecker) CanConnect(user uuid.UUID, vm VMRef) bool {
	known, ok := c.e.vms[vm.ID]
	if !ok || known.OrgID != vm.OrgID || !sameOwner(known.UserID, vm.UserID) || !sameOwner(known.TeamID, vm.TeamID) {
		return false
	}
	switch {
	case vm.UserID != nil:
		if *vm.UserID != user {
			return false
		}
	case vm.TeamID != nil:
		if !c.e.teams[teamMember{vm.OrgID, *vm.TeamID, user}] {
			return false
		}
	default:
		return false
	}
	// The owner relation implies the generated compute.instances.connect grant, so only denies remain; they match exactly as the casbin matcher does.
	obj := VMObject(vm.ID)
	return !slices.ContainsFunc(c.denyRules(user, vm.OrgID), func(d Rule) bool {
		return util.KeyMatch(obj, d.Object) && util.KeyMatch("compute.instances.connect", d.Action)
	})
}

// denyRules caches DenyRulesFor per user and org; on error it returns a deny-all rule so the check fails closed.
func (c *ConnectChecker) denyRules(user, org uuid.UUID) []Rule {
	key := userOrg{user, org}
	rules, ok := c.denies[key]
	if !ok {
		var err error
		if rules, err = c.e.DenyRulesFor(UserSubject(user), org); err != nil {
			slog.Error("authz deny rules", "user", user, "org", org, "err", err)
			rules = []Rule{{Effect: Deny, Object: "*", Action: "*"}}
		}
		c.denies[key] = rules
	}
	return rules
}

func sameOwner(a, b *uuid.UUID) bool { return (a == nil) == (b == nil) && (a == nil || *a == *b) }

// DenyRulesFor returns the deny rules of sub and of every team and role it reaches in org.
func (e *Enforcer) DenyRulesFor(sub string, org uuid.UUID) ([]Rule, error) {
	dom := org.String()
	subjects, err := e.ce.GetImplicitRolesForUser(sub, dom)
	if err != nil {
		return nil, fmt.Errorf("authz: implicit roles of %s: %w", sub, err)
	}
	subjects = append(subjects, sub)
	policy, err := e.ce.GetPolicy()
	if err != nil {
		return nil, fmt.Errorf("authz: get policy: %w", err)
	}
	var out []Rule
	for _, p := range policy {
		if p[4] == string(Deny) && (p[1] == dom || p[1] == allDomains) && slices.Contains(subjects, p[0]) {
			out = append(out, Rule{Effect: Deny, Object: p[2], Action: p[3]})
		}
	}
	return out, nil
}

func validate(p Policy) error {
	roleOrg := make(map[uuid.UUID]uuid.UUID, len(p.Roles))
	for _, cr := range p.Roles {
		roleOrg[cr.ID] = cr.OrgID
	}
	for _, mr := range p.Memberships {
		if !slices.Contains(builtinRoles, mr.Role) {
			return fmt.Errorf("authz: membership of user %s in org %s has unknown role %q", mr.UserID, mr.OrgID, mr.Role)
		}
	}
	for _, cr := range p.Roles {
		for _, parent := range cr.Parents {
			if !validParent(parent, cr.OrgID, roleOrg) {
				return fmt.Errorf("authz: role %s has invalid parent %q", cr.ID, parent)
			}
		}
	}
	for _, b := range p.Bindings {
		if org, ok := roleOrg[b.RoleID]; !ok || org != b.OrgID {
			return fmt.Errorf("authz: binding of %s to role %s: no such custom role in org %s", b.Subject, b.RoleID, b.OrgID)
		}
		if !validSubject(b.Subject) {
			return fmt.Errorf("authz: binding to role %s has invalid subject %q", b.RoleID, b.Subject)
		}
	}
	return nil
}

// validParent allows "member", "admin" or the canonical ID of a custom role in org; roleOrg maps role IDs to orgs.
func validParent(parent string, org uuid.UUID, roleOrg map[uuid.UUID]uuid.UUID) bool {
	if parent == "member" || parent == "admin" {
		return true
	}
	id, err := uuid.Parse(parent)
	if err != nil || id.String() != parent {
		return false
	}
	o, ok := roleOrg[id]
	return ok && o == org
}

func validSubject(s string) bool {
	id, ok := strings.CutPrefix(s, "user:")
	if !ok {
		id, ok = strings.CutPrefix(s, "team:")
	}
	parsed, err := uuid.Parse(id)
	return ok && err == nil && parsed.String() == id
}

// checkDepth fails closed on chains Casbin would silently cut off (hiding deep deny rules) and on cycles.
func checkDepth(groups [][]string) error {
	type node struct{ org, sub string }
	next := map[node][]string{}
	for _, g := range groups {
		k := node{g[2], g[0]}
		next[k] = append(next[k], g[1])
	}
	const visiting = -1
	depth := map[node]int{}
	var walk func(k node) (int, error)
	walk = func(k node) (int, error) {
		if d, seen := depth[k]; seen {
			if d == visiting {
				return 0, fmt.Errorf("authz: role cycle through %s in org %s", k.sub, k.org)
			}
			return d, nil
		}
		depth[k] = visiting
		longest := 0
		for _, n := range next[k] {
			d, err := walk(node{k.org, n})
			if err != nil {
				return 0, err
			}
			longest = max(longest, d+1)
		}
		depth[k] = longest
		return longest, nil
	}
	for k := range next {
		d, err := walk(k)
		if err != nil {
			return err
		}
		if d > MaxRoleDepth {
			return fmt.Errorf("authz: %s in org %s reaches roles %d hops deep (max %d)", k.sub, k.org, d, MaxRoleDepth)
		}
	}
	return nil
}

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

// DiskRef is a live disk's owner: exactly one of UserID and TeamID is set.
type DiskRef struct {
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
	VMs   []VMRef
	Disks []DiskRef
}

type Enforcer struct {
	policy Policy
	ce     *casbin.Enforcer
	vms    map[uuid.UUID]VMRef
	teams  map[teamMember]bool
	roles  map[userOrg]string
	orgsOf map[uuid.UUID][]uuid.UUID
	// members maps every known team, empty ones included, to its user IDs.
	members map[TeamRef][]uuid.UUID
}

type teamMember struct{ org, team, user uuid.UUID }

type userOrg struct{ user, org uuid.UUID }

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
	var rules, groups [][]string
	teamAdmins := map[uuid.UUID][]uuid.UUID{}
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
		rules = append(rules, []string{u, org, UserEnvObject(mr.UserID), "secrets.variables.*", string(Allow)})
		rules = append(rules, []string{u, org, UserIntegrationsObject(mr.UserID), "secrets.integrations.*", string(Allow)})
		rules = append(rules, []string{u, org, UserImagesObject(mr.UserID), "artifacts.images.*", string(Allow)})
		rules = append(rules, []string{u, org, UserImagesObject(mr.UserID), "build.builds.create", string(Allow)})
		rules = append(rules, []string{u, org, UserDashboardsObject(mr.UserID), "telemetry.dashboards.*", string(Allow)})
		for _, act := range []string{"network.devices.create", "network.devices.list", "network.devices.delete"} {
			rules = append(rules, []string{u, org, UserDevicesObject(mr.UserID), act, string(Allow)})
		}
	}
	for _, tm := range p.TeamMembers {
		u, org := UserSubject(tm.UserID), tm.OrgID.String()
		groups = append(groups, []string{u, TeamSubject(tm.TeamID), org})
		if tm.Admin {
			rules = append(rules, []string{u, org, TeamObject(tm.TeamID), "iam.teams.manageMembers", string(Allow)})
			rules = append(rules, []string{u, org, TeamEnvObject(tm.TeamID), "secrets.variables.*", string(Allow)})
			rules = append(rules, []string{u, org, TeamIntegrationsObject(tm.TeamID), "secrets.integrations.*", string(Allow)})
			rules = append(rules, []string{u, org, TeamImagesObject(tm.TeamID), "artifacts.images.*", string(Allow)})
			rules = append(rules, []string{u, org, TeamImagesObject(tm.TeamID), "build.builds.create", string(Allow)})
			rules = append(rules, []string{u, org, TeamDashboardsObject(tm.TeamID), "telemetry.dashboards.*", string(Allow)})
			rules = append(rules, []string{u, org, TeamVMsObject(tm.TeamID), "compute.instances.create", string(Allow)})
			rules = append(rules, []string{u, org, TeamDisksObject(tm.TeamID), "compute.disks.create", string(Allow)})
			teamAdmins[tm.TeamID] = append(teamAdmins[tm.TeamID], tm.UserID)
		}
	}
	for _, t := range p.Teams {
		sub, org := TeamSubject(t.ID), t.OrgID.String()
		rules = append(rules,
			[]string{sub, org, TeamEnvObject(t.ID), "secrets.variables.list", string(Allow)},
			[]string{sub, org, TeamEnvObject(t.ID), "secrets.variables.use", string(Allow)},
			[]string{sub, org, ObjEnv, "secrets.variables.use", string(Allow)},
			[]string{sub, org, TeamIntegrationsObject(t.ID), "secrets.integrations.list", string(Allow)},
			[]string{sub, org, TeamIntegrationsObject(t.ID), "secrets.integrations.use", string(Allow)},
			[]string{sub, org, ObjIntegrations, "secrets.integrations.use", string(Allow)},
			[]string{sub, org, TeamImagesObject(t.ID), "artifacts.images.list", string(Allow)},
			[]string{sub, org, TeamImagesObject(t.ID), "artifacts.images.use", string(Allow)},
			[]string{sub, org, ObjImages, "artifacts.images.use", string(Allow)},
			[]string{sub, org, TeamDashboardsObject(t.ID), "telemetry.dashboards.get", string(Allow)},
			[]string{sub, org, TeamDashboardsObject(t.ID), "telemetry.dashboards.create", string(Allow)},
		)
	}
	for _, vm := range p.VMs {
		obj, org := VMObject(vm.ID), vm.OrgID.String()
		grant := func(sub string, acts ...string) {
			for _, act := range acts {
				rules = append(rules, []string{sub, org, obj, act, string(Allow)})
			}
		}
		switch {
		case vm.UserID != nil:
			grant(UserSubject(*vm.UserID), "compute.instances.get", "compute.instances.setPower", "compute.instances.delete", "compute.instances.setNetworks", "compute.instances.setDisks", "compute.instances.connect", "compute.instances.snapshot", "compute.instances.restore", "compute.instances.fork", "compute.instances.getMetrics")
		case vm.TeamID != nil:
			grant(TeamSubject(*vm.TeamID), "compute.instances.get", "compute.instances.setPower", "compute.instances.connect", "compute.instances.snapshot", "compute.instances.getMetrics")
			for _, admin := range teamAdmins[*vm.TeamID] {
				grant(UserSubject(admin), "compute.instances.get", "compute.instances.setPower", "compute.instances.delete", "compute.instances.setNetworks", "compute.instances.setDisks", "compute.instances.snapshot", "compute.instances.restore", "compute.instances.fork", "compute.instances.getMetrics")
			}
		}
	}
	for _, d := range p.Disks {
		obj, org := DiskObject(d.ID), d.OrgID.String()
		grant := func(sub string, acts ...string) {
			for _, act := range acts {
				rules = append(rules, []string{sub, org, obj, act, string(Allow)})
			}
		}
		switch {
		case d.UserID != nil:
			grant(UserSubject(*d.UserID), "compute.disks.get", "compute.disks.update", "compute.disks.delete", "compute.disks.use")
		case d.TeamID != nil:
			grant(TeamSubject(*d.TeamID), "compute.disks.get", "compute.disks.use")
			for _, admin := range teamAdmins[*d.TeamID] {
				grant(UserSubject(admin), "compute.disks.get", "compute.disks.update", "compute.disks.delete", "compute.disks.use")
			}
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
	e := &Enforcer{policy: p, ce: ce, vms: make(map[uuid.UUID]VMRef, len(p.VMs)), teams: make(map[teamMember]bool, len(p.TeamMembers)),
		roles: make(map[userOrg]string, len(p.Memberships)), orgsOf: map[uuid.UUID][]uuid.UUID{}, members: make(map[TeamRef][]uuid.UUID, len(p.Teams))}
	for _, vm := range p.VMs {
		e.vms[vm.ID] = vm
	}
	for _, mr := range p.Memberships {
		e.roles[userOrg{mr.UserID, mr.OrgID}] = mr.Role
		e.orgsOf[mr.UserID] = append(e.orgsOf[mr.UserID], mr.OrgID)
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
		if BuiltinRules(mr.Role) == nil {
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

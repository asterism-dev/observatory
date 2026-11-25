// Package accounts bridges today's org/team permission model and the account model during the migration.
package accounts

import (
	"strings"

	"github.com/google/uuid"

	"github.com/asterism-dev/observatory/internal/authz"
	"github.com/asterism-dev/observatory/internal/policy"
	"github.com/asterism-dev/observatory/internal/store"
)

type kind struct{ service, typ string }

// objectMap maps an old object prefix to the resource type it becomes; scope says how the account is found.
var objectMap = map[string]struct {
	kind
	scope string // "org" (default account), "team" (the team's account) or "user" (default account)
}{
	"org/vms":               {kind{"compute", "vm"}, "org"},
	"org/team-vms":          {kind{"compute", "vm"}, "team"},
	"org/disks":             {kind{"compute", "disk"}, "org"},
	"org/team-disks":        {kind{"compute", "disk"}, "team"},
	"org/nodes":             {kind{"compute", "node"}, "org"},
	"org/networks":          {kind{"compute", "network"}, "org"},
	"org/env":               {kind{"secrets", "env"}, "org"},
	"org/team-env":          {kind{"secrets", "env"}, "team"},
	"org/user-env":          {kind{"secrets", "env"}, "user"},
	"org/integrations":      {kind{"integrations", "integration"}, "org"},
	"org/team-integrations": {kind{"integrations", "integration"}, "team"},
	"org/user-integrations": {kind{"integrations", "integration"}, "user"},
	"org/images":            {kind{"images", "image"}, "org"},
	"org/team-images":       {kind{"images", "image"}, "team"},
	"org/user-images":       {kind{"images", "image"}, "user"},
	"org/dashboards":        {kind{"telemetry", "dashboard"}, "org"},
	"org/team-dashboards":   {kind{"telemetry", "dashboard"}, "team"},
	"org/user-dashboards":   {kind{"telemetry", "dashboard"}, "user"},
	"org/logs":              {kind{"telemetry", "logs"}, "org"},
	"org/settings":          {kind{"telemetry", "token"}, "org"},
}

// actionMap maps an old action prefix to its new service and type; prefixes absent here stay org-level.
var actionMap = map[string]kind{
	"vm": {"compute", "vm"}, "disk": {"compute", "disk"}, "node": {"compute", "node"}, "network": {"compute", "network"},
	"env": {"secrets", "env"}, "integration": {"integrations", "integration"}, "image": {"images", "image"},
	"dashboard": {"telemetry", "dashboard"}, "logs": {"telemetry", "logs"},
}

// newAction maps an old action ("vm:power", "vm:*", "*") to new action patterns; false for org-level actions.
func newAction(act string) ([]string, kind, bool) {
	if act == "*" {
		out := make([]string, len(policy.Services))
		for i, s := range policy.Services {
			out[i] = s + ":*"
		}
		return out, kind{}, true
	}
	if act == "telemetry:token" {
		return []string{"telemetry:token:create"}, kind{"telemetry", "token"}, true
	}
	prefix, verb, _ := strings.Cut(act, ":")
	k, ok := actionMap[prefix]
	if !ok {
		return nil, kind{}, false
	}
	return []string{k.service + ":" + k.typ + ":" + verb}, k, true
}

// RewriteRule turns one old rule of org into a new rule; ${account} stands for the account the role is bound in.
func RewriteRule(r authz.Rule, org uuid.UUID, s store.AccountSnapshot) (policy.Rule, bool) {
	actions, k, ok := newAction(r.Action)
	if !ok {
		return policy.Rule{}, false
	}
	effect := policy.Allow
	if r.Effect == authz.Deny {
		effect = policy.Deny
	}
	if r.Object == "org/*" {
		svc, typ := "*", "*"
		if k.service != "" {
			svc, typ = k.service, k.typ+"/*"
		}
		return policy.Rule{Effect: effect, Actions: actions, Resources: []string{"obs:" + svc + ":${account}:" + typ}}, true
	}
	prefix, rest := r.Object, ""
	if i := strings.LastIndex(r.Object, "/"); i > len("org") {
		if _, known := objectMap[r.Object[:i]]; known {
			prefix, rest = r.Object[:i], r.Object[i+1:]
		}
	}
	m, ok := objectMap[strings.TrimSuffix(prefix, "*")]
	if !ok || k.service != "" && k != m.kind {
		return policy.Rule{}, false
	}
	account, id := "${account}", "*"
	switch {
	case rest == "" || rest == "*":
	case m.scope == "team":
		team, err := uuid.Parse(rest)
		acc, found := s.OfTeam(team)
		if err != nil || !found {
			return policy.Rule{}, false
		}
		account = acc.String()
	case m.scope == "user":
		acc, found := s.Default(org)
		if !found {
			return policy.Rule{}, false
		}
		account = acc.String()
	default:
		id = rest
	}
	return policy.Rule{Effect: effect, Actions: actions, Resources: []string{"obs:" + m.service + ":" + account + ":" + m.typ + "/" + id}}, true
}

// sharedFromDefault is what team VMs used from the org before the org stopped owning resources.
var sharedFromDefault = []string{"compute:node:use", "compute:network:use", "images:image:use", "secrets:env:use", "integrations:integration:use"}

// Derive computes every user's rights in the account model from today's policy.
func Derive(p authz.Policy, s store.AccountSnapshot) map[uuid.UUID]policy.Rights {
	out := map[uuid.UUID]policy.Rights{}
	add := func(user, acc uuid.UUID, rules ...policy.Rule) {
		r, ok := out[user]
		if !ok {
			r = policy.Rights{UserID: user.String(), Accounts: map[uuid.UUID][]policy.Rule{}}
		}
		r.Accounts[acc] = append(r.Accounts[acc], rules...)
		out[user] = r
	}
	orgOf := map[uuid.UUID][]uuid.UUID{}
	for _, m := range p.Memberships {
		orgOf[m.UserID] = append(orgOf[m.UserID], m.OrgID)
		if m.Role == "owner" || m.Role == "admin" {
			for _, acc := range s.InOrg(m.OrgID) {
				add(m.UserID, acc, policy.BuiltinRole("admin")...)
			}
		} else if def, ok := s.Default(m.OrgID); ok {
			add(m.UserID, def, policy.BuiltinRole("member")...)
		}
	}
	teamMembers := map[uuid.UUID][]uuid.UUID{}
	for _, tm := range p.TeamMembers {
		teamMembers[tm.TeamID] = append(teamMembers[tm.TeamID], tm.UserID)
		if acc, ok := s.OfTeam(tm.TeamID); ok {
			key := "member"
			if tm.Admin {
				key = "admin"
			}
			add(tm.UserID, acc, policy.BuiltinRole(key)...)
		}
	}
	roles := map[uuid.UUID]authz.CustomRole{}
	for _, r := range p.Roles {
		roles[r.ID] = r
	}
	for _, b := range p.Bindings {
		rules := flatten(roles, b.RoleID.String(), b.OrgID, s, map[string]bool{})
		kindOf, id, _ := strings.Cut(b.Subject, ":")
		subject, err := uuid.Parse(id)
		if err != nil {
			continue
		}
		switch kindOf {
		case "user":
			for _, acc := range s.InOrg(b.OrgID) {
				add(subject, acc, rules...)
			}
		case "team":
			if acc, ok := s.OfTeam(subject); ok {
				for _, u := range teamMembers[subject] {
					add(u, acc, rules...)
				}
			}
		}
	}

	var shares []policy.Share
	for _, org := range orgsOf(s) {
		def, ok := s.Default(org)
		if !ok {
			continue
		}
		var teams []uuid.UUID
		for _, a := range s.Accounts {
			if a.OrgID == org && a.TeamID != nil {
				teams = append(teams, a.ID)
			}
		}
		if len(teams) > 0 {
			shares = append(shares, policy.Share{Owner: def, Grantees: teams, Rule: policy.Rule{
				Effect: policy.Allow, Actions: sharedFromDefault, Resources: []string{"obs:*:${account}:*"}}})
		}
		for _, sub := range s.Subscriptions {
			if sub.Account != def {
				continue
			}
			deny := subscriptionDeny(sub)
			for user := range out {
				add(user, def, deny)
			}
			if len(teams) > 0 {
				shares = append(shares, policy.Share{Owner: def, Grantees: teams, Rule: deny})
			}
		}
	}
	for user, r := range out {
		for _, sh := range shares {
			for acc := range r.Accounts {
				if sh.Owner == acc || containsID(sh.Grantees, acc) {
					r.Shares = append(r.Shares, sh)
					break
				}
			}
		}
		out[user] = r
	}
	return out
}

func subscriptionDeny(sub store.Subscription) policy.Rule {
	return policy.Rule{
		Effect: policy.Deny, Actions: []string{"integrations:integration:use"},
		Resources:  []string{policy.NewID("integrations", sub.Account, "integration", sub.ID.String()).String()},
		Conditions: []policy.Condition{{Op: "StringNotEquals", Key: "obs:UserId", Values: []string{sub.Owner.String()}}},
	}
}

// flatten resolves a role and its parents into new rules; built-in parents become the account role of that name.
func flatten(roles map[uuid.UUID]authz.CustomRole, key string, org uuid.UUID, s store.AccountSnapshot, seen map[string]bool) []policy.Rule {
	if seen[key] {
		return nil
	}
	seen[key] = true
	if key == "member" || key == "admin" {
		return policy.BuiltinRole(key)
	}
	id, err := uuid.Parse(key)
	if err != nil {
		return nil
	}
	role, ok := roles[id]
	if !ok {
		return nil
	}
	var out []policy.Rule
	for _, r := range role.Rules {
		if nr, ok := RewriteRule(r, org, s); ok {
			out = append(out, nr)
		}
	}
	for _, parent := range role.Parents {
		out = append(out, flatten(roles, parent, org, s, seen)...)
	}
	return out
}

func orgsOf(s store.AccountSnapshot) []uuid.UUID {
	seen := map[uuid.UUID]bool{}
	var out []uuid.UUID
	for _, a := range s.Accounts {
		if !a.Platform && !seen[a.OrgID] {
			seen[a.OrgID] = true
			out = append(out, a.OrgID)
		}
	}
	return out
}

func containsID(ids []uuid.UUID, id uuid.UUID) bool {
	for _, x := range ids {
		if x == id {
			return true
		}
	}
	return false
}

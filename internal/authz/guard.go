package authz

import (
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/casbin/casbin/v3/util"
	"github.com/google/uuid"
)

var (
	ErrInvalidPattern = errors.New("invalid rule")
	ErrDenyNotAllowed = errors.New("only org owners and admins may write deny rules")
	ErrEscalation     = errors.New("cannot grant a permission you do not hold")
)

// CanGrant fails unless every rule is valid, denies come from an org owner/admin and actor holds every allowed action unblocked.
func (e *Enforcer) CanGrant(actor string, org uuid.UUID, isOrgAdmin bool, rules []Rule) error {
	for _, r := range rules {
		if (r.Effect != Allow && r.Effect != Deny) || ValidatePattern(r.Object) != nil || len(ExpandActions(r.Action)) == 0 {
			return fmt.Errorf("%w: %s %s@%s", ErrInvalidPattern, r.Effect, r.Action, r.Object)
		}
		if r.Effect == Deny && !isOrgAdmin {
			return ErrDenyNotAllowed
		}
	}
	return e.CanHold(actor, org, rules)
}

// CanGrantRole checks a role's own rules with CanGrant and everything inherited from parents with CanHold.
func (e *Enforcer) CanGrantRole(actor string, org uuid.UUID, isOrgAdmin bool, rules []Rule, parents []string) error {
	if err := e.CanGrant(actor, org, isOrgAdmin, rules); err != nil {
		return err
	}
	roleOrg := make(map[uuid.UUID]uuid.UUID, len(e.policy.Roles))
	for _, cr := range e.policy.Roles {
		roleOrg[cr.ID] = cr.OrgID
	}
	for _, parent := range parents {
		if !validParent(parent, org, roleOrg) {
			return fmt.Errorf("%w: parent %q", ErrInvalidPattern, parent)
		}
	}
	inherited, err := e.EffectiveRules(parents, org)
	if err != nil {
		return fmt.Errorf("%w: %w", ErrInvalidPattern, err)
	}
	return e.CanHold(actor, org, inherited)
}

// CanHold fails unless actor holds every allowed action of rules, unblocked by its denies; objects are not pattern-checked.
func (e *Enforcer) CanHold(actor string, org uuid.UUID, rules []Rule) error {
	denies, err := e.DenyRulesFor(actor, org)
	if err != nil {
		return fmt.Errorf("authz: can hold: %w", err)
	}
	for _, r := range rules {
		if r.Effect != Allow {
			continue
		}
		actions := ExpandActions(r.Action)
		if len(actions) == 0 {
			return fmt.Errorf("%w: %s %s@%s", ErrInvalidPattern, r.Effect, r.Action, r.Object)
		}
		for _, a := range actions {
			// Allowed treats r.Object literally, so a wildcard grant needs a rule covering the whole prefix.
			if !e.Allowed(actor, org, r.Object, a) || slices.ContainsFunc(denies, func(d Rule) bool { return blocks(d, r.Object, a) }) {
				return fmt.Errorf("%w: %s@%s", ErrEscalation, a, r.Object)
			}
		}
	}
	return nil
}

// blocks reports whether deny d hits action act on any object the pattern obj may match.
func blocks(d Rule, obj, act string) bool {
	if !util.KeyMatch(act, d.Action) && !util.KeyMatch(d.Action, act) {
		return false
	}
	dp, op := strings.TrimSuffix(d.Object, "*"), strings.TrimSuffix(obj, "*")
	return strings.HasPrefix(dp, op) || strings.HasPrefix(op, dp)
}

// EffectiveRules returns the allow rules of built-in keys and org custom role IDs, parents included.
func (e *Enforcer) EffectiveRules(roleKeys []string, org uuid.UUID) ([]Rule, error) {
	var out []Rule
	seen := map[string]bool{}
	var visit func(key string) error
	visit = func(key string) error {
		if seen[key] {
			return nil
		}
		seen[key] = true
		if rules := BuiltinRules(key); rules != nil {
			out = append(out, rules...)
			return nil
		}
		i := slices.IndexFunc(e.policy.Roles, func(cr CustomRole) bool { return cr.OrgID == org && cr.ID.String() == key })
		if i < 0 {
			return fmt.Errorf("authz: no role %q in org %s", key, org)
		}
		cr := e.policy.Roles[i]
		for _, r := range cr.Rules {
			if r.Effect == Allow {
				out = append(out, r)
			}
		}
		for _, p := range cr.Parents {
			if err := visit(p); err != nil {
				return err
			}
		}
		return nil
	}
	for _, k := range roleKeys {
		if err := visit(k); err != nil {
			return nil, err
		}
	}
	return out, nil
}

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
	denies, err := e.DenyRulesFor(actor, org)
	if err != nil {
		return fmt.Errorf("authz: can grant: %w", err)
	}
	for _, r := range rules {
		if r.Effect != Allow {
			continue
		}
		for _, a := range ExpandActions(r.Action) {
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

// EffectiveRules returns the allow rules of built-in keys and org custom role IDs, parents included; unknown keys add nothing.
func (e *Enforcer) EffectiveRules(roleKeys []string, org uuid.UUID) []Rule {
	var out []Rule
	seen := map[string]bool{}
	var visit func(key string)
	visit = func(key string) {
		if seen[key] {
			return
		}
		seen[key] = true
		if rules := BuiltinRules(key); rules != nil {
			out = append(out, rules...)
			return
		}
		for _, cr := range e.policy.Roles {
			if cr.OrgID != org || cr.ID.String() != key {
				continue
			}
			for _, r := range cr.Rules {
				if r.Effect == Allow {
					out = append(out, r)
				}
			}
			for _, p := range cr.Parents {
				visit(p)
			}
		}
	}
	for _, k := range roleKeys {
		visit(k)
	}
	return out
}

package policy

import (
	"errors"
	"slices"

	"github.com/google/uuid"
)

var ErrEscalation = errors.New("grant exceeds the granter's permissions")

// CanGrant checks a role rule or binding taking effect in acting against the granter's own rights.
func CanGrant(r Rights, acting uuid.UUID, g Rule) error {
	if err := g.Validate(); err != nil {
		return err
	}
	// Who may write denies is a role check elsewhere; a deny never widens access.
	if g.Effect == Deny {
		return nil
	}
	v := Vars{UserID: r.UserID, UserEmail: r.UserEmail, Account: acting}
	held := r.Accounts[acting]
	for _, a := range g.Actions {
		for _, res := range g.Resources {
			res = Substitute(res, v)
			if !slices.ContainsFunc(held, func(h Rule) bool { return holds(h, a, res, v) }) {
				return ErrEscalation
			}
			if slices.ContainsFunc(held, func(h Rule) bool { return blocks(h, a, res, v) }) {
				return ErrEscalation
			}
		}
	}
	return nil
}

// CanShare checks a share of owner's resources; ${account} in the rule means the owner.
func CanShare(r Rights, owner uuid.UUID, s Rule) error { return CanGrant(r, owner, s) }

// holds counts only unconditional allows: a conditional one might not apply where the grant does.
func holds(h Rule, action, res string, v Vars) bool {
	return h.Effect == Allow && len(h.Conditions) == 0 &&
		slices.ContainsFunc(h.Actions, func(p string) bool { return CoversAction(p, action) }) &&
		slices.ContainsFunc(h.Resources, func(p string) bool { return CoversResource(Substitute(p, v), res) })
}

func blocks(h Rule, action, res string, v Vars) bool {
	return h.Effect == Deny &&
		slices.ContainsFunc(h.Actions, func(p string) bool { return OverlapsAction(p, action) }) &&
		slices.ContainsFunc(h.Resources, func(p string) bool { return OverlapsResource(Substitute(p, v), res) })
}

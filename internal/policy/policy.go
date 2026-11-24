package policy

import (
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/google/uuid"
)

type Effect string

const (
	Allow Effect = "allow"
	Deny  Effect = "deny"
)

type Rule struct {
	Effect     Effect      `json:"effect"`
	Actions    []string    `json:"actions"`
	Resources  []string    `json:"resources"`
	Conditions []Condition `json:"conditions,omitempty"`
}

// Share is a rule an owner account attaches to its resources for whole grantee accounts.
type Share struct {
	Owner    uuid.UUID   `json:"owner"`
	Grantees []uuid.UUID `json:"grantees"`
	Rule     Rule        `json:"rule"`
}

// Rights is the caller token's content: flattened binding rules per account plus the shares granting those accounts.
type Rights struct {
	UserID    string               `json:"uid"`
	UserEmail string               `json:"email"`
	Accounts  map[uuid.UUID][]Rule `json:"accounts"`
	Shares    []Share              `json:"shares,omitempty"`
}

type Request struct {
	Acting   uuid.UUID
	Action   string
	Resource ID
	Context  Context
}

type Decision struct {
	Allowed bool
	Reason  string
}

func (r Rule) Validate() error {
	if r.Effect != Allow && r.Effect != Deny {
		return fmt.Errorf("unknown effect %q", r.Effect)
	}
	if len(r.Actions) == 0 || len(r.Resources) == 0 {
		return errors.New("a rule needs actions and resources")
	}
	placeholders := strings.NewReplacer("${obs:UserId}", "v", "${obs:UserEmail}", "v", "${account}", "v")
	for _, p := range r.Resources {
		if parts := strings.Split(placeholders.Replace(p), ":"); len(parts) != 4 || parts[0] != "obs" {
			return fmt.Errorf("invalid resource pattern %q", p)
		}
	}
	for _, c := range r.Conditions {
		if err := c.Validate(); err != nil {
			return err
		}
	}
	return nil
}

// matches reports whether the rule applies; a condition that cannot be evaluated makes a deny apply and an allow not.
func (r Rule) matches(q Request, ctx Context, v Vars) bool {
	if !slices.ContainsFunc(r.Actions, func(p string) bool { return MatchAction(p, q.Action) }) {
		return false
	}
	id := q.Resource.String()
	if !slices.ContainsFunc(r.Resources, func(p string) bool { return MatchResource(Substitute(p, v), id) }) {
		return false
	}
	for _, c := range r.Conditions {
		ok, err := c.Eval(ctx, v)
		if err != nil {
			return r.Effect == Deny
		}
		if !ok {
			return false
		}
	}
	return true
}

func Decide(r Rights, q Request) Decision {
	a, b := q.Acting, q.Resource.Account
	ctx := maps.Clone(q.Context)
	if ctx == nil {
		ctx = Context{}
	}
	// Identity keys always come from the rights, never from the caller-supplied context.
	ctx["obs:UserId"] = []string{r.UserID}
	ctx["obs:UserEmail"] = []string{r.UserEmail}
	ctx["obs:CallerAccount"] = []string{a.String()}
	ctx["obs:ResourceAccount"] = []string{b.String()}

	idVars := Vars{UserID: r.UserID, UserEmail: r.UserEmail, Account: a}
	ownerVars := idVars
	ownerVars.Account = b
	var shares []Rule
	if a != b {
		for _, s := range r.Shares {
			if s.Owner == b && slices.Contains(s.Grantees, a) {
				shares = append(shares, s.Rule)
			}
		}
	}

	identity := r.Accounts[a]
	idAllow, shareAllow := false, false
	for _, rule := range identity {
		if rule.matches(q, ctx, idVars) {
			if rule.Effect == Deny {
				return Decision{Reason: "deny"}
			}
			idAllow = true
		}
	}
	for _, rule := range shares {
		if rule.matches(q, ctx, ownerVars) {
			if rule.Effect == Deny {
				return Decision{Reason: "deny"}
			}
			shareAllow = true
		}
	}
	switch {
	case !idAllow:
		return Decision{Reason: "no-allow"}
	case a == b:
		return Decision{Allowed: true, Reason: "allow"}
	case !shareAllow:
		return Decision{Reason: "no-share"}
	default:
		return Decision{Allowed: true, Reason: "cross-account-allow"}
	}
}

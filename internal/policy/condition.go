package policy

import (
	"fmt"
	"net/netip"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
)

type Condition struct {
	Op         string   `json:"op"`
	Key        string   `json:"key"`
	Values     []string `json:"values"`
	Quantifier string   `json:"quantifier,omitempty"`
}

// Context maps condition keys to their values; single-valued keys have one entry.
type Context map[string][]string

type Vars struct {
	UserID, UserEmail string
	Account           uuid.UUID
}

var conditionKeys = []string{
	"obs:CurrentTime", "obs:SourceIp", "obs:SecureTransport", "obs:UserId", "obs:UserEmail",
	"obs:CallerAccount", "obs:ResourceAccount", "obs:CreatedBy", "obs:TagKeys",
}

var negated = map[string]string{"StringNotEquals": "StringEquals", "StringNotLike": "StringLike", "NotIpAddress": "IpAddress"}

var positive = []string{"StringEquals", "StringLike", "DateLessThan", "DateGreaterThan", "IpAddress", "Bool", "Null"}

func Substitute(s string, v Vars) string {
	if !strings.Contains(s, "${") {
		return s
	}
	return strings.NewReplacer("${obs:UserId}", v.UserID, "${obs:UserEmail}", v.UserEmail, "${account}", v.Account.String()).Replace(s)
}

// literal stands in for * and ? inside substituted values; it never occurs in input, so they cannot act as wildcards.
const literal = '￿'

func SubstitutePattern(s string, v Vars) string {
	if !strings.Contains(s, "${") {
		return s
	}
	esc := strings.NewReplacer("*", string(literal), "?", string(literal))
	return Substitute(s, Vars{UserID: esc.Replace(v.UserID), UserEmail: esc.Replace(v.UserEmail), Account: v.Account})
}

func validKey(k string) bool {
	for _, p := range []string{"obs:ResourceTag/", "obs:RequestTag/"} {
		if tag, ok := strings.CutPrefix(k, p); ok {
			return tag != ""
		}
	}
	return slices.Contains(conditionKeys, k)
}

func (c Condition) Validate() error {
	base, isNeg := negated[c.Op]
	if !isNeg {
		base = c.Op
	}
	if !slices.Contains(positive, base) {
		return fmt.Errorf("unknown condition operator %q", c.Op)
	}
	if c.Quantifier != "" && c.Quantifier != "ForAnyValue" && c.Quantifier != "ForAllValues" {
		return fmt.Errorf("unknown quantifier %q", c.Quantifier)
	}
	if !validKey(c.Key) {
		return fmt.Errorf("unknown condition key %q", c.Key)
	}
	if len(c.Values) == 0 {
		return fmt.Errorf("condition %s on %s has no values", c.Op, c.Key)
	}
	for _, v := range c.Values {
		if base == "Null" {
			if v != "true" && v != "false" {
				return fmt.Errorf("Null takes true or false, not %q", v)
			}
			continue
		}
		if err := parseRule(base, v); err != nil {
			return err
		}
	}
	return nil
}

func parseRule(base, v string) error {
	var err error
	switch base {
	case "DateLessThan", "DateGreaterThan":
		_, err = time.Parse(time.RFC3339, v)
	case "IpAddress":
		_, err = parsePrefix(v)
	case "Bool":
		_, err = strconv.ParseBool(v)
	}
	if err != nil {
		return fmt.Errorf("%s: invalid value %q: %w", base, v, err)
	}
	return nil
}

func parsePrefix(v string) (netip.Prefix, error) {
	if strings.Contains(v, "/") {
		return netip.ParsePrefix(v)
	}
	a, err := netip.ParseAddr(v)
	if err != nil {
		return netip.Prefix{}, err
	}
	return netip.PrefixFrom(a, a.BitLen()), nil
}

// Eval reports whether the condition holds; an error means a value could not be parsed, and callers fail closed.
func (c Condition) Eval(ctx Context, v Vars) (bool, error) {
	if err := c.Validate(); err != nil {
		return false, err
	}
	want := make([]string, len(c.Values))
	for i, s := range c.Values {
		if c.Op == "StringLike" || c.Op == "StringNotLike" {
			want[i] = SubstitutePattern(s, v)
		} else {
			want[i] = Substitute(s, v)
		}
	}
	have := ctx[c.Key]
	if c.Op == "Null" {
		return slices.Contains(want, strconv.FormatBool(len(have) == 0)), nil
	}
	base, isNeg := negated[c.Op]
	if !isNeg {
		base = c.Op
	}
	if len(have) == 0 {
		return c.Quantifier == "ForAllValues" || isNeg && c.Quantifier == "", nil
	}
	all := c.Quantifier == "ForAllValues"
	for _, h := range have {
		ok, err := anyMatch(base, h, want)
		if err != nil {
			return false, err
		}
		if isNeg {
			ok = !ok
		}
		if ok != all {
			return ok, nil
		}
	}
	return all, nil
}

func anyMatch(base, have string, want []string) (bool, error) {
	for _, w := range want {
		ok, err := compare(base, have, w)
		if err != nil || ok {
			return ok, err
		}
	}
	return false, nil
}

func compare(base, have, want string) (bool, error) {
	switch base {
	case "StringEquals":
		return have == want, nil
	case "StringLike":
		return MatchAction(want, have), nil
	case "DateLessThan", "DateGreaterThan":
		h, err := time.Parse(time.RFC3339, have)
		if err != nil {
			return false, err
		}
		w, err := time.Parse(time.RFC3339, want)
		if err != nil {
			return false, err
		}
		if base == "DateLessThan" {
			return h.Before(w), nil
		}
		return h.After(w), nil
	case "IpAddress":
		h, err := netip.ParseAddr(have)
		if err != nil {
			return false, err
		}
		p, err := parsePrefix(want)
		if err != nil {
			return false, err
		}
		return p.Contains(h), nil
	case "Bool":
		h, err := strconv.ParseBool(have)
		if err != nil {
			return false, err
		}
		w, err := strconv.ParseBool(want)
		return err == nil && h == w, err
	}
	return false, fmt.Errorf("unknown operator %q", base)
}

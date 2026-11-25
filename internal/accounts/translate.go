package accounts

import (
	"strings"

	"github.com/google/uuid"

	"github.com/asterism-dev/observatory/internal/policy"
	"github.com/asterism-dev/observatory/internal/store"
)

// Translate is the account-model request for an old check; false if it has none or the resource is unknown.
func Translate(s store.AccountSnapshot, org uuid.UUID, obj, act string) (policy.Request, bool) {
	actions, k, ok := newAction(act)
	if !ok || len(actions) != 1 {
		return policy.Request{}, false
	}
	prefix, rest := obj, ""
	if i := strings.LastIndex(obj, "/"); i > len("org") {
		if _, known := objectMap[obj[:i]]; known {
			prefix, rest = obj[:i], obj[i+1:]
		}
	}
	m, ok := objectMap[prefix]
	if !ok || m.kind != k {
		return policy.Request{}, false
	}
	var acc uuid.UUID
	id := "-"
	switch {
	case rest == "" || m.scope == "user":
		acc, ok = s.Default(org)
	case m.scope == "team":
		team, err := uuid.Parse(rest)
		if err != nil {
			return policy.Request{}, false
		}
		acc, ok = s.OfTeam(team)
	default:
		acc, ok = s.Resources[m.typ+"/"+rest]
		id = rest
	}
	if !ok {
		return policy.Request{}, false
	}
	return policy.Request{Acting: acc, Action: actions[0], Resource: policy.NewID(m.service, acc, m.typ, id)}, true
}

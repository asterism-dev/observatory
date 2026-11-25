package policy

import (
	"testing"

	"github.com/google/uuid"
)

func TestBuiltinRoles(t *testing.T) {
	q := func(action string, id ID) Request { return Request{Acting: accA, Action: action, Resource: id} }
	vmX, acct := vm(accA, "x"), NewID("iam", accA, "binding", "-")
	rights := func(key string) Rights { return Rights{Accounts: map[uuid.UUID][]Rule{accA: BuiltinRole(key)}} }
	for _, c := range []struct {
		role, action string
		id           ID
		want         bool
	}{
		{"viewer", "compute:vm:read", vmX, true}, {"viewer", "compute:vm:start", vmX, false},
		{"member", "compute:vm:connect", vmX, true}, {"member", "compute:disk:use", NewID("compute", accA, "disk", "d"), true},
		{"member", "iam:binding:create", acct, false},
		{"admin", "iam:binding:create", acct, true}, {"admin", "compute:vm:fork", vmX, true},
	} {
		if got := Decide(rights(c.role), q(c.action, c.id)).Allowed; got != c.want {
			t.Errorf("%s %s = %v", c.role, c.action, got)
		}
	}
	if BuiltinRole("owner") != nil {
		t.Error("unknown key must return nil")
	}
}

func TestBuiltinRolesNeedShareAcrossAccounts(t *testing.T) {
	r := Rights{Accounts: map[uuid.UUID][]Rule{accA: BuiltinRole("member")}}
	node := NewID("compute", accB, "node", "n1")
	q := Request{Acting: accA, Action: "compute:node:use", Resource: node}
	if d := Decide(r, q); d.Reason != "no-share" {
		t.Errorf("without a share = %+v, want no-share", d)
	}
	r.Shares = []Share{{Owner: accB, Grantees: []uuid.UUID{accA}, Rule: Rule{Effect: Allow, Actions: []string{"compute:node:use"}, Resources: []string{"obs:compute:${account}:node/*"}}}}
	if d := Decide(r, q); !d.Allowed {
		t.Errorf("with a share = %+v", d)
	}
}

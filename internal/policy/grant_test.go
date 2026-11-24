package policy

import (
	"errors"
	"testing"

	"github.com/google/uuid"
)

func TestCanGrant(t *testing.T) {
	held := Rights{UserID: "u1", Accounts: map[uuid.UUID][]Rule{accA: {
		allow([]string{"compute:vm:*"}, []string{"obs:compute:${account}:vm/*"}),
		allow([]string{"compute:vm:connect"}, mine, Condition{Op: "StringEquals", Key: "obs:CreatedBy", Values: []string{"${obs:UserId}"}}),
		deny([]string{"compute:vm:delete"}, []string{"obs:compute:${account}:vm/prod"}),
	}}}
	ok := []Rule{
		allow([]string{"compute:vm:start"}, []string{"obs:compute:${account}:vm/x"}),
		allow([]string{"compute:vm:start"}, []string{"obs:compute:${account}:vm/*"}, Condition{Op: "Bool", Key: "obs:SecureTransport", Values: []string{"true"}}),
		deny([]string{"*"}, mine),
		// vm:* is held unconditionally, so connect is grantable even though a conditional allow also exists.
		allow([]string{"compute:vm:connect"}, []string{"obs:compute:${account}:vm/x"}),
	}
	for _, g := range ok {
		if err := CanGrant(held, accA, g); err != nil {
			t.Errorf("CanGrant(%+v) = %v", g, err)
		}
	}
	bad := []Rule{
		allow([]string{"compute:disk:read"}, []string{"obs:compute:${account}:disk/*"}),         // not held
		allow([]string{"compute:vm:start"}, mine),                                               // wider resource
		allow([]string{"compute:vm:delete"}, []string{"obs:compute:${account}:vm/*"}),           // overlaps own deny
		allow([]string{"compute:vm:start"}, []string{"obs:compute:" + accB.String() + ":vm/*"}), // other account
	}
	for i, g := range bad {
		if err := CanGrant(held, accA, g); !errors.Is(err, ErrEscalation) {
			t.Errorf("bad[%d] CanGrant(%+v) = %v, want ErrEscalation", i, g, err)
		}
	}
	onlyConditional := Rights{UserID: "u1", Accounts: map[uuid.UUID][]Rule{accA: {
		allow([]string{"compute:vm:connect"}, mine, Condition{Op: "StringEquals", Key: "obs:CreatedBy", Values: []string{"${obs:UserId}"}}),
	}}}
	if err := CanGrant(onlyConditional, accA, allow([]string{"compute:vm:connect"}, []string{"obs:compute:${account}:vm/x"})); !errors.Is(err, ErrEscalation) {
		t.Errorf("conditional allow counted as held: %v", err)
	}
}

func TestCanShare(t *testing.T) {
	held := Rights{Accounts: map[uuid.UUID][]Rule{accB: {allow([]string{"*"}, mine)}}}
	if err := CanShare(held, accB, allow([]string{"compute:node:use"}, []string{"obs:compute:${account}:node/*"})); err != nil {
		t.Errorf("owner admin sharing: %v", err)
	}
	if err := CanShare(held, accA, allow([]string{"compute:node:use"}, []string{"obs:compute:${account}:node/*"})); !errors.Is(err, ErrEscalation) {
		t.Errorf("sharing an account without bindings: %v", err)
	}
}

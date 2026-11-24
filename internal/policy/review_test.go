package policy

import (
	"errors"
	"testing"

	"github.com/google/uuid"
)

func TestCanGrantUserVariableIsAnyUser(t *testing.T) {
	held := Rights{UserID: "u1", Accounts: map[uuid.UUID][]Rule{accA: {
		allow([]string{"storage:bucket:*"}, []string{"obs:storage:${account}:bucket/u1"}),
	}}}
	g := allow([]string{"storage:bucket:read"}, []string{"obs:storage:${account}:bucket/${obs:UserId}"})
	if err := CanGrant(held, accA, g); !errors.Is(err, ErrEscalation) {
		t.Fatalf("granting every user their own bucket from holding one: %v", err)
	}
}

func TestDecideUnknownEffectNeverAllows(t *testing.T) {
	r := Rights{Accounts: map[uuid.UUID][]Rule{accA: {{Effect: "Deny", Actions: []string{"*"}, Resources: mine}, allow([]string{"*"}, mine)}}}
	if d := Decide(r, Request{Acting: accA, Action: "compute:vm:start", Resource: vm(accA, "x")}); d.Allowed {
		t.Fatalf("unknown effect widened access: %+v", d)
	}
}

func TestDecideDenyOnMissingTime(t *testing.T) {
	r := Rights{Accounts: map[uuid.UUID][]Rule{accA: {
		allow([]string{"*"}, mine),
		deny([]string{"*"}, mine, Condition{Op: "DateGreaterThan", Key: "obs:CurrentTime", Values: []string{"2020-01-01T00:00:00Z"}}),
	}}}
	if Decide(r, Request{Acting: accA, Action: "compute:vm:start", Resource: vm(accA, "x")}).Allowed {
		t.Error("time-based deny skipped without obs:CurrentTime in context")
	}
}

func TestDecideDenyOnMissingSourceIp(t *testing.T) {
	r := Rights{Accounts: map[uuid.UUID][]Rule{accA: {
		allow([]string{"*"}, mine),
		deny([]string{"*"}, mine, Condition{Op: "IpAddress", Key: "obs:SourceIp", Values: []string{"10.0.0.0/8"}}),
	}}}
	if Decide(r, Request{Acting: accA, Action: "compute:vm:start", Resource: vm(accA, "x")}).Allowed {
		t.Error("IP-based deny skipped without obs:SourceIp in context")
	}
}

func TestVariablesMatchLiterally(t *testing.T) {
	r := Rights{UserEmail: "?ob@corp.com", Accounts: map[uuid.UUID][]Rule{accA: {
		allow([]string{"*"}, mine, Condition{Op: "StringLike", Key: "obs:ResourceTag/owner", Values: []string{"${obs:UserEmail}"}}),
	}}}
	q := Request{Acting: accA, Action: "compute:vm:start", Resource: vm(accA, "x"), Context: Context{"obs:ResourceTag/owner": {"bob@corp.com"}}}
	if Decide(r, q).Allowed {
		t.Error("a ? in the caller's email acted as a wildcard")
	}
}

func TestCanGrantUnknownEffectBlocks(t *testing.T) {
	held := Rights{Accounts: map[uuid.UUID][]Rule{accA: {allow([]string{"*"}, mine), {Effect: "Deny", Actions: []string{"*"}, Resources: mine}}}}
	if err := CanGrant(held, accA, allow([]string{"compute:vm:start"}, mine)); !errors.Is(err, ErrEscalation) {
		t.Fatalf("held rule with unknown effect did not block: %v", err)
	}
}

func TestForAnyValueNegatedOnAbsentKey(t *testing.T) {
	c := Condition{Op: "StringNotEquals", Key: "obs:TagKeys", Values: []string{"x"}, Quantifier: "ForAnyValue"}
	if ok, err := c.Eval(Context{}, Vars{}); ok || err != nil {
		t.Fatalf("ForAnyValue on an absent key = %v, %v; want false", ok, err)
	}
}

func TestDecideRejectsInvalidResource(t *testing.T) {
	r := Rights{Accounts: map[uuid.UUID][]Rule{accA: {allow([]string{"*"}, []string{"obs:*:${account}:*"})}}}
	if d := Decide(r, Request{Acting: accA, Action: "compute:vm:start", Resource: NewID("compute", accA, "vm", "x:y")}); d.Reason != "invalid-resource" {
		t.Fatalf("Decide = %+v, want invalid-resource", d)
	}
}

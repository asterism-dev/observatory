package policy

import (
	"encoding/json"
	"reflect"
	"testing"

	"github.com/google/uuid"
)

var (
	accA = uuid.MustParse("6f1c2b1e-0a4b-4c1e-9a77-1f2e3d4c5b6a")
	accB = uuid.MustParse("0d9e8f7a-6b5c-4d3e-8f2a-1b0c9d8e7f6a")
	mine = []string{"obs:*:${account}:*"}
)

func vm(acc uuid.UUID, id string) ID { return NewID("compute", acc, "vm", id) }
func allow(actions, resources []string, cs ...Condition) Rule {
	return Rule{Allow, actions, resources, cs}
}
func deny(actions, resources []string, cs ...Condition) Rule {
	return Rule{Deny, actions, resources, cs}
}

func TestDecideSameAccount(t *testing.T) {
	r := Rights{UserID: "u1", Accounts: map[uuid.UUID][]Rule{accA: {
		allow([]string{"compute:vm:*"}, mine),
		deny([]string{"compute:vm:delete"}, []string{"obs:compute:${account}:vm/prod"}),
	}}}
	check := func(action, id string, want string) {
		t.Helper()
		if d := Decide(r, Request{Acting: accA, Action: action, Resource: vm(accA, id)}); d.Reason != want {
			t.Errorf("%s %s = %+v, want %s", action, id, d, want)
		}
	}
	check("compute:vm:start", "x", "allow")
	check("compute:vm:delete", "prod", "deny")
	check("compute:disk:read", "x", "no-allow")
}

func TestDecideAccountVariableDoesNotLeak(t *testing.T) {
	r := Rights{Accounts: map[uuid.UUID][]Rule{accA: {allow([]string{"*"}, mine)}, accB: {allow([]string{"compute:vm:read"}, mine)}}}
	if d := Decide(r, Request{Acting: accA, Action: "compute:vm:delete", Resource: vm(accB, "x")}); d.Allowed {
		t.Fatalf("rule bound in A matched a resource in B: %+v", d)
	}
}

func TestDecideCrossAccount(t *testing.T) {
	share := Share{Owner: accB, Grantees: []uuid.UUID{accA}, Rule: allow([]string{"compute:node:use"}, []string{"obs:compute:${account}:node/*"})}
	idAllow := allow([]string{"compute:node:use"}, []string{"obs:compute:*:node/*"})
	node := NewID("compute", accB, "node", "n1")
	q := Request{Acting: accA, Action: "compute:node:use", Resource: node}
	if d := Decide(Rights{Accounts: map[uuid.UUID][]Rule{accA: {idAllow}}, Shares: []Share{share}}, q); d.Reason != "cross-account-allow" {
		t.Errorf("both sides: %+v", d)
	}
	if d := Decide(Rights{Accounts: map[uuid.UUID][]Rule{accA: {idAllow}}}, q); d.Reason != "no-share" {
		t.Errorf("no share: %+v", d)
	}
	if d := Decide(Rights{Accounts: map[uuid.UUID][]Rule{accA: {}}, Shares: []Share{share}}, q); d.Reason != "no-allow" {
		t.Errorf("no identity allow: %+v", d)
	}
	other := Share{Owner: accB, Grantees: []uuid.UUID{uuid.New()}, Rule: share.Rule}
	if d := Decide(Rights{Accounts: map[uuid.UUID][]Rule{accA: {idAllow}}, Shares: []Share{other}}, q); d.Allowed {
		t.Errorf("share to another account applied: %+v", d)
	}
	shareDeny := Share{Owner: accB, Grantees: []uuid.UUID{accA}, Rule: deny([]string{"*"}, []string{"obs:compute:${account}:node/n1"})}
	if d := Decide(Rights{Accounts: map[uuid.UUID][]Rule{accA: {idAllow}}, Shares: []Share{share, shareDeny}}, q); d.Reason != "deny" {
		t.Errorf("share deny: %+v", d)
	}
}

func TestDecideRequiredTag(t *testing.T) {
	r := Rights{Accounts: map[uuid.UUID][]Rule{accA: {
		allow([]string{"compute:vm:create"}, mine),
		deny([]string{"compute:vm:create"}, mine, Condition{Op: "Null", Key: "obs:RequestTag/cost-center", Values: []string{"true"}}),
	}}}
	q := Request{Acting: accA, Action: "compute:vm:create", Resource: vm(accA, "new")}
	if Decide(r, q).Allowed {
		t.Error("untagged create allowed")
	}
	q.Context = Context{"obs:RequestTag/cost-center": {"42"}}
	if !Decide(r, q).Allowed {
		t.Error("tagged create denied")
	}
}

func TestDecideSourceIpCondition(t *testing.T) {
	r := Rights{Accounts: map[uuid.UUID][]Rule{accA: {allow([]string{"compute:vm:*"}, mine, Condition{Op: "IpAddress", Key: "obs:SourceIp", Values: []string{"10.0.0.0/8"}})}}}
	q := Request{Acting: accA, Action: "compute:vm:start", Resource: vm(accA, "x"), Context: Context{"obs:SourceIp": {"192.168.1.1"}}}
	if Decide(r, q).Allowed {
		t.Error("allowed from outside the range")
	}
}

func TestDecideDenyWithBadConditionStillDenies(t *testing.T) {
	r := Rights{Accounts: map[uuid.UUID][]Rule{accA: {
		allow([]string{"*"}, mine),
		deny([]string{"*"}, mine, Condition{Op: "NotIpAddress", Key: "obs:SourceIp", Values: []string{"10.0.0.0/8"}}),
	}}}
	q := Request{Acting: accA, Action: "compute:vm:start", Resource: vm(accA, "x"), Context: Context{"obs:SourceIp": {"garbage"}}}
	if Decide(r, q).Allowed {
		t.Error("condition error must not lift a deny")
	}
}

func TestDecideUserVariables(t *testing.T) {
	r := Rights{UserID: "u1", Accounts: map[uuid.UUID][]Rule{accA: {allow([]string{"compute:vm:connect"}, mine, Condition{Op: "StringEquals", Key: "obs:CreatedBy", Values: []string{"${obs:UserId}"}})}}}
	q := Request{Acting: accA, Action: "compute:vm:connect", Resource: vm(accA, "x"), Context: Context{"obs:CreatedBy": {"u2"}}}
	if Decide(r, q).Allowed {
		t.Error("CreatedBy of another user matched")
	}
	q.Context["obs:UserId"] = []string{"u2"} // a caller-supplied identity key must be overridden
	if Decide(r, q).Allowed {
		t.Error("context could spoof obs:UserId")
	}
}

func TestRightsJSONRoundTrip(t *testing.T) {
	r := Rights{UserID: "u1", UserEmail: "a@b.c", Accounts: map[uuid.UUID][]Rule{accA: {allow([]string{"*"}, mine)}},
		Shares: []Share{{Owner: accB, Grantees: []uuid.UUID{accA}, Rule: allow([]string{"compute:node:use"}, mine)}}}
	b, _ := json.Marshal(r)
	var got Rights
	if err := json.Unmarshal(b, &got); err != nil || !reflect.DeepEqual(got, r) {
		t.Fatalf("round trip: %v %+v", err, got)
	}
}

func TestRuleValidate(t *testing.T) {
	if err := allow([]string{"*"}, mine).Validate(); err != nil {
		t.Errorf("valid rule: %v", err)
	}
	for _, r := range []Rule{
		{Effect: "maybe", Actions: []string{"*"}, Resources: mine},
		allow(nil, mine),
		allow([]string{"*"}, nil),
		allow([]string{"*"}, []string{"compute:vm/*"}),
		allow([]string{"*"}, mine, Condition{Op: "Nope", Key: "obs:UserId", Values: []string{"x"}}),
	} {
		if r.Validate() == nil {
			t.Errorf("Validate accepted %+v", r)
		}
	}
}

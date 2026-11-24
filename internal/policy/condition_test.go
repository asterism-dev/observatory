package policy

import "testing"

func TestConditionOperators(t *testing.T) {
	ctx := Context{"obs:SourceIp": {"10.1.2.3"}, "obs:CurrentTime": {"2026-11-24T12:00:00Z"},
		"obs:SecureTransport": {"true"}, "obs:ResourceTag/env": {"prod"}, "obs:TagKeys": {"env", "team"},
		"obs:CreatedBy": {"u1"}}
	v := Vars{UserID: "u1"}
	for _, c := range []struct {
		cond Condition
		want bool
	}{
		{Condition{Op: "StringEquals", Key: "obs:ResourceTag/env", Values: []string{"dev", "prod"}}, true},
		{Condition{Op: "StringNotEquals", Key: "obs:ResourceTag/env", Values: []string{"prod"}}, false},
		{Condition{Op: "StringLike", Key: "obs:ResourceTag/env", Values: []string{"pr*"}}, true},
		{Condition{Op: "StringEquals", Key: "obs:CreatedBy", Values: []string{"${obs:UserId}"}}, true},
		{Condition{Op: "IpAddress", Key: "obs:SourceIp", Values: []string{"10.0.0.0/8"}}, true},
		{Condition{Op: "NotIpAddress", Key: "obs:SourceIp", Values: []string{"10.0.0.0/8"}}, false},
		{Condition{Op: "DateLessThan", Key: "obs:CurrentTime", Values: []string{"2027-01-01T00:00:00Z"}}, true},
		{Condition{Op: "Bool", Key: "obs:SecureTransport", Values: []string{"false"}}, false},
		{Condition{Op: "Null", Key: "obs:RequestTag/cost-center", Values: []string{"true"}}, true},
		{Condition{Op: "StringEquals", Key: "obs:RequestTag/cost-center", Values: []string{"x"}}, false},
		{Condition{Op: "StringNotEquals", Key: "obs:RequestTag/cost-center", Values: []string{"x"}}, true},
		{Condition{Op: "StringEquals", Key: "obs:TagKeys", Values: []string{"env", "team", "x"}, Quantifier: "ForAllValues"}, true},
		{Condition{Op: "StringEquals", Key: "obs:TagKeys", Values: []string{"env"}, Quantifier: "ForAllValues"}, false},
		{Condition{Op: "StringEquals", Key: "obs:TagKeys", Values: []string{"team"}, Quantifier: "ForAnyValue"}, true},
	} {
		got, err := c.cond.Eval(ctx, v)
		if err != nil || got != c.want {
			t.Errorf("%+v = %v, %v; want %v", c.cond, got, err, c.want)
		}
	}
}

func TestConditionErrorFailsClosed(t *testing.T) {
	_, err := Condition{Op: "IpAddress", Key: "obs:SourceIp", Values: []string{"10.0.0.0/8"}}.Eval(Context{"obs:SourceIp": {"nope"}}, Vars{})
	if err == nil {
		t.Fatal("bad context IP must error")
	}
}

func TestConditionValidate(t *testing.T) {
	for _, c := range []Condition{
		{Op: "StringEqualsIgnoreCase", Key: "obs:UserId", Values: []string{"a"}},
		{Op: "StringEquals", Key: "obs:Whatever", Values: []string{"a"}},
		{Op: "StringEquals", Key: "obs:ResourceTag/", Values: []string{"a"}},
		{Op: "IpAddress", Key: "obs:SourceIp", Values: []string{"300.1.1.1"}},
		{Op: "DateLessThan", Key: "obs:CurrentTime", Values: []string{"tomorrow"}},
		{Op: "Bool", Key: "obs:SecureTransport", Values: []string{"yes"}},
		{Op: "StringEquals", Key: "obs:UserId", Values: nil},
		{Op: "StringEquals", Key: "obs:UserId", Values: []string{"a"}, Quantifier: "ForSome"},
	} {
		if c.Validate() == nil {
			t.Errorf("Validate accepted %+v", c)
		}
	}
	if err := (Condition{Op: "IpAddress", Key: "obs:SourceIp", Values: []string{"10.0.0.0/8", "192.168.1.1"}}).Validate(); err != nil {
		t.Errorf("valid condition rejected: %v", err)
	}
}

package main

import (
	"strings"
	"testing"

	"github.com/asterism-dev/observatory/internal/proto"
)

func TestUnderlayRules(t *testing.T) {
	st := proto.GatewayState{
		Orgs: []proto.GatewayOrg{{ID: orgA}, {ID: orgB}, {ID: orgC}},
		Chassis: []proto.GatewayChassis{
			{NodeID: nodeA, OrgID: orgA, IP: "10.198.0.11"},
			{NodeID: "a2", OrgID: orgA, IP: "10.198.0.12"},
			{NodeID: nodeB, OrgID: orgB, IP: "10.198.0.21"},
		},
	}
	r := underlayRules(st)
	if !strings.HasPrefix(r, "table inet obs_underlay\ndelete table inet obs_underlay\ntable inet obs_underlay {") {
		t.Errorf("not an atomic replace:\n%s", r)
	}
	if strings.Count(r, "type ipv4_addr") != 3 {
		t.Errorf("want two org sets (org C has no chassis) and the quarantine set:\n%s", r)
	}
	for _, want := range []string{
		"set org_0 {\n\t\ttype ipv4_addr\n\t\telements = { 10.198.0.11, 10.198.0.12 }\n\t}",
		"set org_1 {\n\t\ttype ipv4_addr\n\t\telements = { 10.198.0.21 }\n\t}",
		"type filter hook forward priority 0; policy accept;",
		`iifname "wg-underlay" oifname "wg-underlay" ip saddr @org_0 ip daddr @org_0 accept`,
		`iifname "wg-underlay" oifname "wg-underlay" ip saddr @org_1 ip daddr @org_1 accept`,
	} {
		if !strings.Contains(r, want) {
			t.Errorf("missing %q in:\n%s", want, r)
		}
	}
	if strings.Contains(r, "org_1 {\n\t\ttype ipv4_addr\n\t\telements = { 10.198.0.11") || strings.Contains(r, "10.198.0.11, 10.198.0.21") {
		t.Errorf("org A node in org B set:\n%s", r)
	}
	if !strings.HasSuffix(strings.TrimSpace(r), "iifname \"wg-underlay\" drop\n\t}\n}") {
		t.Errorf("does not end with the drop:\n%s", r)
	}
}

func TestUnderlayRulesQuarantineControlException(t *testing.T) {
	st := proto.GatewayState{
		Orgs: []proto.GatewayOrg{{ID: orgA}},
		Chassis: []proto.GatewayChassis{
			{NodeID: nodeA, OrgID: orgA, IP: "10.198.0.11", Quarantined: true},
			{NodeID: "a2", OrgID: orgA, IP: "10.198.0.12"},
		},
	}
	r := underlayRules(st)
	for _, want := range []string{
		"set quarantine {\n\t\ttype ipv4_addr\n\t\telements = { 10.198.0.11 }\n\t}",
		"type filter hook input priority 0; policy accept;\n\t\tiifname \"wg-underlay\" ip saddr @quarantine ip daddr 10.198.0.1 tcp dport 8091 accept\n\t\tiifname \"wg-underlay\" ip saddr @quarantine drop",
		"type filter hook output priority 0; policy accept;\n\t\toifname \"wg-underlay\" ip daddr @quarantine ip saddr 10.198.0.1 tcp sport 8091 ct state established accept\n\t\toifname \"wg-underlay\" ip daddr @quarantine drop",
		"type filter hook forward priority 0; policy accept;\n\t\tiifname \"wg-underlay\" ip saddr @quarantine drop\n\t\toifname \"wg-underlay\" ip daddr @quarantine drop\n",
	} {
		if !strings.Contains(r, want) {
			t.Errorf("missing %q in:\n%s", want, r)
		}
	}
	st.Chassis[0].Quarantined = false
	if r := underlayRules(st); !strings.Contains(r, "set quarantine {\n\t\ttype ipv4_addr\n\t}") {
		t.Errorf("empty quarantine set:\n%s", r)
	}
}

func TestUnderlayRulesGatewayExemption(t *testing.T) {
	st := proto.GatewayState{
		Orgs:           []proto.GatewayOrg{{ID: orgA}, {ID: orgB}},
		GatewayChassis: proto.GatewayChassisRef{Name: nodeA, IP: "10.198.0.11"},
		Chassis: []proto.GatewayChassis{
			{NodeID: nodeA, OrgID: orgA, IP: "10.198.0.11"},
			{NodeID: nodeB, OrgID: orgB, IP: "10.198.0.21"},
		},
	}
	want := []string{
		`iifname "wg-underlay" oifname "wg-underlay" ip saddr 10.198.0.11 udp dport 6081 accept`,
		`iifname "wg-underlay" oifname "wg-underlay" ip daddr 10.198.0.11 udp dport 6081 accept`,
		`iifname "wg-underlay" oifname "wg-underlay" ip saddr 10.198.0.11 udp sport 6081 accept`,
		`iifname "wg-underlay" oifname "wg-underlay" ip daddr 10.198.0.11 udp sport 6081 accept`,
	}
	r := underlayRules(st)
	quarantine := strings.Index(r, "oifname \"wg-underlay\" ip daddr @quarantine drop\n\t\t"+`iifname "wg-underlay" oifname`)
	final := strings.LastIndex(r, "iifname \"wg-underlay\" drop")
	for _, w := range want {
		if i := strings.Index(r, "\t\t"+w+"\n"); i < 0 || i < quarantine || quarantine < 0 || i > final {
			t.Errorf("missing %q after the quarantine drops and before the final drop in:\n%s", w, r)
		}
	}
	if strings.Count(r, "6081") != len(want) {
		t.Errorf("exemption beyond the gateway chassis IP:\n%s", r)
	}
	st.GatewayChassis = hubGW
	if r := underlayRules(st); strings.Contains(r, "6081") {
		t.Errorf("exemption in self-contained mode:\n%s", r)
	}
}

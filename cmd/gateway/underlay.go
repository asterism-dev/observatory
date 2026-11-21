package main

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/asterism-dev/observatory/internal/netaddr"
	"github.com/asterism-dev/observatory/internal/proto"
)

const geneveUDPPort = 6081

// underlayRules lets Geneve between nodes pass only within one org; traffic to the gateway itself is INPUT, not forward.
func underlayRules(s proto.GatewayState) string {
	byOrg := map[string][]string{}
	var quarantine []string
	for _, c := range s.Chassis {
		byOrg[c.OrgID] = append(byOrg[c.OrgID], c.IP)
		if c.Quarantined {
			quarantine = append(quarantine, c.IP)
		}
	}
	var sets, rules strings.Builder
	sets.WriteString("\tset quarantine {\n\t\ttype ipv4_addr\n")
	if len(quarantine) > 0 {
		fmt.Fprintf(&sets, "\t\telements = { %s }\n", strings.Join(quarantine, ", "))
	}
	sets.WriteString("\t}\n")
	gw, port := netaddr.GatewayUnderlay.String(), strconv.Itoa(netaddr.ControlPort)
	n := 0
	for _, o := range s.Orgs {
		ips := byOrg[o.ID]
		if len(ips) == 0 {
			continue
		}
		fmt.Fprintf(&sets, "\tset org_%d {\n\t\ttype ipv4_addr\n\t\telements = { %s }\n\t}\n", n, strings.Join(ips, ", "))
		fmt.Fprintf(&rules, "\t\tiifname \"wg-underlay\" oifname \"wg-underlay\" ip saddr @org_%d ip daddr @org_%d accept\n", n, n)
		n++
	}
	// Shared mode: the hub-host node relays Geneve for every org's devices, so it may reach nodes of other orgs.
	if gc := s.GatewayChassis; gc.Name != "hub-gw" {
		for _, r := range []string{"ip saddr " + gc.IP + " udp dport", "ip daddr " + gc.IP + " udp dport", "ip saddr " + gc.IP + " udp sport", "ip daddr " + gc.IP + " udp sport"} {
			fmt.Fprintf(&rules, "\t\tiifname \"wg-underlay\" oifname \"wg-underlay\" %s %d accept\n", r, geneveUDPPort)
		}
	}
	return "table inet obs_underlay\ndelete table inet obs_underlay\ntable inet obs_underlay {\n" + sets.String() +
		// A quarantined node gets nothing over the underlay except the control proxy, so it can still receive its update.
		"\tchain input {\n\t\ttype filter hook input priority 0; policy accept;\n" +
		"\t\tiifname \"wg-underlay\" ip saddr @quarantine ip daddr " + gw + " tcp dport " + port + " accept\n" +
		"\t\tiifname \"wg-underlay\" ip saddr @quarantine drop\n\t}\n" +
		"\tchain output {\n\t\ttype filter hook output priority 0; policy accept;\n" +
		"\t\toifname \"wg-underlay\" ip daddr @quarantine ip saddr " + gw + " tcp sport " + port + " ct state established accept\n" +
		"\t\toifname \"wg-underlay\" ip daddr @quarantine drop\n\t}\n" +
		"\tchain forward {\n\t\ttype filter hook forward priority 0; policy accept;\n" +
		"\t\tiifname \"wg-underlay\" ip saddr @quarantine drop\n\t\toifname \"wg-underlay\" ip daddr @quarantine drop\n" + rules.String() +
		"\t\tiifname \"wg-underlay\" drop\n\t}\n}\n"
}

func applyUnderlay(run runner, s proto.GatewayState) error {
	return withTempFile(underlayRules(s), func(path string) error {
		if _, err := run("nft", "-f", path); err != nil {
			return fmt.Errorf("nft underlay rules: %w", err)
		}
		return nil
	})
}

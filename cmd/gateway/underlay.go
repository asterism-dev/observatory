package main

import (
	"fmt"
	"strings"

	"github.com/asterism-dev/observatory/internal/proto"
)

// underlayRules lets Geneve between nodes pass only within one org; traffic to the gateway itself is INPUT, not forward.
func underlayRules(s proto.GatewayState) string {
	byOrg := map[string][]string{}
	for _, c := range s.Chassis {
		byOrg[c.OrgID] = append(byOrg[c.OrgID], c.IP)
	}
	var sets, rules strings.Builder
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
	return "table inet obs_underlay\ndelete table inet obs_underlay\ntable inet obs_underlay {\n" + sets.String() +
		"\tchain forward {\n\t\ttype filter hook forward priority 0; policy accept;\n" + rules.String() +
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

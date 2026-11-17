package main

import (
	"fmt"
	"net/netip"
	"regexp"
	"strings"

	"github.com/asterism-dev/observatory/internal/netaddr"
	"github.com/asterism-dev/observatory/internal/proto"
)

type runner func(args ...string) (string, error)

const (
	devicePool = "10.192.0.0/16"
	gatewayDNS = "10.192.0.1"
)

var dhcpTagRe = regexp.MustCompile(`obs-(org|network)=([^\s",}]+)`)

func pgName(org string) string { return "pg_" + strings.ReplaceAll(org, "-", "_") }

func parseSubnet(s string) (netip.Prefix, error) {
	p, err := netip.ParsePrefix(s)
	if err != nil || !p.Addr().Is4() {
		return netip.Prefix{}, fmt.Errorf("invalid subnet %q", s)
	}
	return p.Masked(), nil
}

func orgACLs(pg, router string, ports []proto.GatewayPort) [][]string {
	trusted := "{" + gatewayDNS + ", " + router + "}"
	acls := [][]string{
		{"acl-del", pg},
		{"acl-add", pg, "from-lport", "1002", "inport == @" + pg + " && (ip4.dst == " + trusted + " || (udp.src == 68 && udp.dst == 67))", "allow-related"},
		{"acl-add", pg, "from-lport", "1001", "inport == @" + pg + " && ip4", "drop"},
		{"acl-add", pg, "from-lport", "1001", "inport == @" + pg + " && ip6", "drop"},
		{"acl-add", pg, "to-lport", "1002", "outport == @" + pg + " && ip4.src == " + trusted, "allow-related"},
	}
	for _, p := range ports {
		if len(p.AllowFrom) > 0 {
			acls = append(acls, []string{"acl-add", pg, "to-lport", "1002",
				fmt.Sprintf("outport == %q && ip4.src == {%s}", p.Name, strings.Join(p.AllowFrom, ", ")), "allow-related"})
		}
	}
	return append(acls,
		[]string{"acl-add", pg, "to-lport", "1001", "outport == @" + pg + " && ip4", "drop"},
		[]string{"acl-add", pg, "to-lport", "1001", "outport == @" + pg + " && ip6", "drop"})
}

func reconcileNB(run runner, s proto.GatewayState) error {
	read := func(args ...string) (string, error) {
		out, err := run(append([]string{"ovn-nbctl"}, args...)...)
		if err != nil {
			return "", fmt.Errorf("ovn-nbctl %s: %w", strings.Join(args, " "), err)
		}
		return strings.TrimSpace(out), nil
	}
	names := func(table string) (map[string]bool, error) {
		out, err := read("--bare", "--columns=name", "list", table)
		set := map[string]bool{}
		for _, n := range strings.Fields(out) {
			set[n] = true
		}
		return set, err
	}
	// dhcpRow finds the DHCP options row tagged tag, creating it outside the transaction because its UUID is needed inside.
	dhcpRow := func(cidr, tag string) (string, error) {
		find := func() (string, error) {
			out, err := read("--bare", "--columns=_uuid", "find", "dhcp_options", "external_ids:"+tag)
			return strings.Join(strings.Fields(out), ""), err
		}
		uuid, err := find()
		if err != nil || uuid != "" {
			return uuid, err
		}
		if _, err := read("dhcp-options-create", cidr, tag); err != nil {
			return "", err
		}
		if uuid, err = find(); err == nil && uuid == "" {
			err = fmt.Errorf("dhcp options %s missing after create", tag)
		}
		return uuid, err
	}

	pgs, err := names("port_group")
	if err != nil {
		return err
	}

	m := "--may-exist"
	cmds := [][]string{
		{m, "lr-add", "lr-gw"},
		{m, "ls-add", "ls-transit"},
		{m, "lrp-add", "lr-gw", "lrp-transit", "02:00:00:c7:00:01", "10.199.0.1/30"},
		{m, "lsp-add", "ls-transit", "transit-lr"},
		{"lsp-set-type", "transit-lr", "router"},
		{"lsp-set-addresses", "transit-lr", "router"},
		{"lsp-set-options", "transit-lr", "router-port=lrp-transit"},
		{m, "lsp-add", "ls-transit", "gw0"},
		{"lsp-set-addresses", "gw0", "02:00:00:c7:00:02 10.199.0.2"},
		{"lsp-set-options", "gw0", "requested-chassis=hub-gw"},
		{"lrp-set-gateway-chassis", "lrp-transit", "hub-gw", "1"},
		{m, "lr-route-add", "lr-gw", devicePool, "10.199.0.2"},
	}
	wantLS := map[string]bool{"ls-transit": true}
	wantLSP := map[string]bool{"gw0": true}
	wantLRP := map[string]bool{"lrp-transit": true}
	wantPG := map[string]bool{}
	wantTag := map[string]bool{}
	addPort := func(sw, dhcp string, p proto.GatewayPort) {
		wantLSP[p.Name] = true
		addr := p.MAC + " " + p.IP
		cmds = append(cmds,
			[]string{m, "lsp-add", sw, p.Name},
			[]string{"lsp-set-addresses", p.Name, addr},
			[]string{"lsp-set-port-security", p.Name, addr},
			[]string{"lsp-set-dhcpv4-options", p.Name, dhcp},
			[]string{"lsp-set-options", p.Name, "requested-chassis=" + p.Chassis})
	}
	for _, o := range s.Orgs {
		slice, err := parseSubnet(o.Slice)
		if err != nil {
			return fmt.Errorf("org %s: %w", o.ID, err)
		}
		router := netaddr.Router(slice).String()
		mac := netaddr.MAC(netaddr.Router(slice))
		sw, lrp, lsp, pg := "ls-mgmt-"+o.ID, "lrp-mgmt-"+o.ID, "mgmt-lr-"+o.ID, pgName(o.ID)
		tag := "obs-org=" + o.ID
		dhcp, err := dhcpRow(slice.String(), tag)
		if err != nil {
			return err
		}
		wantLS[sw], wantLRP[lrp], wantPG[pg], wantTag[tag] = true, true, true, true
		cmds = append(cmds,
			[]string{m, "ls-add", sw},
			[]string{m, "lrp-add", "lr-gw", lrp, mac, fmt.Sprintf("%s/%d", router, slice.Bits())},
			[]string{m, "lsp-add", sw, lsp},
			[]string{"lsp-set-type", lsp, "router"},
			[]string{"lsp-set-addresses", lsp, "router"},
			[]string{"lsp-set-options", lsp, "router-port=" + lrp},
			[]string{"dhcp-options-set-options", dhcp, "server_id=" + router, "server_mac=" + mac, "lease_time=3600",
				"router=" + router, fmt.Sprintf("mtu=%d", s.GuestMTU), "classless_static_route={" + devicePool + "," + router + "}"})
		members := []string{}
		for _, p := range o.Ports {
			addPort(sw, dhcp, p)
			members = append(members, p.Name)
		}
		if !pgs[pg] {
			cmds = append(cmds, []string{"pg-add", pg})
		}
		if len(members) > 0 {
			cmds = append(cmds, append([]string{"pg-set-ports", pg}, members...))
		} else {
			cmds = append(cmds, []string{"clear", "port_group", pg, "ports"})
		}
		cmds = append(cmds, orgACLs(pg, router, o.Ports)...)

		for _, n := range o.Networks {
			subnet, err := parseSubnet(n.Subnet)
			if err != nil {
				return fmt.Errorf("network %s: %w", n.ID, err)
			}
			nsw, ntag := "ls-net-"+n.ID, "obs-network="+n.ID
			ndhcp, err := dhcpRow(subnet.String(), ntag)
			if err != nil {
				return err
			}
			wantLS[nsw], wantTag[ntag] = true, true
			server := netaddr.Router(subnet)
			cmds = append(cmds,
				[]string{m, "ls-add", nsw},
				[]string{"dhcp-options-set-options", ndhcp, "server_id=" + server.String(), "server_mac=" + netaddr.MAC(server),
					"lease_time=3600", fmt.Sprintf("mtu=%d", s.GuestMTU)})
			for _, p := range n.Ports {
				addPort(nsw, ndhcp, p)
			}
		}
	}

	// Stale objects are deleted in the same transaction, ports before their switches.
	var stale [][]string
	ports, err := read("--format=csv", "--data=bare", "--no-headings", "--columns=name,type", "list", "logical_switch_port")
	if err != nil {
		return err
	}
	for _, line := range strings.Split(ports, "\n") {
		if name, typ, _ := strings.Cut(strings.TrimSpace(line), ","); name != "" && typ == "" && !wantLSP[name] {
			stale = append(stale, []string{"lsp-del", name})
		}
	}
	lrps, err := names("logical_router_port")
	if err != nil {
		return err
	}
	for n := range lrps {
		if strings.HasPrefix(n, "lrp-mgmt-") && !wantLRP[n] {
			stale = append(stale, []string{"lrp-del", n})
		}
	}
	switches, err := names("logical_switch")
	if err != nil {
		return err
	}
	for n := range switches {
		if (strings.HasPrefix(n, "ls-mgmt-") || strings.HasPrefix(n, "ls-net-")) && !wantLS[n] {
			stale = append(stale, []string{"ls-del", n})
		}
	}
	for n := range pgs {
		if strings.HasPrefix(n, "pg_") && !wantPG[n] {
			stale = append(stale, []string{"pg-del", n})
		}
	}
	dhcpList, err := read("--format=csv", "--data=bare", "--no-headings", "--columns=_uuid,external_ids", "list", "dhcp_options")
	if err != nil {
		return err
	}
	for _, line := range strings.Split(dhcpList, "\n") {
		uuid, ext, _ := strings.Cut(strings.TrimSpace(line), ",")
		if mt := dhcpTagRe.FindStringSubmatch(ext); mt != nil && uuid != "" && !wantTag["obs-"+mt[1]+"="+mt[2]] {
			stale = append(stale, []string{"dhcp-options-del", uuid})
		}
	}
	cmds = append(cmds, stale...)

	// One transaction so a new port is never visible without port security, port group membership and the default-drop ACLs.
	args := []string{"ovn-nbctl"}
	for i, c := range cmds {
		if i > 0 {
			args = append(args, "--")
		}
		args = append(args, c...)
	}
	if _, err := run(args...); err != nil {
		return fmt.Errorf("ovn-nbctl transaction: %w", err)
	}
	return nil
}

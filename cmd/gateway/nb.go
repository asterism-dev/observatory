package main

import (
	"errors"
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
	gw0MAC     = "02:00:00:c7:00:02"
	gw0IP      = "10.199.0.2"
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

	tx := func(cmds [][]string) error {
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

	pgs, err := names("port_group")
	if err != nil {
		return err
	}

	// Wanted names come from the state alone, so a skipped org keeps its objects.
	wantLS := map[string]bool{"ls-transit": true}
	wantLSP := map[string]bool{"gw0": true}
	wantLRP := map[string]bool{"lrp-transit": true}
	wantPG := map[string]bool{}
	wantTag := map[string]bool{}
	for _, o := range s.Orgs {
		wantLS["ls-mgmt-"+o.ID], wantLRP["lrp-mgmt-"+o.ID], wantPG[pgName(o.ID)], wantTag["obs-org="+o.ID] = true, true, true, true
		for _, p := range o.Ports {
			wantLSP[p.Name] = true
		}
		for _, n := range o.Networks {
			wantLS["ls-net-"+n.ID], wantTag["obs-network="+n.ID] = true, true
			for _, p := range n.Ports {
				wantLSP[p.Name] = true
			}
		}
	}

	// Stale objects go first, ports before their switches, so a reused address never collides with a leftover port.
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
	var errs []error
	if len(stale) > 0 {
		if err := tx(stale); err != nil {
			errs = append(errs, fmt.Errorf("stale objects: %w", err))
		}
	}

	m := "--may-exist"
	hub := [][]string{
		{m, "lr-add", "lr-gw"},
		{m, "ls-add", "ls-transit"},
		{m, "lrp-add", "lr-gw", "lrp-transit", "02:00:00:c7:00:01", "10.199.0.1/30"},
		{m, "lsp-add", "ls-transit", "transit-lr"},
		{"lsp-set-type", "transit-lr", "router"},
		{"lsp-set-addresses", "transit-lr", "router"},
		{"lsp-set-options", "transit-lr", "router-port=lrp-transit"},
		{m, "lsp-add", "ls-transit", "gw0"},
		{"lsp-set-addresses", "gw0", gw0MAC + " " + gw0IP},
		{"lsp-set-options", "gw0", "requested-chassis=" + s.GatewayChassis.Name},
		{"lrp-set-gateway-chassis", "lrp-transit", s.GatewayChassis.Name, "1"},
		{m, "lr-route-add", "lr-gw", devicePool, gw0IP},
	}
	if s.GatewayChassis.Name != "hub-gw" {
		gcs, err := names("gateway_chassis")
		if err != nil {
			return errors.Join(append(errs, err)...)
		}
		if gcs["lrp-transit-hub-gw"] {
			hub = append(hub, []string{"lrp-del-gateway-chassis", "lrp-transit", "hub-gw"})
		}
	}
	if err := tx(append(hub, transitACLs(s)...)); err != nil {
		return errors.Join(append(errs, err)...)
	}

	for _, o := range s.Orgs {
		cmds, err := orgCmds(o, s.GuestMTU, pgs[pgName(o.ID)], dhcpRow)
		if err == nil {
			err = tx(cmds)
		}
		if err != nil {
			errs = append(errs, fmt.Errorf("org %s: %w", o.ID, err))
		}
	}
	return errors.Join(errs...)
}

// transitACLs let a device reach only its own org's router address; lr-gw would answer on all of them.
func transitACLs(s proto.GatewayState) [][]string {
	devices := map[string][]string{}
	for _, d := range s.Devices {
		devices[d.OrgID] = append(devices[d.OrgID], d.IP)
	}
	acls := [][]string{{"acl-del", "ls-transit"}}
	var routers []string
	for _, o := range s.Orgs {
		slice, err := parseSubnet(o.Slice)
		if err != nil {
			continue
		}
		r := netaddr.Router(slice).String()
		routers = append(routers, r)
		if d := devices[o.ID]; len(d) > 0 {
			acls = append(acls, []string{"acl-add", "ls-transit", "from-lport", "1002",
				fmt.Sprintf(`inport == "gw0" && ip4.dst == %s && ip4.src == {%s}`, r, strings.Join(d, ", ")), "allow"})
		}
	}
	if len(routers) > 0 {
		acls = append(acls, []string{"acl-add", "ls-transit", "from-lport", "1001",
			fmt.Sprintf(`inport == "gw0" && ip4.dst == {%s}`, strings.Join(routers, ", ")), "drop"})
	}
	return acls
}

// orgCmds is one org's transaction, so a new port is never visible without port security, port group membership and the default-drop ACLs.
func orgCmds(o proto.GatewayOrg, mtu int, pgExists bool, dhcpRow func(cidr, tag string) (string, error)) ([][]string, error) {
	slice, err := parseSubnet(o.Slice)
	if err != nil {
		return nil, err
	}
	m := "--may-exist"
	var cmds [][]string
	addPort := func(sw, dhcp string, p proto.GatewayPort) {
		addr := p.MAC + " " + p.IP
		cmds = append(cmds,
			[]string{m, "lsp-add", sw, p.Name},
			[]string{"lsp-set-addresses", p.Name, addr},
			[]string{"lsp-set-port-security", p.Name, addr},
			[]string{"lsp-set-dhcpv4-options", p.Name, dhcp},
			[]string{"lsp-set-options", p.Name, "requested-chassis=" + p.Chassis})
	}
	router := netaddr.Router(slice).String()
	mac := netaddr.MAC(netaddr.Router(slice))
	sw, lrp, lsp, pg := "ls-mgmt-"+o.ID, "lrp-mgmt-"+o.ID, "mgmt-lr-"+o.ID, pgName(o.ID)
	dhcp, err := dhcpRow(slice.String(), "obs-org="+o.ID)
	if err != nil {
		return nil, err
	}
	cmds = append(cmds,
		[]string{m, "ls-add", sw},
		[]string{m, "lrp-add", "lr-gw", lrp, mac, fmt.Sprintf("%s/%d", router, slice.Bits())},
		[]string{m, "lsp-add", sw, lsp},
		[]string{"lsp-set-type", lsp, "router"},
		[]string{"lsp-set-addresses", lsp, "router"},
		[]string{"lsp-set-options", lsp, "router-port=" + lrp},
		[]string{"dhcp-options-set-options", dhcp, "server_id=" + router, "server_mac=" + mac, "lease_time=3600",
			"router=" + router, fmt.Sprintf("mtu=%d", mtu), "classless_static_route={" + devicePool + "," + router + "}"})
	members := []string{}
	for _, p := range o.Ports {
		addPort(sw, dhcp, p)
		members = append(members, p.Name)
	}
	if !pgExists {
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
			return nil, fmt.Errorf("network %s: %w", n.ID, err)
		}
		nsw := "ls-net-" + n.ID
		ndhcp, err := dhcpRow(subnet.String(), "obs-network="+n.ID)
		if err != nil {
			return nil, err
		}
		server := netaddr.Router(subnet)
		cmds = append(cmds,
			[]string{m, "ls-add", nsw},
			[]string{"dhcp-options-set-options", ndhcp, "server_id=" + server.String(), "server_mac=" + netaddr.MAC(server),
				"lease_time=3600", fmt.Sprintf("mtu=%d", mtu)})
		for _, p := range n.Ports {
			addPort(nsw, ndhcp, p)
		}
	}
	return cmds, nil
}

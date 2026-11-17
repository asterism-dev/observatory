#!/bin/sh
set -eu

for mod in openvswitch wireguard; do
  [ -d /sys/module/$mod ] || { echo "the Docker host kernel lacks the $mod module" >&2; exit 1; }
done

# a restarted container keeps /var/run, so stale pidfiles would make ovn-ctl skip starting the daemons
rm -rf /var/run/ovn /var/run/openvswitch
mkdir -p /var/run/ovn /var/run/openvswitch /var/log/ovn /var/log/openvswitch

# no underlay forwarding until the agent installs the per-org rules
nft -f - <<'NFT'
table inet obs_underlay
delete table inet obs_underlay
table inet obs_underlay {
	chain forward {
		type filter hook forward priority 0; policy accept;
		iifname "wg-underlay" drop
	}
}
NFT

link() { ip link show "$1" >/dev/null 2>&1 || ip link add "$1" type wireguard; }
link wg-underlay
wg set wg-underlay listen-port 51820
ip addr replace 10.198.0.1/16 dev wg-underlay
ip link set wg-underlay up
link wg-clients
wg set wg-clients listen-port 51821
ip addr replace 10.192.0.1/16 dev wg-clients
ip link set wg-clients up

# NB and SB listen on unix sockets only; the agent adds the SB SSL remote for nodes
/usr/share/ovn/scripts/ovn-ctl start_northd \
  --db-nb-create-insecure-remote=no --db-sb-create-insecure-remote=no

[ -f /etc/openvswitch/conf.db ] || ovsdb-tool create /etc/openvswitch/conf.db /usr/share/openvswitch/vswitch.ovsschema
ovsdb-server --remote=punix:/var/run/openvswitch/db.sock --pidfile --detach --log-file /etc/openvswitch/conf.db
ovs-vsctl --no-wait init
ovs-vswitchd --pidfile --detach --log-file
echo hub-gw > /etc/openvswitch/system-id.conf
ovs-vsctl set open . external_ids:system-id=hub-gw \
  external_ids:ovn-remote=unix:/var/run/ovn/ovnsb_db.sock \
  external_ids:ovn-encap-type=geneve external_ids:ovn-encap-ip=10.198.0.1 \
  external_ids:ovn-bridge-datapath-type=system
ovn-controller --pidfile --detach --log-file unix:/var/run/openvswitch/db.sock

i=0
until ovs-vsctl br-exists br-int; do
  i=$((i + 1))
  [ $i -le 30 ] || { echo "ovn-controller did not create br-int" >&2; exit 1; }
  sleep 1
done
ovs-vsctl --may-exist add-port br-int gw0 -- set interface gw0 type=internal \
  mac='"02:00:00:c7:00:02"' external_ids:iface-id=gw0
ip addr replace 10.199.0.2/30 dev gw0
ip link set gw0 up
ip route replace 10.64.0.0/10 via 10.199.0.1

rule() { iptables -C "$@" 2>/dev/null || iptables -A "$@"; }
for proto in udp tcp; do
  rule INPUT -i wg-clients -d 10.192.0.1 -p $proto --dport 53 -j ACCEPT
done
rule INPUT -i wg-clients -j DROP
rule INPUT -i wg-underlay -p udp --dport 6081 -j ACCEPT
rule INPUT -i wg-underlay -p tcp --dport 6642 -j ACCEPT
rule INPUT -i wg-underlay -j DROP
for proto in udp tcp; do
  rule INPUT -i gw0 -s 10.64.0.0/10 -d 10.192.0.1 -p $proto --dport 53 -j ACCEPT
done
rule INPUT -i gw0 -j DROP
iptables -P FORWARD DROP
rule FORWARD -i wg-clients -o gw0 -j ACCEPT
# VMs may only answer device connections, never open their own
rule FORWARD -i gw0 -o wg-clients -m conntrack --ctstate ESTABLISHED,RELATED -j ACCEPT
# per-org filtering happens in the nft table obs_underlay
rule FORWARD -i wg-underlay -o wg-underlay -j ACCEPT

exec gateway -hub "${HUB_URL:-http://host.docker.internal:8090}"

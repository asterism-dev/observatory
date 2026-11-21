#!/bin/sh
set -eu

# set (shared mode): gw0 joins the OVS of the node on this host, whose chassis UUID this is
chassis=${OBSERVATORY_GATEWAY_CHASSIS:-}
if [ -n "$chassis" ]; then
  h='[0-9a-f]'
  case $chassis in
    $h$h$h$h$h$h$h$h-$h$h$h$h-$h$h$h$h-$h$h$h$h-$h$h$h$h$h$h$h$h$h$h$h$h) ;;
    *) echo "OBSERVATORY_GATEWAY_CHASSIS must be a lowercase node UUID" >&2; exit 1 ;;
  esac
fi

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

if [ -z "$chassis" ]; then
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
  db=/var/run/openvswitch/db.sock
  nobr="ovn-controller did not create br-int"
else
  db=/host-ovs/db.sock
  nobr="the node's OVS at /host-ovs has no br-int; is the node installed and running?"
fi
vsctl() { ovs-vsctl --db="unix:$db" "$@"; }

i=0
until [ -S "$db" ] && vsctl --timeout=5 br-exists br-int; do
  i=$((i + 1))
  [ $i -le 30 ] || { echo "$nobr" >&2; exit 1; }
  sleep 1
done
if [ -z "$chassis" ]; then
  vsctl --may-exist add-port br-int gw0 -- set interface gw0 type=internal \
    mac='"02:00:00:c7:00:02"' external_ids:iface-id=gw0
else
  id=$(vsctl --timeout=5 get open . external_ids:system-id 2>/dev/null | tr -d '"')
  [ "$id" = "$chassis" ] || { echo "the host OVS is chassis '$id', not OBSERVATORY_GATEWAY_CHASSIS $chassis" >&2; exit 1; }
  # ports of earlier containers; their links may outlive them in a netns that storage still holds
  for old in $(vsctl --timeout=5 --bare --columns=name find interface external_ids:iface-id=gw0); do
    vsctl --timeout=5 --if-exists del-port "$old"
  done
  ip link del gw0 2>/dev/null || true
  # a unique host end, so a surviving earlier one cannot block the move; a veth, as vswitchd loses moved internal ports
  br=gw0b$(od -An -N4 -tx1 /dev/urandom | tr -d ' \n')
  ip link add gw0 address 02:00:00:c7:00:02 type veth peer name "$br"
  # moving and raising in one request needs no setns, so NET_ADMIN is enough
  ip link set "$br" netns /hostns/net up
  vsctl --timeout=5 add-port br-int "$br" -- set interface "$br" external_ids:iface-id=gw0
  trap 'vsctl --timeout=5 --if-exists del-port "$br"; ip link del gw0 2>/dev/null || true' EXIT
fi
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
rule INPUT -i wg-underlay -p tcp --dport 8091 --syn -m connlimit --connlimit-above 16 --connlimit-mask 32 -j REJECT
rule INPUT -i wg-underlay -d 10.198.0.1 -p tcp --dport 8091 -j ACCEPT
rule INPUT -i wg-underlay -d 10.198.0.1 -p icmp --icmp-type echo-request -m limit --limit 10/second -j ACCEPT
rule INPUT -i wg-underlay -d 10.198.0.1 -p tcp --dport 10810:11809 -j ACCEPT
# SB, node control and storage exports only from the underlay, not from the container network either
for port in 6642 8091 10810:11809; do
  rule INPUT -i lo -p tcp --dport $port -j ACCEPT
  rule INPUT ! -i wg-underlay -p tcp --dport $port -j DROP
done
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

hub=${HUB_URL:-http://host.docker.internal:8090}
[ -n "$chassis" ] || exec gateway -hub "$hub"
# not exec: the EXIT trap must remove the veth from the host's br-int when the gateway stops
gateway -hub "$hub" &
pid=$!
trap 'kill -TERM "$pid" 2>/dev/null || true' TERM INT
status=0
# a trapped signal interrupts wait before the gateway has exited
while kill -0 "$pid" 2>/dev/null; do wait "$pid" && status=0 || status=$?; done
exit "$status"

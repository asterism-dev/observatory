#!/bin/sh
# Node installer served by the hub; re-running re-enrolls and keeps VMs and images under /var/lib/observatory-node.
set -eu

FIRECRACKER_VERSION=v1.18.0

usage() {
	echo "usage: install.sh --hub http://HOST:PORT --token TOKEN|--token-stdin [--memory-mb N]" >&2
	exit 2
}
die() {
	echo "install.sh: $*" >&2
	exit 1
}
say() { echo "==> $*"; }

HUB='' TOKEN='' MEMORY_MB=''
while [ $# -gt 0 ]; do
	case $1 in
	--hub | --token | --memory-mb)
		[ $# -ge 2 ] || usage
		case $1 in
		--hub) HUB=${2%/} ;;
		--token) TOKEN=$2 ;;
		--memory-mb) MEMORY_MB=$2 ;;
		esac
		shift 2
		;;
	--token-stdin)
		IFS= read -r TOKEN || true
		shift
		;;
	*) usage ;;
	esac
done
[ -n "$HUB" ] || usage
[ -n "$TOKEN" ] || usage
case $HUB in
http://*:[0-9]* | https://*:[0-9]*) ;;
*) die "--hub must be http://HOST:PORT, got '$HUB'" ;;
esac

[ "$(uname -s)/$(uname -m)" = Linux/x86_64 ] || die "nodes need an amd64 (x86_64) Linux host, this one is $(uname -s)/$(uname -m)"
SUDO=''
[ "$(id -u)" = 0 ] || SUDO=sudo
[ -c /dev/kvm ] || die "/dev/kvm is missing: nodes need KVM (enable virtualization or nested virtualization)"

TMP=$(mktemp -d)
trap 'rm -rf "$TMP"' EXIT
dl() { curl -fsSL --connect-timeout 10 --max-time 600 "$@"; }
sha256_check() { sha256sum -c "$@"; }

say "downloading the node from $HUB"
dl -o "$TMP/observatory-node" "$HUB/node/observatory-node" || die "downloading observatory-node failed"
dl -o "$TMP/SHA256SUMS" "$HUB/node/SHA256SUMS" || die "downloading SHA256SUMS failed"
grep '  observatory-node$' "$TMP/SHA256SUMS" >"$TMP/sums" || die "observatory-node is missing from SHA256SUMS"
(cd "$TMP" && sha256_check sums >/dev/null) || die "checksum mismatch in the downloaded node binary"

say "installing packages"
$SUDO apt-get update -qq
$SUDO env DEBIAN_FRONTEND=noninteractive apt-get install -y -qq openvswitch-switch ovn-host wireguard-tools qemu-utils nftables >/dev/null

if [ "$(firecracker --version 2>/dev/null | head -1)" != "Firecracker $FIRECRACKER_VERSION" ] ||
	[ "$(jailer --version 2>/dev/null | head -1)" != "Jailer $FIRECRACKER_VERSION" ]; then
	say "installing Firecracker $FIRECRACKER_VERSION"
	fc=firecracker-$FIRECRACKER_VERSION-x86_64.tgz
	url=https://github.com/firecracker-microvm/firecracker/releases/download/$FIRECRACKER_VERSION
	if ! { dl -o "$TMP/$fc" "$url/$fc" && dl -o "$TMP/$fc.sha256.txt" "$url/$fc.sha256.txt"; }; then
		die "downloading Firecracker failed"
	fi
	(cd "$TMP" && sha256_check "$fc.sha256.txt" >/dev/null) || die "Firecracker checksum mismatch"
	tar -xzf "$TMP/$fc" -C "$TMP"
	$SUDO install -m 0755 "$TMP/release-$FIRECRACKER_VERSION-x86_64/firecracker-$FIRECRACKER_VERSION-x86_64" /usr/local/bin/firecracker
	$SUDO install -m 0755 "$TMP/release-$FIRECRACKER_VERSION-x86_64/jailer-$FIRECRACKER_VERSION-x86_64" /usr/local/bin/jailer
fi

say "preparing the host network (bridge vmm0, NAT for 192.168.64.0/24)"
$SUDO modprobe nbd
echo nbd | $SUDO tee /etc/modules-load.d/observatory-node.conf >/dev/null
printf '[NetDev]\nName=vmm0\nKind=bridge\n' | $SUDO tee /etc/systemd/network/vmm0.netdev >/dev/null
printf '[Match]\nName=vmm0\n[Link]\nRequiredForOnline=no\n[Network]\nAddress=192.168.64.1/24\nConfigureWithoutCarrier=yes\n' |
	$SUDO tee /etc/systemd/network/vmm0.network >/dev/null
$SUDO systemctl enable --now systemd-networkd >/dev/null 2>&1
$SUDO networkctl reload
echo net.ipv4.ip_forward=1 | $SUDO tee /etc/sysctl.d/90-observatory-node.conf >/dev/null
$SUDO sysctl -q -w net.ipv4.ip_forward=1
$SUDO mkdir -p /etc/nftables.d
# Declaring then flushing the table makes loading idempotent and leaves other tables (e.g. Docker's) alone.
$SUDO tee /etc/nftables.d/observatory-node.nft >/dev/null <<'NFT'
table ip vmm {}
flush table ip vmm
table ip vmm {
  chain post {
    type nat hook postrouting priority 100;
    ip saddr 192.168.64.0/24 ip daddr != 192.168.64.0/24 masquerade
  }
}
NFT
grep -qs 'include "/etc/nftables.d/\*.nft"' /etc/nftables.conf ||
	echo 'include "/etc/nftables.d/*.nft"' | $SUDO tee -a /etc/nftables.conf >/dev/null
$SUDO systemctl enable nftables >/dev/null 2>&1
$SUDO nft -f /etc/nftables.d/observatory-node.nft

say "enrolling the node"
$SUDO install -m 0755 "$TMP/observatory-node" /usr/local/bin/observatory-node
set -- -config /etc/observatory-node/node.json -hub "$HUB" -token-stdin
[ -z "$MEMORY_MB" ] || set -- "$@" -memory-mb "$MEMORY_MB"
# The token goes in on stdin: sudo logs command lines.
printf '%s\n' "$TOKEN" | $SUDO /usr/local/bin/observatory-node enroll "$@"

$SUDO tee /etc/systemd/system/observatory-node.service >/dev/null <<'UNIT'
[Unit]
Description=Observatory node
Wants=network-online.target
After=network-online.target openvswitch-switch.service ovn-controller.service docker.service

[Service]
ExecStartPre=-/bin/sh -c 'iptables -L DOCKER-USER -n >/dev/null 2>&1 || exit 0; iptables -C DOCKER-USER -i vmm0 -j ACCEPT 2>/dev/null || iptables -I DOCKER-USER -i vmm0 -j ACCEPT; iptables -C DOCKER-USER -o vmm0 -m conntrack --ctstate RELATED,ESTABLISHED -j ACCEPT 2>/dev/null || iptables -I DOCKER-USER -o vmm0 -m conntrack --ctstate RELATED,ESTABLISHED -j ACCEPT'
ExecStart=/usr/local/bin/observatory-node -config /etc/observatory-node/node.json
Restart=always
RestartSec=5

[Install]
WantedBy=multi-user.target
UNIT
cursor=$($SUDO journalctl -u observatory-node -n 0 --show-cursor 2>/dev/null | sed -n 's/^-- cursor: //p')
$SUDO systemctl daemon-reload
$SUDO systemctl enable observatory-node >/dev/null 2>&1
$SUDO systemctl restart observatory-node

node_journal() {
	if [ -n "$cursor" ]; then
		$SUDO journalctl -u observatory-node --after-cursor "$cursor" "$@" 2>/dev/null
	else
		$SUDO journalctl -u observatory-node "$@" 2>/dev/null
	fi
}
i=0
while [ $i -lt 30 ]; do
	if node_journal -o cat | grep -q "connected to hub"; then
		say "node installed; it appears on the hub as online"
		exit 0
	fi
	i=$((i + 1))
	sleep 1
done
node_journal -n 20 -o cat >&2 || true
die "the node did not connect to $HUB within 30 s (journalctl -u observatory-node); the service keeps retrying, re-run the installer after fixing the cause"

# Nodes

A node is a Linux host that runs an org's VMs with Firecracker. It belongs to one org, holds its own credential and
keeps a WebSocket connection to the hub, which pushes the desired VMs.

## Requirements

- Linux on amd64 (x86_64) with systemd, `apt` (tested on Debian 13; other apt-based distributions such as Ubuntu 24.04 should work but are untested) and `curl`.
- KVM: `/dev/kvm` must exist. In a VM, enable nested virtualization.
- Root, or a user with `sudo`.
- The hub's URL reachable from the host, and internet access for packages, Firecracker and the Ubuntu cloud image.
- Outbound UDP to the gateway on port 51820 (the WireGuard underlay, see [network.md](network.md)).
- The `nbd` kernel module (standard on Debian and Ubuntu kernels) for disks attached from the storage server.
- RAM: without `--memory-mb` the node offers the host's RAM minus 4 GB to VMs. On a host with 4 GB or less the
  node refuses to start; pass `--memory-mb` there.
- Disk under `/var/lib/observatory-node`: about 5 GB for the image, plus each VM's root disk (sparse).

## Install

An org admin adds the node in the GUI (Nodes → Add node) or with `POST /api/orgs/{org}/nodes`. The hub returns a
one-line command with a single-use enrollment token that expires after 24 hours:

```sh
curl -fsSL http://hub.example:8090/node/install.sh | sudo sh -s -- --hub http://hub.example:8090 --token TOKEN
```

Append `--memory-mb N` to set the RAM the node offers to VMs. The installer prints `node installed` once the node
has connected; the node then shows `online` in the GUI.

The token is on the command line, so it can end up in shell history and in `ps`. `--token-stdin` reads it from
stdin instead, but that does not work with `curl | sh` (stdin is the script); download the script first:

```sh
curl -fsSLO http://hub.example:8090/node/install.sh
printf '%s\n' "$TOKEN" | sudo sh install.sh --hub http://hub.example:8090 --token-stdin
```

The hub serves the installer, the node binary and its `SHA256SUMS` itself (`make node` builds the binary; see the
README). Over plain HTTP the checksum only catches broken downloads, not tampering: whoever can change the binary can
change the checksum too. In production, put the hub behind a TLS reverse proxy and use its `https://` URL; the node
then connects with `wss://`.

## What the installer changes on the host

- **Packages** (apt): `openvswitch-switch`, `ovn-host`, `wireguard-tools`, `qemu-utils`, `nftables`;
  `openvswitch-switch` and `ovn-controller` are enabled.
- **Firecracker and jailer** (pinned version, checksum-verified from the Firecracker GitHub releases) in
  `/usr/local/bin/firecracker` and `/usr/local/bin/jailer`.
- **nbd** kernel module, loaded now and at boot via `/etc/modules-load.d/observatory-node.conf`.
- **Bridge `vmm0`** with `192.168.64.1/24`, managed by systemd-networkd (`/etc/systemd/network/vmm0.netdev`,
  `vmm0.network`); systemd-networkd is enabled.
- **IP forwarding** via `/etc/sysctl.d/90-observatory-node.conf`.
- **NAT** for `192.168.64.0/24`: nftables table `ip vmm` in `/etc/nftables.d/observatory-node.nft`, included from
  `/etc/nftables.conf`; the `nftables` service is enabled. Other tables (e.g. Docker's) are left alone.
  The same table drops traffic from `vmm0` to the underlay (`10.198.0.0/16`), so VMs cannot reach the gateway's
  services (southbound DB, storage exports) or other nodes through their node.
- **Docker hosts:** the service adds `DOCKER-USER` rules accepting traffic from and back to `vmm0`, so Docker's
  forward policy does not block the VMs.
- **Node binary** in `/usr/local/bin/observatory-node`, config in `/etc/observatory-node/node.json` (mode 0600, holds
  the credential), data in `/var/lib/observatory-node` (`vms/`, `images/`).
- **systemd unit** `observatory-node.service`, enabled and started; logs with `journalctl -u observatory-node`.
  It starts after `nftables.service` and has `MemorySwapMax=0` and `LimitCORE=0`, so host-bound secret values the
  node and its egress proxies hold in memory never reach swap or a core dump.

## Underlay

On its first connection the node sends its WireGuard public key and a certificate request; the hub assigns it an
address in `10.198.0.0/16` and returns the underlay config. The node then writes and manages:

- `/etc/wireguard/wg-underlay.conf` (`wg-quick@wg-underlay`, enabled), a tunnel to the gateway's UDP port 51820;
- `/etc/ovn/ovn-ca.pem` and `/etc/ovn/ovn-chassis-cert.pem`, with the key in `/var/lib/observatory-node/vms/ovn-key.pem`;
  the WireGuard key is `/var/lib/observatory-node/vms/chassis.key`;
- the OVS `system-id` (the node id), `ovn-remote=ssl:10.198.0.1:6642`, Geneve encapsulation over the underlay address,
  and restarts `ovn-controller` when this changes.

Each VM's management and network NICs are taps on `br-int`. The chassis cert is valid for one year; the hub renews
it while the node is connected once less than 30 days are left.

## VM addresses

Each VM gets a NAT address in `192.168.64.0/24`, shown as its IP in the GUI. It is reachable only from the node
host, e.g. `ssh ubuntu@192.168.64.2` with a key from your profile. VMs boot the Ubuntu 24.04 cloud image; set
`"firecracker": {"imageURL": "..."}` in `node.json` to use another one. Each VM also gets a management address in
its org's slice, reachable from devices as `<vm>.internal` ([network.md](network.md)).

## Disks

Disks attached to a VM ([disks.md](disks.md)) are connected with `qemu-nbd` to `/dev/nbdN` on the node and passed to
Firecracker as extra drives (`vdb`, `vdc`, … in attach order). `qemu-nbd` connects over the underlay to
`10.198.0.1:<port>` with TLS-PSK; the key is in `<vm dir>/nbd-<disk id>-tls/keys.psk` (0600). If the storage server
goes away, `qemu-nbd` holds the guest's I/O and reconnects for up to 7 days. The node reports the disks it still uses,
which lets the hub release a detached disk; while the node is offline its detached disks stay `releasing`.

## Egress proxy

VMs with host-bound secrets get an egress proxy on the node ([egress.md](egress.md)). At start the node recreates two
nftables tables, `bridge obs_egress` and `inet obs_egress`, which drops redirects left over from a previous run; other
tables are left alone. If it cannot load them it refuses to start.

- `bridge obs_egress` marks frames from each VM's NAT tap with `0x4f0000 | n` (n = last octet of the VM's NAT
  address).
- `inet obs_egress` redirects the VM's TCP to port 443 (IPv4, not to `192.168.64.0/24` or the node itself) to its
  proxy on `192.168.64.1:15000 + n`, accepts a proxy port only with that VM's mark and drops the range
  `15002–15253` otherwise.
- Proxies run as UID/GID `71000 + n`; for UIDs `71002–71253` the output chain rejects loopback, the node's own
  addresses and the internal ranges listed in [egress.md](egress.md#on-the-node), and allows DNS only to the
  nameservers in `/etc/resolv.conf` as read at node start (restart the node after they change).

The root-only Linux tests (`go test -run TestEgress ./internal/provider/linuxvm/`) recreate these tables too and add
addresses to `vmm0`: on a live node they wipe the running VMs' redirects until the node restarts. Run them in a
separate network namespace or on a host without VMs.

## Integrations

The node runs a token service for every VM it runs ([integrations.md](integrations.md)). The guest connects over
vsock to the host (CID 2, port 1051); Firecracker turns that into a connection to the Unix socket `v.sock_1051` in the
VM's jail root (`<vm dir>/jail/firecracker/vm/root/`). The node creates the socket before the VM boots or resumes,
owned by the VM's jail UID with mode 0600 (the jail root is 0755 inside the VM's 0700 directory), so another VM's
Firecracker cannot open it. The socket goes away with the jail, not when its listener closes. The guest agent's port
1050 never carries secrets.

- One JSON line per connection (`{"op":"token","kind":"github"}`), 4 KiB request limit, 2 s timeout, 16 concurrent
  connections per VM; answers `{"ok":true,"token":…,"expiresAt":…}` or `{"ok":false,"error":"not-connected"|"unavailable"}`.
- Tokens arrive in the desired list of the VM's node only and live in node memory: never in `vm.json`, the seed or
  status messages. After a node restart the service answers `unavailable` until the hub's first push.
- The node journal gets one line per request: VM id, kind, result (`token vm=<id> kind=github result=ok`), never the
  token.

## Re-enroll

Running the installer again is safe: it updates packages, Firecracker and the binary, and keeps `node.json` settings,
the VMs and the images. It needs a fresh enrollment token, because tokens are single use: regenerate the credential
on the node's page (`POST /api/orgs/{org}/nodes/{id}/credential`) and run the returned command. For an enrolled node
this revokes the old credential at once and disconnects it; it shows `offline` until the command has run. The node
keeps its id and its VMs, which are reconciled when it reconnects.

Use this after a lost or leaked credential, or to move the node to another hub URL.

Nodes installed before the VM network existed must be re-installed this way: the new binary sends its underlay keys on
register and joins the gateway. Their VMs get the management NIC on their next start.

Nodes installed before disks existed must be re-installed too: only the new installer adds the nftables rules that keep
VMs off the underlay (`vmm0` → `10.198.0.0/16` dropped). Without them a guest can reach the storage exports (it still
needs the export key) and the southbound DB through its node.

Nodes installed before hibernation existed should be re-installed as well: only the new unit has `KillMode=mixed` and
`TimeoutStopSec=300`, which let a service restart hibernate the VMs instead of killing them
([vm-lifecycle.md](vm-lifecycle.md#node-restart)).

Nodes installed before host-bound secrets existed should be re-installed too: the new unit keeps secret values off swap
and out of core dumps and starts after `nftables.service`.

## Remove

Delete the VMs on the node, then delete the node in the GUI; this revokes its credential. The host is not cleaned up:
stop and disable `observatory-node.service` and remove the files listed above if needed.

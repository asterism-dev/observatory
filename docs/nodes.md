# Nodes

A node is a Linux host that runs an org's VMs with Firecracker. It belongs to one org, holds its own credential and
keeps a WebSocket connection to the hub, which pushes the desired VMs.

## Requirements

- Linux on amd64 (x86_64) with systemd, `apt` (Debian 13 and Ubuntu 24.04 are tested) and `curl`.
- KVM: `/dev/kvm` must exist. In a VM, enable nested virtualization.
- Root, or a user with `sudo`.
- The hub's URL reachable from the host, and internet access for packages, Firecracker and the Ubuntu cloud image.
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

- **Packages** (apt): `openvswitch-switch`, `ovn-host`, `wireguard-tools`, `qemu-utils`, `nftables`. OVS/OVN are not
  used yet; they are installed so later networking needs no re-install.
- **Firecracker and jailer** (pinned version, checksum-verified from the Firecracker GitHub releases) in
  `/usr/local/bin/firecracker` and `/usr/local/bin/jailer`.
- **nbd** kernel module, loaded now and at boot via `/etc/modules-load.d/observatory-node.conf`.
- **Bridge `vmm0`** with `192.168.64.1/24`, managed by systemd-networkd (`/etc/systemd/network/vmm0.netdev`,
  `vmm0.network`); systemd-networkd is enabled.
- **IP forwarding** via `/etc/sysctl.d/90-observatory-node.conf`.
- **NAT** for `192.168.64.0/24`: nftables table `ip vmm` in `/etc/nftables.d/observatory-node.nft`, included from
  `/etc/nftables.conf`; the `nftables` service is enabled. Other tables (e.g. Docker's) are left alone.
- **Docker hosts:** the service adds `DOCKER-USER` rules accepting traffic from and back to `vmm0`, so Docker's
  forward policy does not block the VMs.
- **Node binary** in `/usr/local/bin/observatory-node`, config in `/etc/observatory-node/node.json` (mode 0600, holds
  the credential), data in `/var/lib/observatory-node` (`vms/`, `images/`).
- **systemd unit** `observatory-node.service`, enabled and started; logs with `journalctl -u observatory-node`.

Each VM gets a NAT address in `192.168.64.0/24`, shown as its IP in the GUI. It is reachable only from the node
host, e.g. `ssh ubuntu@192.168.64.2` with a key from your profile. VMs boot the Ubuntu 24.04 cloud image; set
`"firecracker": {"imageURL": "..."}` in `node.json` to use another one.

## Re-enroll

Running the installer again is safe: it updates packages, Firecracker and the binary, and keeps `node.json` settings,
the VMs and the images. It needs a fresh enrollment token, because tokens are single use: regenerate the credential
on the node's page (`POST /api/orgs/{org}/nodes/{id}/credential`) and run the returned command. For an enrolled node
this revokes the old credential at once and disconnects it; it shows `offline` until the command has run. The node
keeps its id and its VMs, which are reconciled when it reconnects.

Use this after a lost or leaked credential, or to move the node to another hub URL.

## Remove

Delete the VMs on the node, then delete the node in the GUI; this revokes its credential. The host is not cleaned up:
stop and disable `observatory-node.service` and remove the files listed above if needed.

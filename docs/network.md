# VM network

Every VM gets a management NIC on its org's virtual switch (OVN), in addition to the NAT NIC on its node. People reach
the VMs they may connect to from their own machines through a WireGuard gateway, resolve `<vm>.internal`, and attach
VMs to org networks for VM-to-VM traffic.

## Address plan

| Range | Use |
|---|---|
| `10.64.0.0/10` | management; each org gets a `/20` on its first VM or device (router `.1`, VMs from `.10`), at most 1 024 orgs |
| `10.192.0.0/16` | devices, one `/32` each; the gateway is `10.192.0.1` (DNS) |
| `10.198.0.0/16` | node underlay; the gateway is `10.198.0.1`, nodes from `10.198.0.10` |
| `10.199.0.0/30` | transit between the gateway and the router |
| `10.210.0.0/16` | automatic network subnets, one `/24` each, unique hub-wide |
| `192.168.64.0/24` | NAT NIC on each node, reachable only from that node, never routed |

An org's slice is never reused while the org exists. A custom network subnet must be private, `/16` to `/28`, and must
not overlap any range above or any other network of any org.

## Gateway

The gateway runs OVN northd, the northbound and southbound databases, its own ovn-controller (chassis `hub-gw`), two
WireGuard interfaces and a DNS server. It polls `GET /api/gateway/state` every 2 s and applies the state when it changes,
and at least every 60 s. Every 10 s it reports device handshakes, chassis and cleared bindings to
`POST /api/gateway/status`.

| Port | Use |
|---|---|
| UDP 51820 | `wg-underlay`, nodes (must be reachable from every node) |
| UDP 51821 | `wg-clients`, devices (must be reachable from every device) |
| TCP 8091 | node control proxy to the hub, bound to `10.198.0.1`, underlay only ([nodes.md](nodes.md#control-path)) |
| TCP 6642 | southbound DB, SSL only, bound to `10.198.0.1` (inside the underlay) |
| TCP 10810–11809 | storage NBD exports (TLS-PSK), bound to `10.198.0.1`, underlay only ([disks.md](disks.md)) |
| UDP/TCP 53 | DNS on `10.192.0.1`, for devices and VM management NICs |

The northbound DB listens on a unix socket only. On `wg-underlay` the gateway accepts only Geneve (UDP 6081), the
southbound DB (TCP 6642), the node control proxy (TCP 8091), the storage exports (TCP 10810–11809) and rate-limited ICMP echo to `10.198.0.1`, so nodes can ping it for diagnostics. ICMP
between nodes is not forwarded.

### Running it

Hub env (`deploy/dev/hub.env`):

- `OBSERVATORY_GATEWAY_TOKEN`: the gateway's credential, at least 32 characters. `make gateway-token` adds a random one
  if it is missing.
- `OBSERVATORY_STORAGE_TOKEN`: the storage server's credential, at least 32 characters. `make storage-token` adds a
  random one if it is missing.
- `OBSERVATORY_GATEWAY_HOST`: the address nodes and devices use to reach the gateway. It defaults to the host of
  `OBSERVATORY_PUBLIC_URL`, so that URL must not be `localhost` once real nodes or devices connect.
- `OBSERVATORY_SECRETS_KEY` is required: the gateway's WireGuard keys and the OVN CA are generated on first start and
  sealed with it.

```sh
make gateway-up      # builds the gateway and storage images and starts them with the dev compose (profile "gateway")
make gateway-down
```

The storage server runs as compose service `storage` in the gateway's network namespace, which the service `netns`
owns so that both keep it across restarts and crashes of either. Its NBD exports
(TCP 10810–11809) listen on `10.198.0.1` and are reachable from the underlay only; nothing is published on the host.
Disk files live in the `disks` volume. Put it on XFS (`mkfs.xfs -m reflink=1`) or btrfs for instant copies; other
filesystems fall back to a sparse copy. Operation and security of disks: [disks.md](disks.md).

Stopping the gateway does not cut the underlay: its WireGuard interfaces, peers and filter rules live in the `netns`
service's namespace and keep forwarding with the last state applied. To cut nodes and devices off, stop `netns`
(`docker compose stop netns`, which takes gateway and storage along). If Docker restarts `netns` on its own, run
`docker compose restart netns` so gateway and storage join its new namespace.

The container reads `OBSERVATORY_GATEWAY_TOKEN` and `HUB_URL` (default `http://host.docker.internal:8090`). For a
hub on another port, set `HUB_URL` in the shell or in `hub.env`. The gateway needs the `openvswitch` and `wireguard`
kernel modules on the Docker host; the compose `modules` service loads them.

Do not run the gateway on a host that is also a node: both would share the kernel's OVS datapath.

## Devices

A device belongs to one person in one org. Devices → Add device, or `observatory device create <name>`
([cli.md](cli.md#connecting)), returns the WireGuard config once (download or QR code).
The hub keeps only the public key, so a lost config means deleting the device and adding a new one.

```ini
[Interface]
PrivateKey = …
Address = 10.192.0.12/32
DNS = 10.192.0.1
MTU = 1300

[Peer]
PublicKey = <gateway key>
Endpoint = <gateway host>:51821
AllowedIPs = <org slice>, 10.192.0.1/32
PersistentKeepalive = 25
```

Import it with `wg-quick up <file>` or the WireGuard app, then `ssh dev@<vm>.internal`. Owners see and delete
their own devices; org admins see and delete every device of the org but never get a config. Removing a member from
the org deletes their devices in it; deleting a device cuts its traffic within one gateway poll.

On macOS:

- If `<vm>.internal` does not resolve while the tunnel is up, another resolver answers first: a VPN such as Tailscale
  or an older `/etc/resolver/internal`. Route the domain to the gateway with
  `echo "nameserver 10.192.0.1" | sudo tee /etc/resolver/internal`.
- A device on the machine that runs the gateway container gets no handshake through the machine's own address (the
  WireGuard app sends over the physical interface). Set `Endpoint = 127.0.0.1:51821` in that device's config.

## Direct paths

Nodes of one org reach each other through the gateway (relay) or, when possible, directly over their own network.
Only the hub host needs inbound ports; a node listens on `wg-underlay` UDP 51822 for direct peers, and if that port is
unreachable the relay is used.

- Each node reports candidate addresses on port 51822: private LAN addresses first, then public addresses and the
  endpoint the gateway observed for it (with its observed port), then CGNAT/VPN-range addresses (`100.64.0.0/10`, other
  interfaces such as `tailscale0`), at most 8. Loopback, link-local, multicast, the platform pools from the address
  plan (including `10.198.0.0/16`, `192.168.64.0/24` and `10.64.0.0/10`), addresses with a zone ID and
  docker/OVS/bridge/`wg-underlay` interfaces are never candidates; the hub validates and drops them, and any reported
  candidate on another port. A physical LAN inside `10.64.0.0/10` therefore cannot be used for direct paths.
  Other VPNs' configuration is never touched, only their addresses are read.
- The hub pushes each node its peer set: the enrolled, non-quarantined nodes of its own org with an underlay address.
  Nodes of other orgs and quarantined nodes never appear in it, and a quarantined node drops out of every peer set on
  the next push.
- A node tries each candidate for 15 s. A path counts as direct only after a fresh WireGuard handshake (younger than
  3 min); until then traffic stays on the relay and the peer gets no allowed IPs. If every candidate fails the node
  keeps the relay and retries every 2 min. A direct path whose handshake is older than 3 min falls back to the relay.
  A peer whose candidates are not known yet (e.g. right after a hub restart) is not tried but configured with its key
  only, so the other node's try still succeeds; a direct path it already has stays until its handshake goes stale.
- On `wg-underlay` the node admits only Geneve (UDP 6081), ICMP echo and established/related traffic from peers, and
  everything from the gateway (`10.198.0.1`); see [nodes.md](nodes.md#underlay).

The node page lists every path with its mode, endpoint and path MTU.

### Hole punching

Two nodes behind NAT often cannot reach each other's candidates, because neither NAT lets the other's first packet in.
When a node reports its paths and both nodes of a same-org pair report each other on the relay, the hub sends each of
them a punch: the other node's endpoint as the gateway observed it and a common start time 2 s ahead. A punch is only
sent between nodes that are in each other's peer set, so never across orgs or to a quarantined node.

- At its first 5 s check after the start time each node points the peer at that endpoint and sends handshakes for 10 s
  (1 s keepalive). Afterwards the keepalive returns to 25 s.
- As for candidates, the path counts as direct only after a fresh handshake; during the punch traffic stays on the
  relay and the peer gets no allowed IPs. Without a handshake the node keeps the relay and the regular 2 min retry.
- The hub punches a pair at most once every 5 min. After 3 failed punches in a row it waits 1 h; after each such
  backoff the pair gets up to 3 more punches 5 min apart.
- A node ignores a punch for a node outside its peer set, for a peer already direct, with an invalid endpoint
  (validated like candidates), arriving while a punch for that peer is running, or whose start time is more than 30 s
  off its own clock. A start time already past but within the 10 s window starts the punch at once, for the rest of
  the window.

Known limits:

- A node whose underlay is down keeps its old peer set until it reconnects to the hub.
- Switching a path between direct and relay can drop traffic for up to one 5 s check.
- A node uses the first working candidate in rank order (LAN, public, VPN), not the one with the largest MTU.
- Hole punching cannot pass a symmetric NAT or CGNAT, which map each destination to a new port; such pairs stay on
  the relay and get up to 3 more punches after each 1 h backoff.

## Guest MTU

VM interfaces (DHCP) and device configs use the guest MTU, a platform setting (platform admin, Admin → Network): 1000
to 1362 (the nodes' `wg-underlay` MTU 1420 minus 58 bytes of Geneve), default 1300. It applies to VMs on their next
start or DHCP lease renewal and to newly downloaded device configs; changing it never restarts a VM and it is never
lowered automatically.

Geneve (58 bytes) and WireGuard (80 bytes) add 138 bytes of overhead. When the guest MTU plus 138 exceeds the MTU of
a direct path, packets on it are fragmented. The node page then shows a warning and the recommended guest MTU, the
path MTU minus 138 rounded down to a multiple of 10. The node probes the path MTU in the background and retries failed
probes after 10 min.

## Isolation

- A device reaches only the VMs its person may `vm:connect`: the owner of a user VM, the members of the owning team for
  a team VM. Org admins and owners do not get `vm:connect`. A user VM admits only its owner's devices whatever the
  rules say (connect check). Changes to rules, teams or devices take effect on the next gateway poll.
- VMs cannot reach each other on the management network, and cannot open connections to devices.
- Each org has its own switch (`ls-mgmt-<org>`) and port group; ACLs allow traffic to a VM only from its allowed
  devices and the gateway. Port security pins every port to its MAC and IP.
- A device reaches only its own org's router address (`<slice>.1`); ACLs on `ls-transit` drop the other orgs'.
- DNS picks the view by source address: a device or a VM management address sees only its own org's names
  (`<vm>.internal`, `<vm>.<network>.internal`); any other source gets REFUSED for `*.internal`. Other names are
  forwarded upstream, except for queries from the gateway's own addresses: when the hub host resolves through a
  device, Docker's DNS (the gateway's upstream) gets REFUSED and moves on to its next server instead of looping back.

## Networks

An org's members create networks (Networks page) with a name (`^[a-z][a-z0-9-]{0,30}$`) and an optional subnet;
without one the hub picks a `/24` from `10.210.0.0/16`. On a VM's page, attach or detach networks; a running VM
restarts for it. VMs on the same network talk freely (DHCP without a router, no ACLs). Networks are not routed and not
reachable from devices. A network can be deleted only when no VM is attached.

## Nodes and southbound trust

Nodes join over WireGuard (`wg-underlay`) to the gateway; see [nodes.md](nodes.md). Nodes are not trusted across orgs:

- The southbound DB is reachable only over SSL. Each node's ovn-controller uses a client cert (CN = node id) signed by
  the hub's OVN CA and connects with the `ovn-controller` RBAC role, so it can change only its own chassis rows.
- Every VM port carries `requested-chassis=<node id>`. The gateway clears any binding by a chassis whose name (its
  cert CN) is not that node id within one poll (2 s) and reports it; the hub logs `gateway cleared chassis bindings`.
  The chassis hostname, which a node sets itself, is never trusted.
- A node caught holding the same port on two consecutive polls, where NB explicitly requests another chassis (or the
  port is a gateway port), is quarantined. Ports missing from NB (VM just deleted or created, network detached) are only
  cleared. Quarantine means the gateway drops all its `wg-underlay` traffic (in, out and forwarded, including Geneve
  and the southbound session from the gateway) except TCP to `10.198.0.1:8091`, so the hub can still stop and delete its
  VMs. The hub logs `gateway quarantined nodes` and the node shows as quarantined. The quarantine is stored on the hub
  and lifted only when
  the node is re-enrolled (regenerate its credential, then re-install).
- The gateway deletes southbound chassis rows of unknown and quarantined nodes, so a re-enrolled node registers afresh.
- The gateway deletes `MAC_Binding` rows on `lr-gw` that do not match the address plan, within one poll.
- The gateway forwards underlay traffic only between nodes of the same org and between a node and the gateway. Direct
  paths are only configured between nodes of one org.

## Known limits

- Southbound contents (port and chassis names, MACs, IPs) are readable by every chassis. They contain no secrets.
- Networks bypass the connect check: any VM of the org may be attached to any network of the org, and VMs on a network
  reach each other regardless of who may connect to them.
- A node can hold a foreign port binding for up to two gateway polls (about 4 s) before it is quarantined; each poll
  also reverts the binding. Traffic sent to the port in that window may reach it.
- VMs never move between nodes. A future move must update the guard, or the old node, still holding the binding, gets
  quarantined.
- The southbound server cert is valid as long as the OVN CA (10 years) and is not rotated.
- Chassis certs are valid for one year; the hub re-signs a connected node's cert when less than 30 days are left.
- DNS over TCP shares the gateway's limit of 256 concurrent requests with UDP.
- A chassis cannot change its southbound hostname (not RBAC-updatable). A node whose OS hostname (or OVS
  `external_ids:hostname`) changes keeps failing southbound commits until it is re-enrolled.
- OVN's `ovn-controller` RBAC leaves some southbound tables writable by every chassis (e.g. `MAC_Binding`,
  `FDB`, `IGMP_Group`, `Controller_Event`). The gateway repairs only `MAC_Binding` on `lr-gw`; a malicious node can
  still disturb these tables, which at worst disrupts traffic but never grants access across orgs.

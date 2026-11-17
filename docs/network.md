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
| TCP 6642 | southbound DB, SSL only, bound to `10.198.0.1` (inside the underlay) |
| UDP/TCP 53 | DNS on `10.192.0.1`, for devices and VM management NICs |

The northbound DB listens on a unix socket only. On `wg-underlay` the gateway accepts only Geneve (UDP 6081), the
southbound DB (TCP 6642) and rate-limited ICMP echo to `10.198.0.1`, so nodes can ping it for diagnostics. ICMP
between nodes is not forwarded.

### Running it

Hub env (`deploy/dev/hub.env`):

- `OBSERVATORY_GATEWAY_TOKEN`: the gateway's credential, at least 32 characters. `make gateway-token` adds a random one
  if it is missing.
- `OBSERVATORY_GATEWAY_HOST`: the address nodes and devices use to reach the gateway. It defaults to the host of
  `OBSERVATORY_PUBLIC_URL`, so that URL must not be `localhost` once real nodes or devices connect.
- `OBSERVATORY_SECRETS_KEY` is required: the gateway's WireGuard keys and the OVN CA are generated on first start and
  sealed with it.

```sh
make gateway-up      # builds the image and starts it with the dev compose (profile "gateway")
make gateway-down
```

The container reads `OBSERVATORY_GATEWAY_TOKEN` and `HUB_URL` (default `http://host.docker.internal:8090`). For a
hub on another port, set `HUB_URL` in the shell or in `hub.env`. The gateway needs the `openvswitch` and `wireguard`
kernel modules on the Docker host; the compose `modules` service loads them.

Do not run the gateway on a host that is also a node: both would share the kernel's OVS datapath.

## Devices

A device belongs to one person in one org. Devices → Add device returns the WireGuard config once (download or QR code).
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

Import it with `wg-quick up <file>` or the WireGuard app, then `ssh ubuntu@<vm>.internal`. Owners see and delete
their own devices; org admins see and delete every device of the org but never get a config. Removing a member from
the org deletes their devices in it; deleting a device cuts its traffic within one gateway poll.

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
  forwarded upstream.

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
- A node caught holding a port bound to another node is quarantined: the gateway drops all its `wg-underlay` traffic
  (in, out and forwarded, including Geneve and the southbound session from the gateway), the hub logs
  `gateway quarantined nodes` and the node shows as quarantined. The quarantine is stored on the hub and lifted only when
  the node is re-enrolled (regenerate its credential, then re-install).
- The gateway deletes southbound chassis rows of unknown and quarantined nodes, so a re-enrolled node registers afresh.
- The gateway deletes `MAC_Binding` rows on `lr-gw` that do not match the address plan, within one poll.
- The gateway forwards underlay traffic only between nodes of the same org and between a node and the gateway.

## Known limits

- Southbound contents (port and chassis names, MACs, IPs) are readable by every chassis. They contain no secrets.
- Networks bypass the connect check: any VM of the org may be attached to any network of the org, and VMs on a network
  reach each other regardless of who may connect to them.
- A node can hold a foreign port binding for up to one gateway poll (2 s) before it is reverted and the node is
  quarantined. Traffic sent to the port in that window may reach it.
- VMs never move between nodes. A future move must update the guard, or the old node, still holding the binding, gets
  quarantined.
- The southbound server cert is valid as long as the OVN CA (10 years) and is not rotated.
- Chassis certs are valid for one year; the hub re-signs a connected node's cert when less than 30 days are left.
- DNS over TCP shares the gateway's limit of 256 concurrent requests with UDP.
- OVN's `ovn-controller` RBAC leaves some southbound tables writable by every chassis (e.g. `MAC_Binding`,
  `FDB`, `IGMP_Group`, `Controller_Event`). The gateway repairs only `MAC_Binding` on `lr-gw`; a malicious node can
  still disturb these tables, which at worst disrupts traffic but never grants access across orgs.

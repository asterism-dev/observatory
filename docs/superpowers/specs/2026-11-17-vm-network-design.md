# VM network (OVN and WireGuard)

## Intent

Project 2 of the VM port (roadmap in `2026-11-17-vms-nodes-design.md`). After project 1 a VM is reachable only from its node. This project gives people access to their VMs from their own machines, the way the PoC does it, but for tenants: OVN for the virtual networks, a WireGuard gateway for devices, `*.internal` DNS, and the connect check that keeps private secrets out of reach of other users.

Success: a member adds a device in the UI, imports the WireGuard config, and runs `ssh ubuntu@dev.internal`; a colleague's device cannot reach that VM; a member of another org cannot even resolve its name; VMs of a team talk to each other over a network the team attached them to.

## Decided with the user

- **Address plan:** one hub-wide address space; each org gets its own slice and its own logical switch. One router, one gateway, one WireGuard interface for all devices; isolation by OVN ACLs.
- **Who may connect:** owner of a user VM; members of the owning team for a team VM. Org admins and owners do **not** get `vm:connect` (they may not look inside, IAM spec). Granting `vm:connect` through custom roles or shares comes later; in this project it is only generated (nobody holds it on `org/vms/*`, so the escalation rule blocks custom grants anyway).
- **Device keys:** the hub generates the key pair and shows the config once for download; it stores only the public key. A lost config means a new device.
- **Networks** (extra VM-to-VM networks) are part of this project, owned by an org. Any VM of the org may be attached to any network of the org; the connect check does **not** apply to network traffic (documented limitation).
- **Nodes are not trusted** across orgs: the shared OVN is hardened so a node can only bind its own VMs' ports and exchange underlay traffic only with nodes of its own org and the gateway (see "Node trust").
- One OVN, one gateway, per-node credential (roadmap).

## Goals

- **Per-org management network.** Every VM gets a management NIC on its org's switch (in addition to the NAT NIC from project 1). VMs cannot reach each other on it (as in the PoC); the management network is for device → VM access.
- **Devices.** A person creates a device in an org, downloads its WireGuard config once, and reaches exactly the VMs they may `vm:connect` in that org (ICMP, TCP, UDP to the management IP). Devices of different orgs and of different people are isolated from each other.
- **Connect check.** A user VM always receives its owner's private scope, so a user VM admits only devices of its owner, whatever the rules say (rule from the secrets spec). Team VMs never receive private variables. *(Our call: judged by VM kind, not by the current variable list, because a deleted variable stays in a running guest until its next boot.)* Later, host-bound secrets may relax it (project 4).
- **DNS.** `<vm>.internal` resolves for devices and VMs of the same org only (per-org view selected by source address). `<vm>.<network>.internal` for network NICs.
- **Networks.** An org's members create networks (name, optional subnet); VMs are attached/detached (restart required, as in the PoC); VMs on the same network reach each other freely; networks are not routed and not reachable from devices.
- **Gateway** as in the PoC (ovn-northd, NB/SB, WireGuard for node underlay and for devices, DNS), run in the dev compose; it pulls desired state from the hub with its own credential.
- **Node underlay** re-ported from the PoC (chassis key in `register`, `underlay` message, WireGuard + ovn-controller config on the node). This brings back the chassis code removed at the end of project 1.
- **Permissions, audit, UI** for devices and networks; `vm:connect` in the catalog.

## Non-goals

- SSH certificates / hub as SSH CA, `observatory ssh`.
- Shares of a VM with individuals (only via custom roles for now).
- Routing networks to devices or to each other; egress control (project 4).
- Multiple gateways / HA; macOS nodes (vz netagent).
- Disks (project 3).

## Anti-goals

- A device reaching a VM its person may not `vm:connect`, or any VM of another org.
- A user VM reachable over the management network by anyone but its owner. (Networks are excluded by decision, see above.)
- The hub storing device private keys.
- Address or name collisions between orgs leaking traffic or DNS answers across orgs.

## Approaches

- **A) Unique slices of one address space, one router, ACLs (picked by the user).** Simple gateway (one `wg-clients`, one DNS server with per-org views), isolation in OVN ACLs computed by the hub. Limit: number of orgs × VMs per org bounded by the pool.
- B) Overlapping 10.200/16 per org with a router and WireGuard interface per org. Full separation, but per-org interfaces, ports and DNS on the gateway.

## Address plan *(our call)*

| Range | Use |
|---|---|
| `10.64.0.0/10` | management; each org gets a `/20` (4 093 VMs) on first VM, 1 024 orgs |
| `10.192.0.0/16` | devices (`/32` each); gateway `10.192.0.1` (DNS) |
| `10.198.0.0/16` | node underlay; gateway `10.198.0.1` |
| `10.199.0.0/30` | transit gateway ↔ router (as PoC) |
| `10.210.0.0/16` | auto subnets for networks (`/24` each), unique hub-wide |
| `192.168.64.0/24` | NAT NIC on each node (project 1), never routed |

- The org slice's first address is the router (`.1`); VMs from `.10`. Slices are recorded in Postgres and never reused while the org exists.
- Custom network subnets must be private, `/16`–`/28`, and not overlap any range above or another network of any org (networks are not routed, but a VM can be on several, so overlap breaks it).

## Isolation

- **Management switches:** one `ls-mgmt-<org>` per org, all attached to router `lr-gw`. Port security (MAC+IP) on every port, as in the PoC.
- **ACLs** (port group per org, computed by the hub, applied by the gateway):
  - VM ports: allow from the router/DHCP/DNS (`10.192.0.1`) and from the device addresses allowed for that VM; drop the rest. VM → VM on management stays dropped.
  - Allowed devices for a VM = devices of persons who hold `vm:connect` on it, minus everyone but the owner if the connect check applies.
- **Gateway firewall** (as PoC, adapted): from `wg-clients` only DNS to the gateway and forwarding into the transit; VMs cannot open connections to devices.
- Changes to rules, team membership, devices or variables re-compute the allow lists; the gateway applies the new state on its next poll (PoC: 2 s).

## Node trust

Nodes belong to orgs and their admins control the host, so a node must not be able to touch another org's traffic:

- The southbound DB is reachable only over SSL; each chassis gets a certificate signed by a hub-held OVN CA at enrollment (sent in `underlay`), and ovn-controller connects with the `ovn-controller` RBAC role, so it can only change its own chassis rows.
- Every logical switch port carries `requested-chassis=<node id>`, so a node can bind only ports of VMs scheduled on it.
- The gateway forwards underlay (Geneve) traffic only between nodes of the same org and between a node and the gateway; `AllowedIPs` on each node's peer is its own address only.
- Known limit: southbound contents (port and chassis names, MACs, IPs) remain readable by every chassis; no secrets are in them. Documented.

## Devices

- A device belongs to one person **in one org** (a person with two orgs creates two devices). Name, public key, address, created/last-handshake time.
- Create returns the config once (`[Interface] PrivateKey, Address, DNS = 10.192.0.1`; `[Peer] gateway key, endpoint, AllowedIPs = <org slice>, 10.192.0.1/32`). The hub keeps only the public key.
- The owner lists and deletes their devices; org admins list and delete every device of the org (revocation), never download configs.
- Removing a member from the org deletes their devices in that org.

## Networks

- Belong to an org; name unique per org (`^[a-z][a-z0-9-]{0,30}$`), optional subnet, else a `/24` from `10.210.0.0/16`.
- Attach/detach: full network list per VM; a running VM restarts and needs `restartAllowed` (as PoC). Delete only without members.
- DHCP without router option; no ACLs inside a network (VMs on it talk freely), not reachable from devices.

## Gateway and nodes

- Gateway as in the PoC (`cmd/gateway`, `deploy/gateway-start.sh`, Dockerfile) in `deploy/dev/compose.yaml`, ports UDP 51820 (underlay) and 51821 (devices). It authenticates to the hub with a credential from `OBSERVATORY_GATEWAY_TOKEN` (shared between hub env and gateway env; constant-time compare). *(Our call; a hub-managed gateway like PoC `-stack` comes later.)*
- `GET /api/gateway/state` returns keys, chassis, device peers, per-org switches, ports, ACL allow lists, networks and DNS records per org; `POST /api/gateway/status` reports handshakes and chassis (shown on devices and nodes).
- Gateway WireGuard private keys are generated by the hub on first start and sealed with the master key (`OBSERVATORY_SECRETS_KEY`).
- Nodes: `register` carries the chassis public key again; the hub enrolls it (underlay address from `10.198.0.0/16`) and sends `underlay`; the node configures WireGuard and ovn-controller as in the PoC. VM taps for management and network NICs go on `br-int` with `iface-id` = port name. The guest config gets the management and network NICs back (PoC `CloudInit`), DNS `10.192.0.1` for `~internal`.

## Permissions

| Action | Object |
|---|---|
| `vm:connect` | `org/vms/*` |
| `vm:network` | `org/vms/*` (change a VM's networks) |
| `device:create` | `org/user-devices/<user>` |
| `device:list`, `device:delete` | `org/user-devices/*` |
| `network:read`, `network:create` | `org/networks` |
| `network:use`, `network:delete` | `org/networks/*` |

- Built-in and generated `vm:*` rules from project 1 become explicit action lists (`vm:read`, `vm:power`, `vm:delete`, `vm:network`), so new VM actions are never granted by a wildcard. Generated: user VM owner `vm:connect`; team subject `vm:connect` on team VMs.
- Defaults: members create/list/delete their own devices (generated rule on `org/user-devices/<self>`); admins `device:list`/`device:delete` on `org/user-devices/*`. Members `network:read`, `network:create`, `network:use`; admins also `network:delete`. `vm:network` for the VM owner, team admins and org admins.
- Audit: `device:create`, `device:delete`, `network:create`, `network:delete`, `vm:network`.

## UI

- **Devices** in the main navigation: list (name, address, last handshake), add dialog → config download once (and QR code, *our call*), delete. Admins see all devices of the org with the owner.
- **Networks** page: list, create, delete; VM detail gets a networks section (attach/detach with restart confirm) and shows the management address and DNS name.

## Left to the builder

- Exact REST paths and payloads; gateway state format (start from the PoC's `GatewayState`).
- How much of the PoC gateway/nb/dns code is copied unchanged.
- How DNS selects the per-org view (source address → org slice / device address → org).
- Existing nodes/VMs from project 1 are dev-only: no migration; nodes are re-installed, VMs get their management NIC on next start. *(Our call.)*
- Test strategy; must include: device of person A cannot reach person B's VM (ACL computed), device of org A cannot resolve or reach org B, a user VM admits only its owner's devices; a node cannot bind another node's port and cannot reach another org's node over the underlay, wildcard rules no longer grant `vm:connect` to admins, network attach/detach restart semantics, address slice allocation never overlaps; one real-hardware run (test VM 108 as node, gateway in Docker on the Mac or on 108).

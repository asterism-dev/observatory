# Mesh underlay

## Intent

Project 1 of the 1.0 rework. Project 2 splits the hub into services (IAM, compute, storage, registry, logs) with one
central IAM and one shared GUI; it needs a network in which hosts reach each other. This project builds that network
and moves the node control channel onto it.

The rule from the start stays: **the hub host is the only thing every participant must be able to reach.** Nodes,
later service hosts, and devices may sit behind NAT or firewalls without open ports. New: when two hosts can reach each
other directly they talk directly; otherwise the hub host relays. No one maintains routes.

Target audience: homelabs (lightweight, few machines, often already running Tailscale) up to enterprise (nodes in one
data centre). Where nodes sit is unknown, so NAT on both sides must work.

Success:

- Two nodes of an org in the same LAN exchange VM traffic (Geneve) directly, without the gateway in the path; two nodes
  behind different NATs reach each other directly when hole punching succeeds, through the gateway otherwise.
- The hub controls nodes without the WebSocket+yamux channel.
- When the gateway is down, the hub can still update and re-enroll nodes.
- The hub and a node run on the same machine.

## Today

- Nodes keep one WebSocket to the hub; yamux carries exactly one stream, the control stream (`internal/fleet/conn.go`,
  `internal/node/agent.go`). The `underlay` message on it gives the node its WireGuard config.
- The underlay (`10.198.0.0/16`, `wg-underlay`) is a star: every node peers only with the gateway (`10.198.0.1`), and the
  gateway forwards underlay traffic between nodes of the same org (`docs/network.md`, Node trust). It carries Geneve,
  the southbound DB session and the storage NBD exports.
- OVN encapsulates Geneve to each chassis' underlay address; OVN itself transports nothing.
- The gateway container runs its own `ovsdb-server`, `ovs-vswitchd` and `ovn-controller` (chassis `hub-gw`,
  `deploy/gateway-start.sh`), so it cannot share a kernel with a node.

## Decided with the user

- **Approach A** (below): kernel WireGuard, built by us, direct where reachable, relay through the gateway otherwise,
  hole punching as a later step within this project.
- **Node control channel moves onto the underlay**; WebSocket+yamux goes away. A small HTTPS enrollment step stays,
  because a node needs its WireGuard config before it can use the underlay.
- **Health check and emergency channel:** every node sends a health check to the hub over the underlay every minute.
  When it fails, the node falls back to HTTPS straight to the hub. The fallback carries **only emergency operations**:
  node update, re-enroll, new underlay config, status. Creating, starting or stopping VMs waits for the underlay.
- **Hosts with roles:** an underlay member is a host with a role (hub host, node, later service host). Enrollment, key,
  underlay address, direct paths, health check and fallback are shared. Only nodes are rolled out in this project.
- **Hub and node on one machine** is part of this project, via one shared OVS per host (way 1 below), preceded by a
  spike.
- **Hard cut on a 1.0 branch**, no compatibility code with 0.x. Running 0.x hubs stay on their release. Branching and
  what happens on main meanwhile is the user's business.

## Goals

1. **Underlay hosts with roles and the control channel.** Generic host enrollment over HTTPS (install token + public
   keys → underlay address, WireGuard config, chassis cert). The node's control traffic runs over the underlay; its
   WireGuard key is its network identity. Health check every minute, fallback to the HTTPS emergency channel, back to
   the underlay as soon as a check passes. The GUI shows a new node state "underlay down" (only HTTPS arrives),
   distinct from "offline" (nothing arrives).
2. **Direct paths.** Nodes learn candidate endpoints for their peers and peer directly; without a working direct path
   traffic goes through the gateway as today. OVN keeps sending Geneve to the same underlay addresses and does not
   notice which path is used.
3. **Hole punching** for peers that are both behind NAT.
4. **Spike, then hub and node on one machine.**

## Non-goals

- Devices peering directly with nodes; they stay in the star through the gateway (`wg-clients`) and keep the plain
  WireGuard app config.
- A redundant gateway or additional relays.
- Rolling out service hosts (project 2), S3.
- Optimising for "best" path beyond "direct if it works, else relay".
- Embedding a third-party mesh (Tailscale components, Nebula).

## Anti-goals

- Any participant needing an open inbound port other than on the hub host.
- Underlay traffic between nodes of different orgs, directly or relayed.
- A quarantined node keeping a direct path to any peer.
- Losing the ability to update or re-enroll a node because the gateway is down.
- Breaking a node that also runs Tailscale or another VPN (`100.64.0.0/10`, routing table 52, its DNS); we use its addresses as candidates but never touch its config.

## Approaches

- **A) Own mesh on kernel WireGuard (picked).** The hub already distributes keys; add endpoint discovery, direct peer
  entries and relay fallback through the existing star. Kernel performance for Geneve and NBD, no new dependency,
  devices unchanged, coexists with Tailscale (disjoint ranges, no shared routing tables). Cost: NAT corner cases are
  ours; symmetric NAT / CGNAT stays on the relay.
- B) Embed Tailscale components (headscale-style coordination, DERP relay, client library). Best NAT traversal, but
  userspace WireGuard, large dependency with unstable internals, and real conflict risk on hosts that already run
  Tailscale (same default range, routing table, DNS); userspace mode (tsnet) would avoid that but cannot carry kernel
  Geneve.
- C) Nebula (lighthouse + relay on the hub, hub as CA, group firewall). Built for this, but not WireGuard, a second
  network stack next to the devices' WireGuard, smaller ecosystem.

For hub and node on one machine:

- **1) One shared OVS per host (picked).** A host with both roles runs one `ovs-vswitchd`/`ovn-controller`; the gateway
  uses it instead of its own, and the one chassis is both gateway chassis and VM host (`enable-chassis-as-gw`).
- 2) Gateway on the OVS userspace datapath. Tiny change, but all gateway traffic (devices, relay, NBD) gets slower.
- 3) Node as a VM on the hub host. Nested virtualisation; a workaround.

## Design

### Hosts and addresses

- A host has an id, a role set and an underlay address in `10.198.0.0/16` (unchanged plan). Nodes keep their existing
  ids and org binding; the hub host is the gateway at `10.198.0.1`.
- One underlay interface per host. Services on a host sit behind it and reach each other locally; only traffic to
  another host goes through WireGuard. (Relevant from project 2 on; services address each other by name.)
- The hub must be reachable at an underlay address so nodes can reach it without the public URL (today it only sits
  behind HTTP on the docker network).

### Peers and isolation

- A node's peer set: the gateway, the nodes of its own org, later the service hosts. Peers outside that set are never
  configured, so WireGuard's key-to-address binding drops their traffic; this replaces the gateway's central
  "same org only" forwarding filter for direct paths. The gateway keeps the filter for relayed traffic.
- Quarantine (`docs/network.md`, Node trust) must also remove the node from every other node's peer set, and the
  gateway keeps dropping its traffic as today, except the node's control traffic to the hub, so the hub can still stop
  and delete VMs on it as it can today over the WebSocket *(our call)*. The node shows as quarantined, not as
  "underlay down".
- Kernel WireGuard routes each address to exactly one peer, so for every peer node the address is either on that peer's
  entry (direct) or covered by the gateway entry (relay). Switching path means moving the peer's `/32`.

### Endpoints, direct paths, relay

- Candidate endpoints per node: its LAN addresses, its public `ip:port` as the gateway sees its WireGuard socket (the
  gateway already reads peers from `wg`; it reports endpoints alongside handshakes), and addresses on other interfaces
  such as a Tailscale or VPN IP. Candidates go to the hub, the hub hands each node the candidates of its peers.
- A node tries direct, judges by handshake freshness, and falls back to the relay when no handshake arrives within a
  short timeout; it retries direct periodically.

### Any network, MTU

Every network must work, including paths through other VPNs (corporate IPsec, Tailscale, PPPoE), which are normal in
enterprise settings (user decision).

- Every candidate is usable, whatever interface it is on. Among working paths the one with the largest MTU wins.
- The node measures the path MTU per peer path; the GUI shows it on the node page.
- A packet too big for a path is fragmented outside WireGuard and still arrives, slower (the builder verifies that
  kernel WireGuard leaves DF unset on its outer packets; some firewalls drop fragments). When the guest MTU does not fit
  a path unfragmented, the GUI warns with the recommended guest MTU.
- The guest MTU becomes a hub setting (default 1300 as today, `netaddr.GuestMTU`), used for VMs (DHCP) and device
  configs. It is never lowered automatically: VMs take it on their next start, so an admin changes it deliberately.

### Hole punching

Both sides send to each other's public candidate at the same time (the hub coordinates), with keepalive to hold the
NAT mapping. Where it fails (symmetric NAT, CGNAT), the relay stays.

### Control channel

- Node ↔ hub control traffic runs over the underlay. The direction of connection setup and the wire format are the
  builder's call, as long as no inbound port is needed except on the hub host and the WireGuard key serves as the
  node's identity.
- Enrollment stays HTTPS with the single-use install token (`install.sh`).
- Health check every 60 s over the underlay; after three consecutive failures (our call) the node switches to the HTTPS
  emergency channel and keeps probing the underlay every minute; one passing check switches back. VMs keep running in
  both modes.
- Emergency channel operations only: node update, new underlay config, status report. Everything else is refused
  there. Re-enroll stays as today (regenerate credential, run `install.sh` with a fresh token); it only has to work
  without the underlay, since enrollment is HTTPS *(our call)*.
- The node keeps its bearer credential (`node.json`) for everything on HTTPS: the emergency channel, OTLP telemetry,
  image pulls through the registry proxy, binary and installer downloads. Moving that traffic onto the underlay is left
  to project 2 *(our call)*.

### Hub and node on one machine

The spike answers: can the gateway run against the host's OVS instead of its own; what in the hub and gateway assumes a
dedicated chassis `hub-gw` (gateway router binding, port guard, quarantine, the `hub-gw` system id); how the local
node's underlay address and the gateway's `10.198.0.1` live on one host. The spike result comes back to the user before
this part is planned.

### Upgrade from 0.x

Fresh install (user decision): 1.0 starts empty, nodes are enrolled anew with a token, VMs are recreated, disks are
saved beforehand if needed. No migration of 0.x data and no acceptance of 0.x node credentials. The upgrade
documentation says so.

## Left to the builder

- Wire format and connection direction of the control channel; how the hub joins the underlay.
- How candidates and peer lists are delivered (control channel, extending today's `underlay` message, etc.).
- Timeouts and retry intervals for direct paths and hole punching.
- Order of the endpoint-discovery and relay-fallback internals, as long as goals 1–3 land in that order.
- Tests: two nodes on one LAN directly, two nodes behind NAT relayed, quarantine cutting direct paths, gateway down →
  emergency channel, Tailscale on a node untouched.

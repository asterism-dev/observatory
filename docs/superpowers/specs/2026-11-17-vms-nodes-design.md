# Nodes and VMs (Firecracker)

## Intent

observatory gets its first resource: VMs that boot on nodes. This is project 1 of the VM port from the PoC (vm-manager). It brings over nodes, the node agent, the Firecracker provider and VMs, and plugs them into what observatory already has: orgs, teams, Casbin permissions, audit, limits and the variable resolver. The PoC's node side is proven on real hardware; the hub side is rebuilt for tenants.

Success: an org admin adds a Linux node with a one-line install command, the node shows up online in that org, a member creates a VM on it with their SSH keys and the org's variables, starts, stops and deletes it, and every step is permission-checked and audited.

## Roadmap (decided with the user)

The port is five projects, each with its own spec and plan, built in this order:

1. **Nodes and VMs (this spec).** Firecracker only; vz (Apple) and cloud-hypervisor are not ported.
2. **Network:** OVN and the WireGuard gateway as in the PoC: networks, devices (WireGuard configs), `*.internal` DNS, node underlay. Decided: one OVN for the hub with one management network per org; an org's networks never reach another org's. Devices belong to a person; a device reaches only VMs the person may `vm:connect`. The connect check from the secrets spec is enforced here: a VM with private variables or private integrations admits only its owner.
3. **Disks:** storage server, NBD attachment via `qemu-nbd`, create/copy/grow/release, as in the PoC. One storage server per hub.
4. **Egress proxy:** per node, transparent redirect (nftables) from the VM taps; forwarding by the name from SNI / `Host`, never by destination IP; an own CA in the guest with name constraints on the bound hosts and a short lifetime; TLS is terminated only for bound hosts, where placeholders are replaced by real values (headers, URL, body) and real values scrubbed from responses. Host-bound variables (`hosts` on a secret) reach VMs only as placeholders. CA trust variables (`NODE_EXTRA_CA_CERTS`, `SSL_CERT_FILE`, `REQUESTS_CA_BUNDLE`) are set system-wide (`/etc/environment`, systemd), not only in login shells. No egress allowlist (decided).
5. **Integrations:** their own resource type (not a variant of variables), on the scopes org, team and private, with their own `integration:*` permissions. GitHub (PAT, verified against the GitHub API), Claude (`claude setup-token`), Linear (API key). Delivered through the egress proxy: the VM holds placeholders only (git credential helper, `gh` wrapper, Claude credentials / `CLAUDE_CODE_OAUTH_TOKEN`, Linear MCP); real values are substituted for the provider's hosts (Claude: `api.anthropic.com`). A token service, where still needed, identifies the VM by its management IP (OVN port security binds IP and MAC) and is reachable only from VMs.

Also decided for the whole port: one credential per node instead of a shared token; one gateway and one storage server per hub, run by the dev compose; node installer yes, hub/node self-update and the hub-managed container stack later.

## Goals (this project)

- **Nodes belong to an org.** An org admin adds a node, gets a one-line install command, the node enrolls and connects. A node serves only its org's VMs.
- **Per-node credential.** The install command carries a single-use enrollment token; the node exchanges it for its own long-lived credential. The hub can revoke a node.
- **Node agent and Firecracker provider** ported from the PoC: WebSocket + yamux transport, desired-state push, reconcile with restart backoff, jailer, NAT bridge `vmm0`, cloud-init seed, Ubuntu cloud image, kernel extracted from the guest.
- **VMs owned by a user or a team** (as decided in the secrets spec), created on a node of the org, with CPU, memory, root disk size and SSH keys. Power: start, shut down (graceful, hard after 10 s), stop (hard). Delete.
- **SSH keys per person** in the profile; at create, the keys of the owner (user VM) or of the team's members (team VM) are put into the VM.
- **Variables reach the VM:** at every boot the node receives `ResolveEnv(owner)` and writes it into the guest (`/etc/environment` and a login-shell profile).
- **Permissions, audit, limits:** new catalog entries for nodes and VMs; every change audited; org/team/user limits (vCPU, RAM, disk GB, VM count) **enforced** at create.
- **UI:** Nodes page (list, add with install command, detail, delete), VMs page (list, create dialog, detail with power actions and delete), SSH keys in the profile. The PoC UI is the reference for content, built with lumen like the rest of observatory.

## Non-goals (this project)

- Everything in projects 2 to 5: OVN, WireGuard, devices, networks, disks, storage, egress proxy, placeholders, integrations, token service.
- Any way for a user to reach a VM other than from the node host. A VM has internet through `vmm0` NAT; that is all until project 2.
- vz and cloud-hypervisor providers.
- Hub and node self-update, the hub-managed container stack.
- Shares of a VM with other users or teams; VM accounts (`vm:<id>` principals); org guardrails such as expiry.
- Booting from pre-booted memory snapshots.
- Scheduling: the creator picks the node.

## Anti-goals

- A node that can see, start or receive data of another org's VMs, or act for another node.
- A node credential that, once leaked, cannot be revoked without touching the other nodes.
- A VM state change (create, power, delete, owner change) without an audit row.
- Private variables of one user ending up in a VM owned by someone else or by a team.
- A rewrite of the proven node-side behaviour (reconcile, jailer, seed, boot) without a reason.

## Approaches

- **A) Port the node side nearly unchanged, rebuild the hub side (picked).** `internal/proto`, `internal/node`, `internal/provider/firecracker`, `internal/provider/linuxvm` and `cmd/node` come over with minimal changes (drop vz/cloud-hypervisor, swap the shared token for the node credential, add variables to the spec). Registry, VM store and API are written new on Postgres, Casbin and audit. Keeps what was learnt on real hardware; new code only where tenants need it.
- B) Rewrite everything. Clean, but high risk exactly where the PoC paid for its lessons (Firecracker quirks, jailer, reconcile).
- C) Keep the PoC hub as an internal service behind observatory. Fastest, but two data stores and two permission models.

## Nodes

- **Add:** an org admin (`node:create`) creates a node with a name. The hub stores it as `pending` and returns an install command with a single-use enrollment token (expires after 24 h; can be regenerated while the node is pending).
- **Enroll:** the installer (ported from the PoC: packages, Firecracker + jailer, `nbd` module, `vmm0` bridge, nftables masquerade, systemd unit) calls the hub with the enrollment token and receives the node's id and credential (random secret, stored hashed on the hub). The token is burnt.
- **Distribution:** the hub serves the installer and the node binary itself (the linux/amd64 node binary is built with the hub and embedded or shipped next to it). No release pipeline in this project. *(Decided.)*
- **TLS:** terminated by a reverse proxy in front of the hub; the hub stays plain HTTP. The node accepts `ws://` (dev) and `wss://`. *(Decided.)*
- **Connect:** the node dials `<hub>/api/nodes/connect` with its credential. Same message set as the PoC (`register`, `status`, `desired`); `underlay`, `disks`, `update`, `updateFailed` stay out until their projects.
- **State:** `pending` (not enrolled), `online`, `offline`. Capacity and machine info come from `register`.
- **Delete:** only without VMs. Revokes the credential and drops the connection.
- **Revoke / re-enroll:** an admin can regenerate the credential, which disconnects the node until it is re-installed with a new enrollment token. Re-enrolling binds to the same node row: same id, its VMs are kept and reconciled, the installer keeps `paths.vms`. The node shows `offline` meanwhile, not `pending`. *(Our call.)*
- **Capacity** is shown, not enforced; overcommit is allowed as in the PoC. *(Our call; org/team/user limits are the enforced bound.)*
- The PoC requires Open vSwitch on the node and always creates a management NIC. In this project the node creates no OVS ports; the installer still installs OVS/OVN so project 2 needs no re-install. *(Builder: decide whether to keep the `ovs-vsctl` startup check now or add it in project 2.)*

## VMs

- **Create:** name, owner (me, or one of my teams where I may `vm:create` for the team), node (one of the org's online or offline nodes I may `node:use`), vCPU, memory, root disk GB. Created on an offline node it shows `pending` until the node connects (as in the PoC).
- **Owner and cost center:** the owner is also the cost center. A user VM counts against the user's membership limit, a team VM against the team's limit; both also against the org limit. Creation is refused when any is exhausted. *(Our call: the IAM spec allowed booking a user's VM onto a team; with owner = user or team that collapses into the owner.)*
- **Limits counted:** VM count, vCPU, memory, root disk GB of all VMs the owner has, running or not.
- **SSH keys:** user VM → the owner's keys; team VM → the keys of all current team members.
- **Replace at every boot (decided):** hub-managed keys and variables are rewritten wholesale on every boot, so anything removed on the hub (a key, a variable, a member leaving the team) is gone after the next boot. Hub-managed keys live in their own file (e.g. via `AuthorizedKeysFile`), so keys a user added inside the VM stay. The node pushes keys and variables into the guest on every boot, not only on cloud-init's first boot (new mechanism; the PoC wrote them once).
- **Restart without hub (decided):** a node that restarts a VM while the hub is unreachable (crash backoff, host reboot) uses the last state the hub pushed. The hub pushes the current keys and variables on connect and on every change to them.
- **Variables:** resolved with `ResolveEnv(owner)` and delivered in the desired VM spec; the node writes them into the guest at boot. They never appear in the node's logs, its `vm.json`, the `register`/`status` messages or the hub's API responses. The node keeps them only in memory and in the seed image inside the VM directory (mode 0700).
- **Power and delete** as in the PoC (`running`/`stopped`, graceful vs. hard stop, desired state survives node restarts, `error` after repeated quick failures). Delete removes the VM and its root disk. As in the PoC, a VM on an offline node is removed from the hub at once (row gone, limits freed, `vm.delete` audited) and the node deletes it when it reconnects, since the hub pushes the full desired list. *(Our call.)*
- **States:** `pending`, `starting`, `running`, `stopped`, `error`, `unknown`, as in the PoC.
- **Leaving / removal:** a member who still owns VMs cannot be removed from the org (rule from the IAM spec); deleting a team that owns VMs is refused. *(Our call for teams.)*
- **Image:** Ubuntu 24.04 cloud image as in the PoC, user `ubuntu`, configurable per node (`imageURL`).

## Data model (outline)

- `nodes`: id, org_id, name, state, enrollment token hash + expiry, credential hash, capacity, machine info, last seen, created_by, created_at.
- `vms`: id, org_id, node_id, owner (`user_id` or `team_id`, exactly one), name (unique per org), cpus, memory_mb, disk_gb, desired power, stop mode, generation, provisioned, last reported state / error / IP, created_by, created_at.
- `ssh_keys`: id, user_id, name, public key, fingerprint, created_at. Keys belong to the person, not to an org.
- Connection state (which node is connected, last status) is in memory in the hub, like the PoC registry; the desired state is always built from Postgres.

## Permissions

New catalog entries:

| Action | Object |
|---|---|
| `node:read`, `node:create` | `org/nodes` |
| `node:read`, `node:update`, `node:delete`, `node:use` | `org/nodes/<id>` |
| `vm:create` | `org/vms` (user VMs), `org/team-vms/<team-id>` (team VMs) |
| `vm:read`, `vm:power`, `vm:delete` | `org/vms/<id>` |

- VM objects are org-scoped (`org/vms/<id>`), like private variables, so org-wide rules (`deny vm:delete@org/vms/*`) also cover user-owned VMs.
- The owner's rights on a VM come from a generated rule: user VM → the user gets `vm:*` on that VM; team VM → the team subject gets `vm:read` and `vm:power`, the team's admins `vm:*`. Like the existing team-admin and private-variable rules.
- `vm:connect`, `vm:share` and `vm:ssh` arrive with project 2 and shares.
- All new actions go through the escalation rule.

Defaults:

| | member | team admin (of that team) | admin | owner |
|---|---|---|---|---|
| see nodes, place VMs on them (`node:read`, `node:use`) | yes | yes | yes | yes |
| add, rename, delete nodes | | | yes | yes |
| create own VMs | yes | yes | yes | yes |
| create team VMs | | yes | yes (all teams) | yes (all teams) |
| own VMs: all actions | yes | yes | yes | yes |
| team VMs: read, power | team member | yes | yes | yes |
| team VMs: delete | | yes | yes | yes |
| see, power and delete every VM in the org | | | yes | yes |

- Org admins can delete and power other people's VMs but, per the IAM spec, not look inside (no console exists yet anyway).
- Audit: `node.create`, `node.enroll`, `node.delete`, `node.revoke`, `vm.create`, `vm.power`, `vm.delete`; node connect/disconnect is logged, not audited. *(Our call.)*

## UI

- **Nodes** (`/orgs/:orgId/nodes`, already in the sidebar): table with name, state, version, vCPU/RAM used of capacity, VM count. Add dialog shows the install command with a copy button. Detail: machine info, VMs on the node, regenerate credential, delete.
- **VMs** (`/orgs/:orgId/vms`, already in the sidebar): table with name, owner, node, state, vCPU, RAM, disk; tabs All / Mine / Teams. Create dialog: name, owner, node, size (vCPU, RAM, disk), and a hint which SSH keys and variables the VM will get. Detail: state, error, IP (NAT address, reachable from the node only), power actions, delete.
- **Profile → SSH keys:** list, add, delete.
- Actions shown only when permitted (`can()` as today). de + en.

```
VMs                                                              [+ Create VM]
 All   Mine   Teams
NAME       OWNER          NODE      STATE     vCPU  RAM    DISK
dev        me             linux-1   running   2     4 GB   20 GB   Start  Shut down  Stop  Delete
ci-runner  team Backend   linux-2   stopped   4     8 GB   40 GB   Start  Delete
```

## Left to the builder

- REST paths and payloads; the node enrollment endpoint; how the installer receives the hub URL and token.
- How much of the PoC's node code is copied now vs. with projects 2/3 (chassis, NBD); dead code for later projects must not ship disabled-but-reachable.
- Credential format and hashing; constant-time comparison.
- Where the registry lives (`internal/nodes`?) and how the desired list is rebuilt on change (PoC: full list + sequence number).
- Where `ResolveEnv` is called (hub at push time); how the node escapes values in `/etc/environment`.
- Test strategy; must include: node of org A cannot receive or affect org B's VMs, revoked credential is refused, enrollment token single use and expiry, limits refused at the boundary (user, team, org), permission table above incl. deny rules, variables never in API responses / logs / node status, private variables never in a team VM or another user's VM. Real-hardware test of the Firecracker path on a Linux test VM (as in the PoC).
- Update docs/permissions.md; add docs for installing a node.

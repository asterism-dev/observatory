# Permissions

How access works inside an organization. Platform-level administration is covered at the end.

## Model

A rule says: **subject** may (allow) or may not (deny) do **action** on **object**, within one org.
Everything is scoped to a single org; resources and grants never cross org boundaries. A request for an
org the caller is not a member of returns `404`.

**Subjects**

| Subject | Meaning |
|---|---|
| `user:<id>` | a person; inherits the role from their membership, their teams and their direct bindings |
| `team:<id>` | every member of the team |
| `role:<owner\|admin\|member\|id>` | a built-in or custom role |

**Objects** have four pattern forms, `*` only ever as the whole last segment:

| Form | Example | Matches |
|---|---|---|
| exact | `org/teams` | that object |
| per type | `org/teams/*` | every object of that type |
| per resource | `org/teams/<id>` | one resource |
| whole scope | `org/*`, `user:<id>/*` | everything in the org or under that user |

Org-scoped objects are `org/...`; objects owned by a user are `user:<id>/...`. IDs must be canonical UUIDs.

**Evaluation:** deny wins; no matching allow means denied. Changes apply to the next request.

## Catalog

| Action | Object |
|---|---|
| `org:update` | `org/settings` |
| `member:read`, `member:invite`, `member:remove`, `member:set-role` | `org/members` |
| `team:read`, `team:create` | `org/teams` |
| `team:update`, `team:delete`, `team:manage-members`, `team:leave` | `org/teams/*` |
| `role:read`, `role:write`, `role:bind` | `org/roles` |
| `limit:read`, `limit:set` | `org/limits` |
| `audit:read` | `org/audit` |
| `env:list`, `env:set`, `env:delete`, `env:use` | `org/env`, `org/team-env/*`, `org/user-env/*` |
| `integration:list`, `integration:set`, `integration:delete`, `integration:use` | `org/integrations`, `org/team-integrations/*`, `org/user-integrations/*` |
| `node:read`, `node:create` | `org/nodes` |
| `node:read`, `node:update`, `node:delete`, `node:use` | `org/nodes/*` |
| `vm:create` | `org/vms` (user VMs), `org/team-vms/*` (team VMs) |
| `vm:read`, `vm:power`, `vm:delete`, `vm:network`, `vm:disks`, `vm:connect`, `vm:snapshot`, `vm:restore`, `vm:fork` | `org/vms/*` |
| `device:create`, `device:list`, `device:delete` | `org/user-devices/*` |
| `network:read`, `network:create` | `org/networks` |
| `network:use`, `network:delete` | `org/networks/*` |
| `disk:create` | `org/disks` (user disks), `org/team-disks/*` (team disks) |
| `disk:read`, `disk:update`, `disk:delete`, `disk:use` | `org/disks/*` |

## Built-in roles

Defined in code, identical in every org, immutable. Each role includes the one to its left.

| Permission | member | admin | owner |
|---|---|---|---|
| `member:read`, `team:read`, `team:leave`, `role:read`, `limit:read` | yes | yes | yes |
| `org:update`, `member:invite`, `member:remove` | | yes | yes |
| `team:*` (create, update, delete, manage members, leave) | | yes | yes |
| `role:write`, `role:bind`, `limit:set`, `audit:read` | | yes | yes |
| `member:set-role` | | | yes |
| `env:list`, `env:use` on `org/env` | yes | yes | yes |
| `env:*` on `org/env` and `org/team-env/*` | | yes | yes |
| `integration:list`, `integration:use` on `org/integrations` | yes | yes | yes |
| `integration:*` on `org/integrations` and `org/team-integrations/*` | | yes | yes |
| `node:read` on `org/nodes` and `org/nodes/*`, `node:use` on `org/nodes/*` | yes | yes | yes |
| `node:*` (create, update, delete) on `org/nodes` and `org/nodes/*` | | yes | yes |
| `vm:create` on `org/vms` | yes | yes | yes |
| `vm:create` on `org/team-vms/*`; `vm:read`, `vm:power`, `vm:delete`, `vm:network`, `vm:disks`, `vm:snapshot`, `vm:restore` on `org/vms/*` | | yes | yes |
| `network:read`, `network:create` on `org/networks`, `network:use` on `org/networks/*` | yes | yes | yes |
| `network:delete` on `org/networks/*` | | yes | yes |
| `device:list`, `device:delete` on `org/user-devices/*` | | yes | yes |
| `disk:create` on `org/disks` | yes | yes | yes |
| `disk:create` on `org/team-disks/*`; `disk:read`, `disk:update`, `disk:delete` on `org/disks/*` | | yes | yes |

No built-in role holds `vm:connect`, `vm:fork`, `disk:use` or `device:create` on others' objects; those come only from the
generated rules below. VM actions are always granted as explicit lists, never `vm:*`, so a new VM action
is never granted by accident. Disk actions are explicit too, never `disk:*`.

## Custom roles and bindings

- A custom role belongs to one org: a name, rules (allow/deny, object pattern, action) and parents.
  Parents may be `member`, `admin` or other custom roles of the same org, never `owner`.
- Inheritance chains (subject, team, role, parents) are limited to depth 8; cycles are rejected.
- A **binding** attaches a role to a user or a team, org-wide. Shares (a binding on a single object) arrive
  with the first resource type.
- Allow rules with action wildcards (`team:*`) are expanded against the catalog when written, so they
  never silently gain future actions. Deny rules keep their wildcard, so a deny also covers future actions.

## Deny rules and the escalation rule

- Only org owners and admins may write deny rules. Only they may edit, delete, bind or unbind roles that
  carry denies (directly or inherited), or remove people from a team bound to such a role.
- Removing a member from the org drops their bindings and teams, so removing someone a deny applies to
  also needs an owner or admin.
- **Escalation rule:** nobody can grant, through role rules, parents or bindings, a permission they do not
  hold themselves (and are not blocked from by their own denies). Violations return `403` and are audited.
- Removing a binding, or editing or deleting a role, in a way that lifts a deny currently applying to
  yourself is forbidden unless you are an owner.

## Owners

- An org may have several owners. Only owners (or holders of `member:set-role`, which an owner can grant via a custom role) promote to or demote from `admin` or `owner`.
- The last owner cannot be demoted, removed or leave.
- Personal orgs are single-member and fixed: no invitations, role changes, removal, leaving or renaming.

## Invitations

A link with a single-use token, valid 7 days; the hub sends no mail. Inviting as `admin` or `owner`
requires being owner (or holding `member:set-role`). At accept time the creator must still be a member and still hold `member:invite`
(plus `member:set-role` for admin/owner invites); otherwise the invite is invalid and must be reissued.

## Teams

A team has members, some of whom are team admins. Team admins manage membership of their team but cannot
demote or remove a peer team admin; that needs `team:update`. Binding a role to a team gives it to every
member.

Members leave a team themselves with `team:leave`, which every member has by default; an org can forbid it
with a deny rule (e.g. `deny team:leave@org/teams/*`, or one team only). Leaving never lifts a deny that
reaches you through the team, unless you are an owner.

## Environment variables

Variables (plain or secret) live in three scopes: the org (`org/env`), a team (`org/team-env/<team>`) and
each member's private scope (`org/user-env/<user>`).

- `env:list` lists names and metadata, including the hosts a secret is bound to; `env:set` and `env:delete`
  manage variables; `env:use` lets a subject's workloads receive the values. There is no action that reads a secret
  value back.
- Binding a secret to hosts, changing its hosts and unbinding it (which delivers the real value at the VMs' next
  boot) need `env:set` on the variable's scope, like changing the value. Placeholders are never returned
  ([egress.md](egress.md)).
- `vm:read` on a VM shows its Egress line: the host names and the number of the owner's secrets bound for it, never
  the secrets' names, values or placeholders. Org admins therefore see which hosts a member's VM has secrets for.
- Team members get `env:list` and `env:use` on their team's scope; the team subject itself gets `env:use`
  on `org/env` and its own scope. Team admins also get `env:set` and `env:delete` on their team.
- A private scope is reachable only by its own user, who holds all four actions on it; no built-in role,
  not even owner, covers another member's private scope. Deny rules still win, e.g.
  `deny env:set@org/user-env/*`.

## Integrations

GitHub, Claude and Linear logins ([integrations.md](integrations.md)) use the same three scopes and the same defaults
as variables: the org (`org/integrations`), a team (`org/team-integrations/<team>`) and each member's private scope
(`org/user-integrations/<user>`).

- `integration:list` shows kind, source, scope, delivery mode, account, status and expiry; `integration:set` connects,
  replaces, signs in again and changes the mode; `integration:delete` disconnects; `integration:use` means "VMs I own
  receive this integration" and is checked when a VM's integrations are resolved. No action returns a token or
  placeholder.
- Generated rules: team admins get `integration:*` on their team's object; the team subject gets `integration:list`
  and `integration:use` on its own object and `integration:use` on `org/integrations`; every member gets
  `integration:*` on their own `org/user-integrations/<user>`. No built-in role, not even owner, covers another
  member's private scope.
- Deny rules win as usual, e.g. `deny integration:set@org/user-integrations/*` ("no private integrations") or
  `deny integration:use@org/integrations` (org integrations reach no VM). All actions go through the escalation rule.

## Nodes and VMs

Every VM object is org-scoped (`org/vms/<id>`), whether a user or a team owns it, so org-wide rules such
as `deny vm:delete@org/vms/*` also cover a member's own VMs. Owner rights come from generated rules:

- A user VM gives its user `vm:read`, `vm:power`, `vm:delete`, `vm:network` and `vm:connect` on that VM.
- A team VM gives the team subject `vm:read`, `vm:power` and `vm:connect` on it, and each admin of that
  team `vm:read`, `vm:power`, `vm:delete` and `vm:network`.
- Team admins also get `vm:create` on `org/team-vms/<team>`, so they create VMs for their own team only.

Org admins and owners see, power, delete and re-network every VM (`vm:read`, `vm:power`, `vm:delete`,
`vm:network` on `org/vms/*`); they never get `vm:connect`, so no action lets them look inside a VM.

**Connect check.** Whether a person's devices may reach a VM is decided by `CanConnect`, not by
`vm:connect` alone: a user VM admits only its owner, a team VM only members of the owning team, and then
`vm:connect` must still be allowed, so an org deny such as `deny vm:connect@org/vms/*` cuts even the owner
off. A custom role granting `vm:connect` has no effect on connecting in this version, and since nobody
holds `vm:connect` on `org/vms/*`, the escalation rule rejects such grants anyway. Members see every node and may place VMs on it (`node:use`); only admins and owners
add, rename or delete nodes.

A member who still owns VMs or disks cannot be removed from the org or leave it, and a team that owns VMs or
disks cannot be deleted (`409` "owns VMs or disks").

## VM lifecycle

Pause, hibernate, resume and the auto-suspend setting are power actions (`vm:power`). Snapshots are VM sub-resources,
so their actions are on the VM object; listing snapshots and reading metrics need `vm:read`
([vm-lifecycle.md](vm-lifecycle.md)).

| Action | Allows |
|---|---|
| `vm:power` | start, shut down, stop, pause, hibernate, resume, set auto-suspend |
| `vm:delete` | delete the VM; set, change or clear its [delete-after](vm-lifecycle.md#delete-after) time |
| `vm:snapshot` | take and delete snapshots |
| `vm:restore` | restore the VM to one of its snapshots |
| `vm:fork` | create a new VM from a snapshot; the owner also needs `vm:create` for the new VM |

- A user VM gives its user `vm:snapshot`, `vm:restore` and `vm:fork` on it.
- A team VM gives the team subject `vm:snapshot`, and each admin of that team `vm:snapshot`, `vm:restore` and
  `vm:fork`.
- Org admins and owners get `vm:snapshot` and `vm:restore` on every VM, never `vm:fork`: a fork holds the source's
  memory and disks, so like `vm:connect` and `disk:use` it is data access. All three actions go through the escalation
  rule.

| | member | team admin (of that team) | admin | owner |
|---|---|---|---|---|
| own VMs: pause, hibernate, auto-suspend, snapshot, restore, fork | yes | yes | yes | yes |
| team VMs: pause, hibernate, auto-suspend, take and delete snapshots | team member | yes | yes | yes |
| team VMs: restore | | yes | yes | yes |
| team VMs: fork | | yes | | |
| every VM: power incl. hibernate, snapshots, restore | | | yes | yes |
| every VM: fork | | | | |

Audit rows: `vm:power` (details `action`, or `autoSuspendMinutes`), `vm:snapshot` (name, memory, disk names),
`vm:snapshot-delete`, `vm:restore` (snapshot name, memory), `vm:fork` on the source (details: the new VM) plus
`vm:create` for the new VM. Suspend and wake by the node are logged, not audited.

## Disks

Every disk object is org-scoped (`org/disks/<id>`), whether a user or a team owns it, so org-wide rules such
as `deny disk:delete@org/disks/*` also cover a member's own disks. Owner rights come from generated rules:

- A user disk gives its user `disk:read`, `disk:update`, `disk:delete` and `disk:use` on that disk.
- A team disk gives the team subject `disk:read` and `disk:use`, and each admin of that team `disk:read`,
  `disk:update`, `disk:delete` and `disk:use`.
- Team admins also get `disk:create` on `org/team-disks/<team>`.
- Owners of a user VM and admins of the owning team of a team VM get `vm:disks` on it (attach and detach).

Org admins and owners may detach and delete any disk (`disk:read`, `disk:update`, `disk:delete`,
`vm:disks`) but never get `disk:use`, so no action lets them read a member's disk contents. An org admin can
still add themselves to a team (`team:update`) and so gets `disk:use` on that team's disks, as with team VMs;
user disks stay out of reach.

## Devices and networks

A device is a person's WireGuard peer in one org, kept under `org/user-devices/<user>`. Every member gets
`device:create`, `device:list` and `device:delete` on their own object (a generated rule); admins and
owners list and delete everyone's devices but never create devices for others.

Networks (`org/networks/<id>`) connect VMs of one org to each other. Members read, create and use
(attach VMs to) networks; only admins and owners delete them. Attaching a VM also needs `vm:network` on
that VM. The connect check does not apply to network traffic: every VM on a network reaches the others.

## Limits

Fields: vCPU, RAM (MB), disk (GB), VM count, snapshots (`snapshotCount`); `null` is unlimited. Set per org by platform admins, per team
and per member by org admins (`limit:set`). They stack: a team or member limit cannot exceed the org limit, and an org limit cannot be lowered below an existing
team or member limit (both return `400` "limit exceeds org limit"). Creating a VM that would exceed a limit returns `409` "limit exceeded".

## Platform admins

A platform admin administers the installation: lists all orgs and their members, sets org limits, manages
the platform-admin flag of users and reads the global audit log. It grants **no** tenant permissions: a
platform admin has no access to an org's data unless they are a member. The last platform admin cannot be
revoked (`409`).

## Audit

Every state change writes an audit row in the same transaction. Every denied authorization writes a row
with result `denied`, actor, org, action and object.

## Not yet built

Shares (per-object bindings) and per-resource cost centers are not built yet. Org deletion is also pending.

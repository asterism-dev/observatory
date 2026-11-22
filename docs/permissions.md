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

**Evaluation:** deny wins; no matching allow means denied. Changes reach every service with the policy stream's latency.

## Action catalog

The actions a rule can name, as registered today:

| Action | Object |
|---|---|
| `iam.orgs.update`, `telemetry.tokens.create` | `org/settings` |
| `iam.members.list`, `iam.invites.create`, `iam.members.delete`, `iam.members.setRole` | `org/members` |
| `iam.teams.list`, `iam.teams.create` | `org/teams` |
| `iam.teams.update`, `iam.teams.delete`, `iam.teams.manageMembers`, `iam.teams.leave` | `org/teams/*` |
| `iam.roles.list`, `iam.roles.manage`, `iam.roleBindings.manage` | `org/roles` |
| `quota.limits.get`, `quota.limits.update` | `org/limits` |
| `audit.events.list`, `audit.settings.get`, `audit.settings.update` | `org/audit` |
| `telemetry.logEntries.list` | `org/logs` |
| `secrets.variables.list`, `secrets.variables.set`, `secrets.variables.delete`, `secrets.variables.use` | `org/env`, `org/team-env/*`, `org/user-env/*` |
| `secrets.integrations.list`, `secrets.integrations.set`, `secrets.integrations.delete`, `secrets.integrations.use` | `org/integrations`, `org/team-integrations/*`, `org/user-integrations/*` |
| `telemetry.dashboards.get`, `telemetry.dashboards.update`, `telemetry.dashboards.create` | `org/dashboards`, `org/team-dashboards/*`, `org/user-dashboards/*` |
| `compute.nodes.get`, `compute.nodes.create` | `org/nodes` |
| `compute.nodes.get`, `compute.nodes.update`, `compute.nodes.delete`, `compute.nodes.use`, `compute.nodes.getMetrics` | `org/nodes/*` |
| `compute.instances.create` | `org/vms` (user VMs), `org/team-vms/*` (team VMs) |
| `compute.instances.get`, `compute.instances.setPower`, `compute.instances.delete`, `compute.instances.setNetworks`, `compute.instances.setDisks`, `compute.instances.connect`, `compute.instances.snapshot`, `compute.instances.restore`, `compute.instances.fork`, `compute.instances.getMetrics` | `org/vms/*` |
| `network.devices.create`, `network.devices.list`, `network.devices.delete` | `org/user-devices/*` |
| `network.networks.get`, `network.networks.create` | `org/networks` |
| `network.networks.use`, `network.networks.delete` | `org/networks/*` |
| `compute.disks.create` | `org/disks` (user disks), `org/team-disks/*` (team disks) |
| `compute.disks.get`, `compute.disks.update`, `compute.disks.delete`, `compute.disks.use` | `org/disks/*` |
| `artifacts.images.list`, `artifacts.images.use`, `artifacts.images.update`, `artifacts.images.setDefault`, `build.builds.create` | `org/images`, `org/team-images/*`, `org/user-images/*` |

Permissions are data. Each service sends iam its catalog at start: its actions (name, object, optional description),
built-in role defaults and owner templates. iam stores one catalog per service, streams them with the policy, checks
custom rules against them and serves them to the role editor (`ListCatalog`, CLI `role catalog`).

- **Registration:** `CatalogInternalService/RegisterCatalog` with the service's token. The allowed action prefixes come
  from the token's caller, never from the catalog: a service names only its own actions (`audit.*` for audit). The hub
  also registers the actions of services not yet extracted (`compute`, `network`, `secrets`, `artifacts`, `build`,
  `telemetry`, `quota`). Role defaults and templates use a concrete action, `<service>.<resource>.*` or `<service>.*`,
  never `*`. Limits: 500 actions, 500 role defaults, 200 templates, 1 MiB per request. A violation is refused
  (invalid argument, naming the first bad entry) and the stored catalog stays as it was. A registration that changes
  the catalog writes the audit event `iam.catalogs.update` with the added and removed actions.
- **Built-in roles:** a role default names the lowest role that gets it. Admin also gets member's defaults and owner
  gets admin's, so owner ⊇ admin ⊇ member whatever the data says.
- **Owner templates:** rules filled in per member (`{user}`), per team or team admin (`{team}`) and per owned
  resource (`org/<kind>/{id}` for its user, team or team admins). They give owners the rights listed under
  Nodes and VMs, Disks and Devices.
- **Unknown actions:** an action a custom rule names that no registered catalog has, for example after a service
  dropped it. The rule is kept and marked unknown in the role editor and `role catalog`, and an allow for it grants
  nothing. A stored wildcard allow (`compute.*`) is marked unknown too and grants nothing. If a service registers that action again, the allow grants again. A deny for it is kept as written and
  still wins. New rules must name a registered action.
- **Readiness:** a service holds requests or answers 503 until iam's policy stream carries its own catalog digest, so
  it never answers 403 for actions its defaults grant. All instances of a service must run the same release: an
  instance whose catalog differs from the last one registered stays unavailable.

## Built-in roles

Built from the role defaults of the registered catalogs, identical in every org, immutable. Each role includes the one to its left.

| Permission | member | admin | owner |
|---|---|---|---|
| `iam.members.list`, `iam.teams.list`, `iam.teams.leave`, `iam.roles.list`, `quota.limits.get` | yes | yes | yes |
| `iam.orgs.update`, `telemetry.tokens.create`, `iam.invites.create`, `iam.members.delete` | | yes | yes |
| `iam.teams.*` (create, update, delete, manage members, leave) | | yes | yes |
| `iam.roles.manage`, `iam.roleBindings.manage`, `quota.limits.update`, `audit.events.list` | | yes | yes |
| `audit.settings.get`, `audit.settings.update` (data events per service) | | yes | yes |
| `telemetry.logEntries.list` (the org's logs explorer, all of the org's logs) | | yes | yes |
| `iam.members.setRole` | | | yes |
| `secrets.variables.list`, `secrets.variables.use` on `org/env` | yes | yes | yes |
| `secrets.variables.*` on `org/env` and `org/team-env/*` | | yes | yes |
| `secrets.integrations.list`, `secrets.integrations.use` on `org/integrations` | yes | yes | yes |
| `secrets.integrations.*` on `org/integrations` and `org/team-integrations/*` | | yes | yes |
| `telemetry.dashboards.get` on `org/dashboards` | yes | yes | yes |
| `telemetry.dashboards.*` on `org/dashboards` and `org/team-dashboards/*` | | yes | yes |
| `compute.nodes.get` on `org/nodes` and `org/nodes/*`, `compute.nodes.use` on `org/nodes/*` | yes | yes | yes |
| `compute.nodes.*` (create, update, delete, metrics) on `org/nodes` and `org/nodes/*` | | yes | yes |
| `compute.instances.getMetrics` on `org/vms/*` | | yes | yes |
| `compute.instances.create` on `org/vms` | yes | yes | yes |
| `compute.instances.create` on `org/team-vms/*`; `compute.instances.get`, `compute.instances.setPower`, `compute.instances.delete`, `compute.instances.setNetworks`, `compute.instances.setDisks`, `compute.instances.snapshot`, `compute.instances.restore` on `org/vms/*` | | yes | yes |
| `network.networks.get`, `network.networks.create` on `org/networks`, `network.networks.use` on `org/networks/*` | yes | yes | yes |
| `network.networks.delete` on `org/networks/*` | | yes | yes |
| `network.devices.list`, `network.devices.delete` on `org/user-devices/*` | | yes | yes |
| `compute.disks.create` on `org/disks` | yes | yes | yes |
| `artifacts.images.list`, `artifacts.images.use` on `org/images` | yes | yes | yes |
| `artifacts.images.*` and `build.builds.create` on `org/images` and `org/team-images/*` | | yes | yes |
| `compute.disks.create` on `org/team-disks/*`; `compute.disks.get`, `compute.disks.update`, `compute.disks.delete` on `org/disks/*` | | yes | yes |

No built-in role holds `compute.instances.connect`, `compute.instances.fork`, `compute.disks.use` or `network.devices.create` on others' objects; those come only from the
owner templates below. VM actions are always granted as explicit lists, never `compute.instances.*`, so a new VM action
is never granted by accident. Disk actions are explicit too, never `compute.disks.*`.

## Custom roles and bindings

- A custom role belongs to one org: a name, rules (allow/deny, object pattern, action) and parents.
  Parents may be `member`, `admin` or other custom roles of the same org, never `owner`.
- Inheritance chains (subject, team, role, parents) are limited to depth 8; cycles are rejected.
- A **binding** attaches a role to a user or a team, org-wide. Shares (a binding on a single object) arrive
  with the first resource type.
- Allow rules with action wildcards (`iam.teams.*`) are expanded against the catalog when written, so they
  never silently gain future actions. Deny rules keep their wildcard, so a deny also covers future actions.
- Wildcards follow resource boundaries: `iam.roles.*` does not include `iam.roleBindings.manage`, and `iam.members.*`
  does not include `iam.invites.create`. The former single image wildcard became two rules, `artifacts.images.*` and `build.builds.create`.

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

- An org may have several owners. Only owners (or holders of `iam.members.setRole`, which an owner can grant via a custom role) promote to or demote from `admin` or `owner`.
- The last owner cannot be demoted, removed or leave.
- Personal orgs are single-member and fixed: no invitations, role changes, removal, leaving or renaming.

## Invitations

A link with a single-use token, valid 7 days; Observatory sends no mail. Inviting as `admin` or `owner`
requires being owner (or holding `iam.members.setRole`). At accept time the creator must still be a member and still hold `iam.invites.create`
(plus `iam.members.setRole` for admin/owner invites); otherwise the invite is invalid and must be reissued.

## Teams

A team has members, some of whom are team admins. Team admins manage membership of their team but cannot
demote or remove a peer team admin; that needs `iam.teams.update`. Binding a role to a team gives it to every
member.

Members leave a team themselves with `iam.teams.leave`, which every member has by default; an org can forbid it
with a deny rule (e.g. `deny iam.teams.leave@org/teams/*`, or one team only). Leaving never lifts a deny that
reaches you through the team, unless you are an owner.

## Environment variables

Variables (plain or secret) live in three scopes: the org (`org/env`), a team (`org/team-env/<team>`) and
each member's private scope (`org/user-env/<user>`).

- `secrets.variables.list` lists names and metadata, including the hosts a secret is bound to; `secrets.variables.set` and `secrets.variables.delete`
  manage variables; `secrets.variables.use` lets a subject's workloads receive the values. There is no action that reads a secret
  value back.
- Binding a secret to hosts, changing its hosts and unbinding it (which delivers the real value at the VMs' next
  boot) need `secrets.variables.set` on the variable's scope, like changing the value. Placeholders are never returned
  ([egress.md](egress.md)).
- `compute.instances.get` on a VM shows its Egress line: the host names and the number of the owner's secrets bound for it, never
  the secrets' names, values or placeholders. Org admins therefore see which hosts a member's VM has secrets for.
- Team members get `secrets.variables.list` and `secrets.variables.use` on their team's scope; the team subject itself gets `secrets.variables.use`
  on `org/env` and its own scope. Team admins also get `secrets.variables.set` and `secrets.variables.delete` on their team.
- A private scope is reachable only by its own user, who holds all four actions on it; no built-in role,
  not even owner, covers another member's private scope. Deny rules still win, e.g.
  `deny secrets.variables.set@org/user-env/*`.

## Integrations

GitHub, Claude and Linear logins ([integrations.md](integrations.md)) use the same three scopes and the same defaults
as variables: the org (`org/integrations`), a team (`org/team-integrations/<team>`) and each member's private scope
(`org/user-integrations/<user>`).

- `secrets.integrations.list` shows kind, source, scope, delivery mode, account, status and expiry; `secrets.integrations.set` connects,
  replaces, signs in again and changes the mode; `secrets.integrations.delete` disconnects; `secrets.integrations.use` means "VMs I own
  receive this integration" and is checked when a VM's integrations are resolved. No action returns a token or
  placeholder.
- Generated rules: team admins get `secrets.integrations.*` on their team's object; the team subject gets `secrets.integrations.list`
  and `secrets.integrations.use` on its own object and `secrets.integrations.use` on `org/integrations`; every member gets
  `secrets.integrations.*` on their own `org/user-integrations/<user>`. No built-in role, not even owner, covers another
  member's private scope.
- Deny rules win as usual, e.g. `deny secrets.integrations.set@org/user-integrations/*` ("no private integrations") or
  `deny secrets.integrations.use@org/integrations` (org integrations reach no VM). All actions go through the escalation rule.

## Dashboards

Dashboards live in the same three scopes: the org (`org/dashboards`), a team (`org/team-dashboards/<team>`) and
each member's private scope (`org/user-dashboards/<user>`).

- `telemetry.dashboards.get` shows a dashboard, `telemetry.dashboards.create` creates one in (or moves one into) a scope, `telemetry.dashboards.update`
  changes or deletes any dashboard of the scope.
- Generated rules: the team subject gets `telemetry.dashboards.get` and `telemetry.dashboards.create` on its own object; team admins get
  `telemetry.dashboards.*` on it; every member gets `telemetry.dashboards.*` on their own `org/user-dashboards/<user>`. No built-in role,
  not even owner, covers another member's private scope.
- A dashboard's creator may also edit it while it is in the org scope, or in a team scope while they are a member of
  that team; this is checked by the API, not by a rule.

## Nodes and VMs

Every VM object is org-scoped (`org/vms/<id>`), whether a user or a team owns it, so org-wide rules such
as `deny compute.instances.delete@org/vms/*` also cover a member's own VMs. Owner rights come from owner templates:

- A user VM gives its user `compute.instances.get`, `compute.instances.setPower`, `compute.instances.delete`, `compute.instances.setNetworks` and `compute.instances.connect` on that VM.
- A team VM gives the team subject `compute.instances.get`, `compute.instances.setPower` and `compute.instances.connect` on it, and each admin of that
  team `compute.instances.get`, `compute.instances.setPower`, `compute.instances.delete` and `compute.instances.setNetworks`.
- Team admins also get `compute.instances.create` on `org/team-vms/<team>`, so they create VMs for their own team only.

Org admins and owners see, power, delete and re-network every VM (`compute.instances.get`, `compute.instances.setPower`, `compute.instances.delete`,
`compute.instances.setNetworks` on `org/vms/*`); they never get `compute.instances.connect`, so no action lets them look inside a VM.

**Connect check.** Whether a person's devices may reach a VM is decided by `CanConnect`, not by
`compute.instances.connect` alone: a user VM admits only its owner, a team VM only members of the owning team, and then
`compute.instances.connect` must still be allowed, so an org deny such as `deny compute.instances.connect@org/vms/*` cuts even the owner
off. A custom role granting `compute.instances.connect` has no effect on connecting in this version, and since nobody
holds `compute.instances.connect` on `org/vms/*`, the escalation rule rejects such grants anyway. Members see every node and may place VMs on it (`compute.nodes.use`); only admins and owners
add, rename or delete nodes. Node usage graphs and logs need `compute.nodes.getMetrics`; admins and owners have it, members only through a custom role.

A member who still owns VMs or disks cannot be removed from the org or leave it, and a team that owns VMs or
disks cannot be deleted (failed precondition, "owns VMs or disks").

## VM lifecycle

Pause, hibernate, resume and the auto-suspend setting are power actions (`compute.instances.setPower`). Snapshots are VM sub-resources,
so their actions are on the VM object; listing snapshots needs `compute.instances.get`
([vm-lifecycle.md](vm-lifecycle.md)).

| Action | Allows |
|---|---|
| `compute.instances.setPower` | start, shut down, stop, pause, hibernate, resume, set auto-suspend |
| `compute.instances.delete` | delete the VM; set, change or clear its [delete-after](vm-lifecycle.md#delete-after) time |
| `compute.instances.snapshot` | take and delete snapshots |
| `compute.instances.restore` | restore the VM to one of its snapshots |
| `compute.instances.fork` | create a new VM from a snapshot; the owner also needs `compute.instances.create` for the new VM |
| `compute.instances.getMetrics` | read the VM's telemetry: usage, processes and logs |

- A user VM gives its user `compute.instances.snapshot`, `compute.instances.restore` and `compute.instances.fork` on it.
- A team VM gives the team subject `compute.instances.snapshot`, and each admin of that team `compute.instances.snapshot`, `compute.instances.restore` and
  `compute.instances.fork`.
- Org admins and owners get `compute.instances.snapshot` and `compute.instances.restore` on every VM, never `compute.instances.fork`: a fork holds the source's
  memory and disks, so like `compute.instances.connect` and `compute.disks.use` it is data access. All three actions go through the escalation
  rule.
- A user VM gives its user `compute.instances.getMetrics`; a team VM gives it to the team subject and each team admin. Org admins and
  owners may read every VM's telemetry (`compute.instances.getMetrics`, incl. logs and processes) but still never connect.

| | member | team admin (of that team) | admin | owner |
|---|---|---|---|---|
| own VMs: pause, hibernate, auto-suspend, snapshot, restore, fork | yes | yes | yes | yes |
| team VMs: pause, hibernate, auto-suspend, take and delete snapshots | team member | yes | yes | yes |
| team VMs: restore | | yes | yes | yes |
| team VMs: fork | | yes | | |
| every VM: power incl. hibernate, snapshots, restore | | | yes | yes |
| every VM: fork | | | | |

Audit rows: `compute.instances.setPower` (details `action`, or `autoSuspendMinutes`), `compute.instances.snapshot` (name, memory, disk names),
`compute.snapshots.delete`, `compute.instances.restore` (snapshot name, memory), `compute.instances.fork` on the source (details: the new VM) plus
`compute.instances.create` for the new VM. Suspend and wake by the node are logged, not audited.

## Disks

Every disk object is org-scoped (`org/disks/<id>`), whether a user or a team owns it, so org-wide rules such
as `deny compute.disks.delete@org/disks/*` also cover a member's own disks. Owner rights come from owner templates:

- A user disk gives its user `compute.disks.get`, `compute.disks.update`, `compute.disks.delete` and `compute.disks.use` on that disk.
- A team disk gives the team subject `compute.disks.get` and `compute.disks.use`, and each admin of that team `compute.disks.get`,
  `compute.disks.update`, `compute.disks.delete` and `compute.disks.use`.
- Team admins also get `compute.disks.create` on `org/team-disks/<team>`.
- Owners of a user VM and admins of the owning team of a team VM get `compute.instances.setDisks` on it (attach and detach).

Org admins and owners may detach and delete any disk (`compute.disks.get`, `compute.disks.update`, `compute.disks.delete`,
`compute.instances.setDisks`) but never get `compute.disks.use`, so no action lets them read a member's disk contents. An org admin can
still add themselves to a team (`iam.teams.update`) and so gets `compute.disks.use` on that team's disks, as with team VMs;
user disks stay out of reach.

## Devices and networks

A device is a person's WireGuard peer in one org, kept under `org/user-devices/<user>`. Every member gets
`network.devices.create`, `network.devices.list` and `network.devices.delete` on their own object (an owner template); admins and
owners list and delete everyone's devices but never create devices for others.

Networks (`org/networks/<id>`) connect VMs of one org to each other. Members read, create and use
(attach VMs to) networks; only admins and owners delete them. Attaching a VM also needs `compute.instances.setNetworks` on
that VM. The connect check does not apply to network traffic: every VM on a network reaches the others.

## Limits

Fields: vCPU, RAM (MB), disk (GB), VM count, snapshots (`snapshotCount`); `null` is unlimited. Set per org by platform admins, per team
and per member by org admins (`quota.limits.update`). They stack: a team or member limit cannot exceed the org limit, and an org limit cannot be lowered below an existing
team or member limit (both return `400` "limit exceeds org limit"). Creating a VM that would exceed a limit returns `409` "limit exceeded".

## Platform admins

A platform admin administers the installation: lists all orgs and their members, sets org limits, manages
the platform-admin flag of users and reads the global audit log. It grants **no** tenant permissions: a
platform admin has no access to an org's data unless they are a member. The last platform admin cannot be
revoked (failed precondition).

## How services authorize

Extracted services do not see sessions. The edge exchanges the user's session (cookie or bearer token) for a short-lived
signed token (EdDSA, valid at most 5 minutes and never longer than the session) and sends it to the service in place of
the client's `Authorization` header. The edge caches it for at most 60 seconds, so a revoked session reaches the
services within about a minute. A logout through the edge, or a 401 from any upstream (hub, iam or another service)
for that session, drops the cached token at once. A removed platform admin role also takes up to a minute, on hub
routes too, which read it from the token. Services verify the token offline against iam's published keys.

- A leaked identity token stays valid at the services until it expires: at most 5 minutes plus 30 seconds of clock skew
  allowance. Revoking the session stops new tokens, not one already issued.
- While iam is unreachable the edge cannot exchange sessions and answers 502 for session requests.
- Only the edge may exchange sessions; other services get 403 from the token endpoint and use the keys and the policy
  stream only.
- iam publishes one signing key today. The key set is a list so a previous key can stay published during a
  rotation, which comes later.
- The hub's own `/api/` routes use the same token: the edge swaps a session (the `obs_session` cookie or an
  `obs_…` bearer) for it, removes the session cookie and the hub verifies the token offline. The hub never sees a
  session there, but on every request it asks iam (`GetSession`) whether the token's session is neither revoked nor
  expired, so hub routes and the IAM API refuse a logged-out or revoked session immediately. If iam cannot answer, the
  hub answers 503 rather than admit the request. Registry credentials are checked with iam the same way on every use. Other bearer tokens
  (node, gateway, storage, builder, registry events, telemetry) pass through unchanged. A stale session is dropped
  rather than refused, so public routes such as `/api/auth/config` keep working; protected ones answer 401. While the
  issuer is unreachable the edge answers 502 for session requests to hub routes as well. `/auth/` and `/api/auth/` go
  to iam with the session unchanged; a path there with dot segments reaches no handler (iam redirects to the clean
  path or answers 404).
- Services never see the edge token. The edge overwrites `X-Observatory-Client-IP` on every request, and a service
  records it only for a call whose identity token verified. Limitation: a leaked identity token (at most 5.5 minutes)
  could be used to forge the client IP in the audit trail.

Every RPC carries a permission annotation: a permission name (`service.resource.verb`), an object pattern and a kind.
The service's interceptor checks it before the handler runs:

- A method without a valid annotation is refused, never allowed.
- A caller who is not a member of the request's org gets not found; a member without the permission is denied.
- Requests without an `org` field are platform scope and only allowed for platform admins, except when the object
  starts with `user:`: then the owning user passes regardless of permission.
- The policy (roles, bindings, teams, memberships) is streamed from iam to every service and applies locally. A
  service refuses requests until it has received a policy.
- On a quiet stream iam resends the current policy every 30 seconds. A stream silent for 90 seconds counts as lost,
  and 5 minutes after the loss (the identity token's lifetime) the service has no policy and answers user requests
  with unavailable (`503`). So a removed member keeps access for at most 90 seconds plus 5 minutes while the stream is
  down. Exempt: the hub keeps the last policy for the gateway's allow lists and for what it pushes to nodes (SSH keys
  and env of running VMs), since dropping those would cut every VM off. A removed member's devices therefore keep
  reaching their VMs until the stream is back.
- If iam cannot reload its policy after a commit (a database error or a 5 second timeout), the commit still succeeds;
  iam retries the reload in the background and sends nothing on the stream until it succeeds, not even the first
  snapshot to a service that reconnects, so the services hit the same
  90 seconds plus 5 minutes bound instead of trusting a policy that misses the change. The hub likewise answers org
  routes with unavailable while a reload after one of its own commits fails.

## IAM API

Users, sessions, SSH keys, orgs, members, invites, teams, roles, bindings and the platform admin calls are one service,
`observatory.iam.v1.IAMService`, served at `/api/iam/v1/` over Connect and REST. Both take the same path
through authorization and audit. REST paths follow the methods, for example `GET /api/iam/v1/me`,
`DELETE /api/iam/v1/orgs/{org}/members/{user}` or `POST /api/iam/v1/invites:accept`; the Connect path is
`/api/iam/v1/observatory.iam.v1.IAMService/<Method>`. A Connect call must send `Connect-Protocol-Version: 1`;
without it the request is taken as REST and the Connect path answers 404. iam runs as its own service
(`observatory serve iam`, see [deploy.md](deploy.md#edge-iam-and-hub)).

- **`{self}`.** The calls on your own account (`GetMe`, sessions, SSH keys, `CreateOrg`, `AcceptInvite`, `LeaveOrg`)
  have the object `user:{self}`, and `{self}` is always the caller. A session or SSH key ID of another user is not
  found, and nothing changes.
- **Membership only.** `GetOrg` and `LeaveOrg` need membership and no permission: a member passes even under a
  `deny * on *` rule, and a non-member gets not found.
- Removing yourself is `LeaveOrg` or `LeaveTeam`; `DeleteMember` and `RemoveTeamMember` refuse your own ID.
- Errors map as follows. Not found and permission denied mean what they say. Conflicts such as the last owner or a
  member who still owns VMs are failed precondition, `400` over REST (0.x answered `409`). Bad input is invalid
  argument.
- A REST request with a query string is refused with `400` before authentication, so a query field cannot override
  the org in the path.
- Request bodies are capped at 1 MiB; a larger one is refused (resource exhausted) before authentication.
- Every call checks that the token's session is neither revoked nor expired, as hub routes do, so a logged-out or
  revoked session is refused (unauthenticated, `401` over REST) immediately.
- `UpdateRole` replaces the whole role (name, description, rules and parents), so its REST form is
  `PUT /api/iam/v1/orgs/{org}/roles/{role}`.
- Writes record their audit events in their own transaction, with the same types and resources as before, so the hub's
  cleanup sees each removal exactly once.
- The hub keeps `GET /api/info` (docs URL, version, latest version) and the platform admin's org limits
  (`GET`/`PUT /api/admin/orgs/{org}/limits`).

## Hub as iam client

The hub has no IAM tables. Its policy is iam's streamed policy plus the grants the hub derives from what it owns
(VMs, disks, images and the like); it rebuilds it on every stream message and after each of its own commits. At start
it waits up to 60 seconds for iam's first policy and otherwise exits. A non-member gets not found, a member without
the permission is denied. It reads user names, e-mails, SSH keys, team names, sessions and memberships through the
internal iam API (`/internal/iam/`, service token).

- A grant iam cannot justify from its own policy (an object the hub owns, such as a member's VM) goes to the hub
  (`CanHold`), which answers once it has iam's latest policy revision, or unavailable after 5 seconds.
- A login that creates a user waits up to 5 seconds for the hub to have the new personal org, so the web app's first
  hub request succeeds. Other iam changes reach the hub with the stream's latency.

What a removed member or deleted team leaves behind is cleaned up from events, not in the same transaction. The hub
consumes the allowed `iam.members.delete`, `iam.members.leave`, `iam.teams.delete`, `iam.sessions.delete` and
`iam.sessions.revoke` events (durable consumer `hub`) and then deletes the subject's private env vars, integrations,
images, dashboards, limits and devices, detaches a team from its disks and VM history, or drops the registry
credentials of the session. Denied events are ignored. Cleanup is idempotent and retried until it succeeds.

- Purge order: the purge takes the hub's org lock, then asks iam whether the user is a member again, and purges only if
  not. Every create of a user-owned row takes the same lock first. A create that saw a re-join commits before the
  purge, which then finds the member back and purges nothing; a create after the purge keeps its row. A create that
  waited behind the purge for a user who is gone leaves a row until the next purge, with no access, since access comes
  from the policy.
- Telemetry tokens the removed member created are revoked when they were authorized at or below the policy revision
  the removal event carries, also if the member has rejoined since; tokens authorized after the rejoin stay. The
  cutoff uses iam's revisions, not clocks. iam seeds its revisions from its clock at start (microseconds), so they keep
  increasing across restarts. If iam's clock is set back by more than the time between two starts, new revisions can
  fall below those of tokens authorized before, and a later removal's purge misses those tokens; the check on use that
  the token's creator still holds the grant, repeated at least every minute, revokes them anyway.

- Removing a member or deleting a team is refused (failed precondition) while the hub reports that the subject owns VMs or live
  disks. The check and the delete are not one transaction, so a VM created in between is not caught.
- Events stay on the bus for 7 days; cleanup that is delayed longer is lost.
- Access ends with the membership, not the cleanup: a removed member's WireGuard device stays a gateway peer until it is
  purged, but reaches no VM, because the allow lists come from the policy.
- Deleting a manifest (image cleanup) is best effort; a failure leaves a registry blob until garbage collection.
- Orgs and users cannot be deleted yet. A future delete must publish `iam.orgs.delete` or `iam.users.delete`, and the
  hub must then purge `dashboards`, `telemetry_tokens`, `vm_history` and `images.auto_by`.
- Registry credentials end with the session that issued them: every use asks iam (`GetSession`), so the registry
  refuses them as soon as the session is revoked or expired, also while the bus is down, and answers `503` while iam
  is unreachable.

## Audit

Every state change writes an audit event to its service's outbox (hub or iam) in the same transaction, so it is published only if the
change commits; the relay delivers it over the event bus to the audit service, which serves the trail at
`/api/audit/v1/`. Every denied authorization writes an event with result `denied`, actor, org, action and object. A denied row carries the same action name as the allowed one,
so consumers must filter on `result` when they count what happened.

Services other than the hub record their calls with a shared interceptor (`internal/platform/auditlog`): every write
and every denial of a verified caller, and reads only where the org turned on data events for that service (org
settings → Audit; `audit.settings.update`). Every service must build its handler chain with
`auditlog.Interceptors`, which puts the audit interceptor before authz, because authz ends a denied call before any
inner interceptor runs; authz reports the verified caller and its own denials back through the context. Only authz
denials are `denied`; a handler error (including not found) is `failed` and is recorded like any other call of its
kind. Calls without a verified identity are not recorded. Data events are offered only for services that record
through the interceptor (today `audit` and `iam`); the list grows as services are extracted. A settings change publishes `audit.settings.changed`, and services reload without a
restart.

## Not yet built

Shares (per-object bindings) and per-resource cost centers are not built yet. Org deletion is also pending.

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
| `node:read`, `node:create` | `org/nodes` |
| `node:read`, `node:update`, `node:delete`, `node:use` | `org/nodes/*` |
| `vm:create` | `org/vms` (user VMs), `org/team-vms/*` (team VMs) |
| `vm:read`, `vm:power`, `vm:delete` | `org/vms/*` |

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
| `node:read` on `org/nodes` and `org/nodes/*`, `node:use` on `org/nodes/*` | yes | yes | yes |
| `node:*` (create, update, delete) on `org/nodes` and `org/nodes/*` | | yes | yes |
| `vm:create` on `org/vms` | yes | yes | yes |
| `vm:*` on `org/vms`, `org/vms/*` and `org/team-vms/*` | | yes | yes |

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

- `env:list` lists names and metadata; `env:set` and `env:delete` manage variables; `env:use` lets a
  subject's workloads receive the values. There is no action that reads a secret value back.
- Team members get `env:list` and `env:use` on their team's scope; the team subject itself gets `env:use`
  on `org/env` and its own scope. Team admins also get `env:set` and `env:delete` on their team.
- A private scope is reachable only by its own user, who holds all four actions on it; no built-in role,
  not even owner, covers another member's private scope. Deny rules still win, e.g.
  `deny env:set@org/user-env/*`.

## Nodes and VMs

Every VM object is org-scoped (`org/vms/<id>`), whether a user or a team owns it, so org-wide rules such
as `deny vm:delete@org/vms/*` also cover a member's own VMs. Owner rights come from generated rules:

- A user VM gives its user `vm:*` on that VM.
- A team VM gives the team subject `vm:read` and `vm:power` on it, and each admin of that team `vm:*`.
- Team admins also get `vm:create` on `org/team-vms/<team>`, so they create VMs for their own team only.

Org admins and owners see, power and delete every VM through `vm:*` on `org/vms/*`; no action lets them
look inside a VM. Members see every node and may place VMs on it (`node:use`); only admins and owners
add, rename or delete nodes.

A member who still owns VMs cannot be removed from the org or leave it, and a team that owns VMs cannot be
deleted (`409` "owns VMs").

## Limits

Fields: vCPU, RAM (MB), disk (GB), VM count; `null` is unlimited. Set per org by platform admins, per team
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

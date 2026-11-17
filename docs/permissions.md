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

**Objects** have three pattern forms, `*` only ever as the whole last segment:

| Form | Example | Matches |
|---|---|---|
| exact | `org/teams` | that object |
| per type | `org/teams/*` | every object of that type |
| per resource | `org/teams/<id>` | one resource |

Org-scoped objects are `org/...`; objects owned by a user are `user:<id>/...`. IDs must be canonical UUIDs.

**Evaluation:** deny wins; no matching allow means denied. Changes apply to the next request.

## Catalog

| Action | Object |
|---|---|
| `org:update` | `org/settings` |
| `member:read`, `member:invite`, `member:remove`, `member:set-role` | `org/members` |
| `team:read`, `team:create` | `org/teams` |
| `team:update`, `team:delete`, `team:manage-members` | `org/teams/*` |
| `role:read`, `role:write`, `role:bind` | `org/roles` |
| `limit:read`, `limit:set` | `org/limits` |
| `audit:read` | `org/audit` |

## Built-in roles

Defined in code, identical in every org, immutable. Each role includes the one to its left.

| Permission | member | admin | owner |
|---|---|---|---|
| `member:read`, `team:read`, `role:read`, `limit:read` | yes | yes | yes |
| `org:update`, `member:invite`, `member:remove` | | yes | yes |
| `team:*` (create, update, delete, manage members) | | yes | yes |
| `role:write`, `role:bind`, `limit:set`, `audit:read` | | yes | yes |
| `member:set-role` | | | yes |

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
- **Escalation rule:** nobody can grant, through role rules, parents or bindings, a permission they do not
  hold themselves (and are not blocked from by their own denies). Violations return `403` and are audited.
- Removing a binding, or editing or deleting a role, in a way that lifts a deny currently applying to
  yourself is forbidden unless you are an owner.

## Owners

- An org may have several owners. Only owners promote to or demote from `admin` or `owner`.
- The last owner cannot be demoted, removed or leave.
- Personal orgs are single-member: no invitations, no leaving, no renaming away from personal status.

## Invitations

A link with a single-use token, valid 7 days; the hub sends no mail. Inviting as `admin` or `owner`
requires being owner. At accept time the creator must still be a member and still hold `member:invite`
(plus `member:set-role` for admin/owner invites); otherwise the invite is invalid and must be reissued.

## Teams

A team has members, some of whom are team admins. Team admins manage membership of their team but cannot
demote or remove a peer team admin; that needs `team:update`. Binding a role to a team gives it to every
member.

## Limits

Fields: vCPU, RAM (MB), disk (GB), VM count; `null` is unlimited. Set per org by platform admins, per team
and per member by org admins (`limit:set`). They are configuration only and not enforced yet.

## Platform admins

A platform admin administers the installation: lists all orgs and their members, sets org limits, manages
the platform-admin flag of users and reads the global audit log. It grants **no** tenant permissions: a
platform admin has no access to an org's data unless they are a member. The last platform admin cannot be
revoked (`409`).

## Audit

Every state change writes an audit row in the same transaction. Every denied authorization writes a row
with result `denied`, actor, org, action and object.

## Not yet built

Shares (per-object bindings) and per-resource cost centers arrive with the first resource type (VMs), as
does the check that a member who still owns resources cannot be removed. Org deletion is also pending.

# Accounts: a new ownership and permission model

## Intent

observatory is moving towards independent services ([node control](2026-11-24-node-control-bus-design.md),
[telemetry](2026-11-24-telemetry-service-design.md)). A service must check permissions itself, without asking the hub
who owns a resource. Today that is impossible: resources belong to users or teams, and permissions come from rules the
hub generates per VM and disk from ownership (`PolicySnapshot` reads every VM and disk). This project replaces the
ownership and permission model so that:

- every resource belongs to an **account**, and its identifier names that account;
- a caller's rights travel in a token, so a check needs the token, the resource identifier and, for conditions, the
  resource's own attributes (which the owning service has anyway), nothing else;
- the model is rich enough for real use: custom roles with rules, sharing across accounts, conditions.

It comes before the telemetry service, which builds on it.

## Goals

- Org = administration only: members, accounts, roles, invitations, limits, audit. An org owns no resources.
- Accounts own every resource: VMs, disks, snapshots, images, env/secrets, integrations, dashboards, networks,
  devices, nodes.
- Resource identifiers carry the account: `obs:<service>:<account>:<type>/<id>`.
- Roles made of rules (allow/deny, actions, identifier patterns, conditions), built-in and custom, bound to users per
  account.
- Shares: an account attaches rules to its resources that grant other accounts access, with the same rule language.
- Conditions on request context and resource attributes, including tags on every resource.
- A signed token from the hub carries all rights of the caller; any service can decide alone.
- Existing installations migrate automatically; nothing is lost.

## Non-goals

- The SSH proxy (later project, user decision): all VM connections go through a proxy that records who connected
  when, and integrations are delivered per connected person (a placeholder per person and session, resolved by the
  egress proxy only while that person holds `integration:use`). It also has to settle per-person Unix users in the
  VM, since root in the VM can read other sessions' placeholders.

- Assuming roles / temporary credentials; identity stays OIDC sessions (plus today's node credentials and telemetry
  tokens).
- Org-wide guardrails over accounts and permission boundaries.
- Policy versioning or a JSON policy document language: rules stay rows like today's `role_rules`.
- Machine users with access keys.
- Shares across orgs (later, for shared platform nodes; the model must allow it).
- Splitting IAM out of the hub; the hub stays the owner of identities, orgs, accounts, roles, bindings, shares.

## Anti-goals

- Any permission check that has to look up who owns a resource.
- Rules generated from resources (today's owner rules).
- Two ownership kinds side by side (no org-owned or user-owned resources next to account-owned ones).

## Today

From `docs/permissions.md` and `internal/authz`:

- Rules: subject (`user:`, `team:`, `role:`) may/may not do action on object (`org/vms/*`, `org/vms/<id>`, …) within one
  org; deny wins; built-in roles member/admin/owner; custom roles with parents (depth 8); bindings of roles to users or
  teams, org-wide.
- Ownership: VMs and disks belong to a user or a team; env, integrations, images, dashboards to the org, a team or a
  user (`scope` column); nodes and networks to the org; devices to a user. Generated rules give owners and team admins
  their rights per VM/disk; `PolicySnapshot` (`internal/store/roles.go:19`) reads all VMs and disks for that.
- Privacy rule: org admins/owners never get `vm:connect`, `vm:fork` or `disk:use` on others' VMs and disks; the connect
  check admits only the owner or the owning team.
- Escalation rule: nobody grants (rules, parents, bindings) what they do not hold themselves.
- Personal orgs exist and are fixed single-member orgs.
- Limits per org (platform admin), per team and per member (org admin), stacked.
- Teams: members, team admins, own resources, carry bindings.

## Approaches considered

- **Account model (chosen).** Org manages, accounts own; identifiers carry the account; roles per account; shares across
  accounts. Mirrors the common cloud model (accounts/projects), removes ownership lookups, replaces teams.
- **Owner in the identifier** (`…:<user>:vm/<id>`, teams dropped). Rejected: a user is not a container; several people
  working on the same resources need an owner that is not a person.
- **Org owns everything, "mine" via tags and conditions.** Rejected: every per-person restriction becomes a condition on
  resource attributes, and there is no separation inside an org except by rules.
- **Keep today's model, services look owners up** (the first telemetry draft). Rejected by the user: a query per check
  or a cache per service.

## Design

### Orgs, accounts, users

- Signing up creates the user's personal org ("<name>'s Org") with one account `default`; the user is its owner.
  Personal orgs stay what they are today (user decision): single-member and fixed — no invitations, role changes,
  removal, leaving or renaming. The owner may create further accounts in it *(design call)*. Every existing user keeps
  or gets exactly one personal org.
- Working with others happens in a normal (shared) org: anyone may create one, as today; its admins invite people,
  create accounts and bind roles to members per account.
- A user may be a member of several orgs and hold roles in several accounts of each.
- Org-level roles stay (owner, admin, member) and only cover administration: invite/remove members, set org roles
  (`member:set-role`), update the org (`org:update`), create/rename/delete accounts, manage custom roles, bind roles,
  set limits, read the audit log. Telemetry tokens, the logs explorer and builder-VM listing (today org-level, partly
  outside the enforcer via `privileged(m.Role)` in `internal/api/builds.go:323`) become account-level actions.
- Org owners and admins are implicitly `admin` in every account of their org, including accounts created later; this
  is how an account gets its first admin *(design call)*.
- Invitations name the org role and optionally account bindings; without bindings the invitee gets `member` on
  `default` *(design call)*.
- Removing a member leaves the resources they created in their accounts; nothing blocks the removal.
- Account IDs are UUIDs, unique across the installation; an account has a name unique in its org. Resource names
  (VMs, disks, networks, nodes, dashboards) become unique per account instead of per org.
- A built-in account `platform`, owned by the installation (platform admins), holds the platform image (today
  `org_id IS NULL`, `internal/store/images.go:189`) and shares `images:image:use` with every account. Shared platform
  nodes will live there later *(design call)*.

### Identifiers

`obs:<service>:<account>:<type>/<id>`, e.g. `obs:compute:<acc>:vm/<id>`, `obs:compute:<acc>:disk/<id>`,
`obs:images:<acc>:image/<id>`, `obs:secrets:<acc>:env/<id>`. Sub-resources extend the path
(`obs:compute:<acc>:vm/<id>/snapshot/<id>`). Patterns use `*` (any run of characters within a segment) and `?`.
The service prefixes match the service names of the bus subjects. The exact type list is the builder's.

The API addresses resources under the account: `/api/accounts/{account}/vms/{id}` etc. Services always load a resource
by account **and** ID, so a request naming the wrong account finds nothing (`404`) — the account in the request is the
caller's claim, the lookup enforces it.

### Rules

A rule is: effect (allow/deny), actions (with `*` patterns, e.g. `compute:vm:*`), resource patterns, optional
conditions. Action names: `<service>:<resource>:<verb>` (e.g. `compute:vm:start`). The full catalog is derived from
today's catalog (`docs/permissions.md#catalog`) by the builder.

- **Roles** are named rule sets. Built-in roles per account (defined in code, immutable): `admin`, `member`, `viewer`
  (exact contents below). Custom roles belong to an org, can have parents, and are written with `${account}` in place
  of the account so one role can be bound in many accounts; a rule may also name a concrete account.
- **Bindings** attach a role to a user in one account. No team subjects.
- **Shares** belong to the owning account: rules with an additional list of grantee accounts. They grant the grantee
  accounts access to the owning account's resources matching the rule.
- Allow-wildcard expansion and depth limits as today.

### Evaluation

For a caller acting through account A on resource R in account B:

1. Any matching deny — in the caller's bindings in A, or in B's shares to A — denies.
2. If A = B: an allow in the caller's bindings in A is required.
3. If A ≠ B: an allow in the caller's bindings in A **and** an allow in a share of B to A are both required. The
   caller's account A is chosen by the request (API path or explicit parameter); a user in several accounts acts
   through one of them per request.
4. Otherwise denied.

Conditions must hold for a rule to match. The evaluator is one shared Go package used by the hub and every service.

### Conditions

Small fixed set *(design call; the builder may trim operators no built-in role or test needs)*:

- Keys: `obs:CurrentTime`, `obs:SourceIp`, `obs:SecureTransport`, `obs:UserId`, `obs:UserEmail`, `obs:CallerAccount`,
  `obs:ResourceAccount`, `obs:CreatedBy`, `obs:ResourceTag/<key>`, `obs:RequestTag/<key>`, `obs:TagKeys`.
- Operators: `StringEquals`, `StringNotEquals`, `StringLike`, `StringNotLike`, `DateLessThan`, `DateGreaterThan`,
  `IpAddress`, `NotIpAddress`, `Bool`, `Null`; `ForAnyValue` / `ForAllValues` for `obs:TagKeys`.
- Variables in values: `${obs:UserId}`, `${obs:UserEmail}`, `${account}`.
- Request keys come from the token: the hub, as the entry point, puts `SourceIp`, `SecureTransport` and the time it
  saw the request into the signed caller token, so a service never trusts a client-supplied value. Resource keys
  (`CreatedBy`, `ResourceTag/*`, `ResourceAccount`) come from the resource the service loaded.
- `obs:CreatedBy` is an immutable attribute every resource gets at creation (the creating user, or empty for system
  resources); it is how "own resources" are expressed without user ownership.

### Tags

Every account-owned resource has tags: up to 50 key/value pairs, keys `^[a-zA-Z0-9_.:/=+-@]{1,128}$`, values up to 256
characters, keys starting with `obs:` reserved. Set on create (`tags` field) or later; actions
`<service>:<resource>:tag` / `untag`. `RequestTag` and `TagKeys` conditions make tags enforceable on create
(e.g. deny `compute:vm:create` unless `obs:RequestTag/cost-center` is set). Tags are shown and edited in the GUI and
CLI, and filterable in lists.

### Built-in roles

Today's privacy rule (admins never connect to, fork or read the disks of others' VMs) is dropped (user decision): an
account is a shared space, and what its roles allow applies to every resource in it. Separation means a separate
account. `obs:CreatedBy` stays available as a condition key for custom roles.

- `viewer`: read everything in the account.
- `member`: everything on the account's VMs, disks, networks, images, env, integrations and dashboards (create, use,
  power, connect, fork, `disk:use`, snapshot, restore, delete, tags); read the account's logs; read and use nodes
  (place VMs on them).
- `admin`: `member` plus node management (create, rename, delete, node metrics), telemetry tokens, and account
  administration: bindings, shares, deny rules, account limits (user decision: nodes and telemetry tokens stay with
  admins, logs go to members).

The identity side of `viewer` and `member` names every account (`obs:*:*:*`): a resource of another account is still
reachable only where that account shares it, but a share must not need a custom role on the receiving side.

### Escalation rule

Kept: nobody may write a role rule, bind a role or create a share granting an action on a resource pattern they do
not hold themselves (and are not denied). Only org owners/admins and account admins write deny rules.
"Holds" is decided conservatively *(design call)*: the granter needs an unconditional allow whose pattern covers the
granted pattern; conditional allows never count towards granting. Pattern coverage is
checked syntactically (the granted pattern must be matched by the held pattern treated as a glob over globs; when in
doubt, refuse). Org owners are exempt, as today.

### Other callers

All go through the same evaluator with rights resolved by the hub *(design call)*:

- Telemetry tokens belong to an account and carry a fixed rule set; the logs explorer is per account.
- Node credentials identify a node, which belongs to an account; nodes act as themselves, not as users.
- Registry clients: user pull/push tokens are per account; builds act under a `build:<id>` principal with the rights of
  the build's account. Image recipe references (`refAllowed`, `internal/store/images.go:346`) become identifiers and
  need `images:image:use` (own account or via share).
- Devices belong to a user in an org (as today); the WireGuard allow-list admits a device to a VM in any account of
  that org where the device's user holds `compute:vm:connect`, evaluated by the hub when it builds the network state.
- The image storage limit (`image_storage_gb`) exists per org and per account.
- Audit rows keep their format; old rows with `org/...` objects stay readable, new rows carry identifiers.

### Token

The hub issues a signed caller token per request it forwards (Ed25519, 60 s): user ID and email, org roles, per
account the resolved rules of all bindings (incl. parents), the shares granting any of those accounts, and the request
context keys. Size is bounded by rules, not resources. A changed binding, role or share applies within 60 s (accepted
by the user). The hub itself checks with the same package and the same data.

### Limits

Per org (platform admin) and per account (org admin), stacked like today's org/team limits. Per-member limits are
dropped *(design call)*.

### Migration (automatic, one release)

- Every org gets an account `default`. User-owned VMs, disks, images, env, integrations, dashboards, devices go there;
  `CreatedBy` = the former owner. Org-scoped env, integrations, images, dashboards, nodes and networks go there too.
- There is nothing private inside an account (user decision): today's user-scoped env/secrets, integrations, images
  and dashboards become ordinary resources of `default`, visible and usable by everyone whose role allows it; a VM gets
  the env and integrations of its account. Clashes *(design call)*:
  - org-scoped vs. user-scoped item with the same name/kind: today's precedence of the VM owner's own scope over the
    org scope (`ResolveVMEnv`, `internal/store/env.go:352`; the builder verifies the exact order) decides which keeps
    the name, so a single-member org keeps exactly what its VMs see today;
  - items of different users with the same name/kind (only in orgs with several members): the org-scoped one, or else
    the first by creation time, keeps the name; the others are renamed `<name>-<user handle>`;
  - per-scope image defaults: the org default wins; others are dropped;
  - a migration report lists every rename and every VM whose effective env or integrations change; the release notes
    say that formerly private items of shared orgs are now visible to the account.
  - Subscriptions must not be shared (user requirement: a Claude subscription is one person's): formerly user-scoped
    `claude` and `gcloud` integrations get, in their new account, a deny rule on `integration:use` for everyone except
    the former owner (`obs:UserId` condition). Plain rules, no "personal" concept. Note that whoever controls a VM
    can use what the VM carries; per-person delivery is the SSH proxy project below.
- Every team becomes an account named like the team; its resources move there; `CreatedBy` = the former team-owned
  resource's creator if known, else empty.
- Bindings: org members → `member` on `default`; org admins/owners are implicitly `admin` everywhere; team members →
  `member` on the team's account, team admins → `admin` there. Everyone keeps at least today's rights; members of
  `default` and of team accounts gain full access to each other's resources there (consequence of dropping the privacy
  rule; listed in the release notes). Custom roles are rewritten from `org/...` objects to identifier patterns
  with `${account}`; team-subject bindings become per-user bindings in the team's account.
- Shares from `default` to every migrated team account: `node:use` on its nodes, `network:use` on its networks,
  `images:image:use` on its images, `env:use` / `integration:use` on the formerly org-scoped env and integrations
  *(design call: team VMs keep running where they run, with what they have)*. VMs may sit on a network of another
  account when a share allows it.
- Dashboard "creator may edit" (checked in the API today) becomes a `CreatedBy` rule in the built-in roles.
- Personal orgs stay personal orgs and get their account `default` like every other org.
- Team-level and member-level limits: team limits become that account's limits; member limits are dropped (listed in
  the release notes).
- The migration is checked by a decision comparison: for every user and every existing VM/disk/… the old enforcer's
  and the new evaluator's allow/deny for every action are compared on a copy of prod data before release; differences
  must be explained or fixed.

### API, GUI, CLI

- Routes move from `/api/orgs/{org}/<resource>` to `/api/accounts/{account}/<resource>`; org routes keep only
  administration. Old routes are removed in the same release (hard cut, GUI and CLI ship with the hub).
- GUI: org switcher plus account switcher; account pages (members/bindings, shares, limits); role editor with
  conditions; tags on every resource. Team pages are removed.
- CLI: a current account in the config (`observatory account use <name>`), `--account` per command.

## Delivery

On `main`, each step releasable *(order proposed; builder may adjust)*:

1. Evaluator package with rules, patterns, conditions, shares, tests.
2. Accounts and `account_id` / `created_by` / tags columns, filled by migration; old model still in charge; new
   decisions computed in shadow mode and differences logged.
3. Switch: new routes, token, GUI, CLI, teams removed; old authz deleted (`internal/authz` and the Casbin dependency; `internal/policy` is the only evaluator).

Step 3 is necessarily large (routes, GUI, CLI change together); it is the one hard cut.

## Success criteria

- Decision comparison on a copy of prod data shows no unexplained differences.
- New signup gets the personal org "<name>'s Org" with `default` and can create a VM immediately; inviting into it is refused.
- An org admin invites a user, binds `member` on account `ml`; the user creates a VM there and sees nothing in
  `default`.
- Share: account `infra` shares `compute:node:use` on its nodes with `ml`; a member of `ml` places a VM on an `infra`
  node; without the share it is denied.
- Condition: a role allowing `compute:vm:*` only with `obs:SourceIp` in a range denies from outside it; a deny
  requiring `obs:RequestTag/cost-center` blocks untagged creates.
- Removing a binding takes effect within 60 s.

## Left to the builder

- Exact action catalog and resource types (derive from `internal/authz/catalog.go`, which has actions `docs/permissions.md` lacks, e.g. `image:*`), built-in role contents in detail (following the above).
- Token encoding and claim layout; caching of resolved rights per session.
- Schema for accounts, bindings, shares, tags; migration SQL.
- GUI layout of the account switcher, role editor and share editor (follow the existing UI components).

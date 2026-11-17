# observatory: identity, tenancy and permissions foundation

## Intent

observatory is to become our VM platform: agents work inside microVMs, each with its own "PC". A proof of concept exists (vm-manager, `SpielerNogard/vm-manager`: Go hub, nodes on vz/Firecracker/cloud-hypervisor, Vue + daisyUI GUI). It has no users and no permissions: the GUI's REST API is open, and one shared token authenticates nodes, gateway and storage. All resources are global.

Before VMs move over, observatory needs the foundation the PoC lacks: who someone is, which tenant they belong to, and what they may do. This first project builds that foundation plus a new UI (Vue 3 + lumen). The PoC UI and VM features are deliberately left out; they come next and plug into this.

Success: a person can register, lands in their own org, invites others, org and platform admins manage members, roles, permissions and limits in the new UI, every change is audited, and the permission engine already answers "may principal P do action A on resource R" for resource types that don't exist yet.

## Goals

- Login in the browser and from a CLI (device flow).
- Organizations and users, with personal orgs, membership, invitations.
- A role and permission engine with per-resource rules, wildcards, allow/deny with deny winning, and role inheritance.
- Ownership: resources belong to a user or to an org.
- Sharing of resources with users or teams of the same org.
- Teams as user groups and cost centers; limits (quotas) per org, team and user, configurable.
- An audit log of every action.
- UI views for every user, org admins and platform admins.

## Non-goals (this project)

- VMs, disks, networks, backups and the rest of the PoC; also porting the PoC UI.
- SSH access (comes with VMs, see "Later").
- Enforcing limits (nothing consumes resources yet).
- The concrete permission catalog per resource type (comes with each type).
- Per-org SSO / bring-your-own IdP (the design keeps it possible, nothing more).
- Session recording.

## Anti-goals

- An agent or user able to grant more rights than they hold (privilege escalation).
- Resources visible or shareable across org boundaries.
- Any action that changes state without an audit entry.
- Agents logging in with a human's credentials.

## Decisions

### Tenancy and ownership (decided by the user)

- **Organization (org)** is the tenant. A user always belongs to at least one org. On first login, observatory creates a **personal org** for the user, who is its admin.
- The **creator of an org is its owner**. An org can have **several owners**; owners can make members admins or owners.
- Every resource has a type and an ID and belongs to **either a user or an org**, depending on the context it was created in.
  - User-owned: only the owner decides what happens to it (the owner may always do everything).
  - Org-owned: all members of the org have access (as their org role grants).
  - User-owned resources still live in the org they were created in. Org admins can see, delete and take them over, but not look inside (no console). **Org deny rules apply to owners too**, so "the owner may do everything" is an allow that a deny can still override.
- **Org guardrails:** an org can forbid creating resources with certain properties, e.g. VMs without an expiry. These are conditions on resource attributes, not plain action denies (see "Later").
- **Platform admins** only administer (orgs, users, limits, audit). They have no access to tenants' resources.
- **Teams** group users and are also **cost centers with limits** (billing, quotas). Teams are optional; an org may have none.
- **Cost center per resource:** at creation the creator may book the resource onto one of their teams. Without a choice, the cost center is the creator themself (no default team). Only teams the creator belongs to are selectable. Leaving a team doesn't rebook existing resources; someone with the right to do so rebooks them.
- **Limits stack:** a new resource must fit the creator's membership limit, the chosen team's limit (if any) and the org's limit. If any is exhausted, creation is refused. In this project limits are configurable only, not enforced.
- Invoicing itself is out of scope; this project only stores the cost center model and limits.
- Resources can be **shared with individual users or teams of the same org**. Never across orgs.
- **Personal credentials must never be readable by others on a shared resource** (cf. boxd, which wipes agent logins when a machine is shared). Enforced with VMs; recorded here as a rule.
- **Limits** exist per org, per team and per user (membership).
- **Each VM gets its own account** (a non-human principal) whose permissions are configurable at creation. Not implemented in this project (no VMs), but the principal model must accommodate it: principals are users or VM accounts.

### Permissions (decided by the user)

- Permissions depend on the resource type (e.g. `vm:console`, `vm:delete`, `vm:create`); the catalog comes later.
- Permissions are grouped into **roles**; roles can contain other roles (sub-roles).
- Rules apply **per type and per individual resource**, support **wildcards**, and have **allow and deny** effects; deny wins, and no allow means denied.
- Engine: **Casbin** (Go). The user chose it over a hand-written evaluator and Cedar. AWS IAM was the reference for features, not for syntax.
- Built-in roles: org owner, org admin, org member, plus platform admin. Orgs can define custom roles.

### Audit (decided by the user: every action is auditable)

- Each entry: principal (user or VM account), org, action, resource, time, source (IP/client), result (allowed, denied, failed).
- **Denied attempts are logged.**
- Append-only; nobody, including admins, can change or delete entries through the API.
- Org admins read their org's log; platform admins read everything.
- My call: plain reads (lists, details) are not logged; sensitive reads (tokens, credentials, configs) are.

### Authentication

- **The hub is an OIDC relying party; the IdP is external and pluggable.** observatory does not ship an IdP in its stack, so operators can use Zitadel, Keycloak or any other OIDC provider. Zitadel is the reference IdP used in development (chosen over Better Auth, which is a TypeScript library needing a second runtime with its own org model, and over building auth into the hub).
- **IdP requirements:** OIDC discovery, authorization code flow with PKCE for a public web client, device authorization grant (RFC 8628) for a public CLI client, and `sub`, `email`, `name` available from the ID token or the userinfo endpoint. Registration via `prompt=create` is used when the IdP supports it; otherwise registration is up to the IdP's login page.
- The IdP only answers *who* someone is. Orgs, membership, roles, permissions and audit live in observatory.
- Browser: OIDC authorization code + PKCE; the hub then sets its own session cookie.
- CLI: OIDC device authorization flow (`observatory login`), `observatory whoami`. My call: only these two commands in this project.
- Registration and passkeys/2FA happen in the IdP's login UI.
- VM accounts will get hub-issued tokens, never IdP accounts.
- Development: Zitadel in its own, separate Compose project (not part of observatory's stack) at `localhost:8080`, `ExternalSecure: false`. The hub must reach the IdP under the same issuer URL as the browser (fine with the hub on the host; use `127.0.0.1.sslip.io` if the hub runs in a container). Production: own domain with HTTPS, config change only.

### Other calls (mine)

- **Owner role** (after boxd, but several owners allowed): only owners promote to or demote from admin and owner, so admins can't lock each other out. The last owner can't be demoted, removed or leave.
- **Teams** (after boxd): groups of org members, created by org admins; team admins manage their team's membership. A team is a Casbin subject: roles and shares can target it.
- **Leaving an org** (after boxd): a member can't be removed while they still own resources in that org; they must be transferred or deleted first.
- **Platform admin bootstrap:** the first user to log in to an installation becomes platform admin.
- **Invitations:** org admin creates an invite link (expiring, single use) and hands it over themselves; no mail sending in observatory.
- **Database:** Postgres, observatory's own (the IdP brings its own storage). Replaces the PoC's JSON files.
- **Code:** a new Go hub in this repo; the PoC is ported later, not copied now. The UI is built with Vite and embedded in the Go binary as in the PoC (`web/embed.go`).
- **UI stack:** Vue 3, vue-router, vue-i18n (de + en as in the PoC), `@asterism-dev/lumen` (installed from GitHub tag), no Tailwind/daisyUI. Theme toggle via lumen's `data-theme`.
- **Limits fields:** vCPU, RAM, disk GB, VM count.
- **Doc language:** English, like the PoC docs.
- **Built-in roles** are defined by the platform and immutable; orgs build custom roles that may inherit them. New catalog permissions then reach every org without per-org migrations.
- **A share** binds one role to one principal on exactly one resource. Sharing requires `<type>:share` on that resource; the owner always holds it.
- **Escalation guard:** resource patterns in rules are restricted to three forms: everything of an owner (`<owner>/*`), all of a type (`<owner>/<type>/*`), one resource (`<owner>/<type>/<id>`). Action globs are expanded against the catalog. Coverage is then a prefix check plus a set check, no general glob-subsumption. Only org admins may write deny rules.
- **User limits** are per membership (user × org).
- **CLI credential:** after the device flow the CLI exchanges the IdP's tokens for a hub-issued token, so CLI sessions can be listed and revoked in the profile. VM accounts later use the same token mechanism.

## Approaches considered

| Area | Options | Picked | Why |
|---|---|---|---|
| Authorization engine | Hand-written AWS-IAM-style evaluator; **Casbin**; Cedar (`cedar-go`); SpiceDB/OpenFGA | Casbin | User's choice: mature, role inheritance built in, deny-override, glob/key matching, multi-tenant domains. Cedar-go lacks templates and full schema validation; Zanzibar systems are a separate service without wildcard/deny semantics. |
| Identity | **External OIDC IdP (Zitadel as reference)**; Better Auth sidecar; auth inside the hub | External OIDC IdP | Operators pick their IdP (Zitadel, Keycloak, ...); Zitadel for dev: Go, self-hostable, device flow, passkeys. Hub stays IdP-agnostic, which also keeps per-org SSO possible later. |

## Authorization model

Sketch; the builder may adjust names but not semantics.

- **Subject:** `user:<id>` or `vm:<id>` (VM accounts later); roles are subjects too (`role:<id>`).
- **Domain:** the org ID. Platform-wide rules use a reserved domain (e.g. `*` or `platform`).
- **Object path:** encodes owner, type and ID so one pattern can target a type, a single resource, or everything an owner has:
  - org-owned: `org/<type>/<id>` (e.g. `org/vm/7f3a`)
  - user-owned: `user:<uid>/<type>/<id>`
  - org-level objects such as membership, roles, limits, audit: e.g. `org/members`, `org/roles/<id>`.
- **Action:** `<type>:<verb>`, glob-matched (`vm:*`, `*`).
- **Effect:** allow or deny; policy effect "some allow and no deny".
- **Role inheritance:** `g` rules, scoped per domain (`g, user:u, role:member, org1`; `g, role:operator, role:viewer, org1`).

Starting model (Casbin conf, to be verified against the Casbin version used):

```
[request_definition]
r = sub, dom, obj, act
[policy_definition]
p = sub, dom, obj, act, eft
[role_definition]
g = _, _, _
[policy_effect]
e = some(where (p.eft == allow)) && !some(where (p.eft == deny))
[matchers]
m = g(r.sub, p.sub, r.dom) && r.dom == p.dom && globMatch(r.obj, p.obj) && globMatch(r.act, p.act)
```

How the user's rules map:

- Owner may do everything: on personal ownership, `p, user:u, org1, user:u/*, *, allow`.
- Org members access org resources: role `member` has allow rules on `org/*` (exact actions from the catalog later).
- Sharing: a role bound to a user for one object: either a `p` row for that user on that object, or a per-resource grant row the hub expands. Only within the same domain (org).
- Org admin: role `admin` inherits `member` and adds org management actions.
- Platform admin: rules in the platform domain cover only administrative objects (orgs, users, limits, audit). The hub never grants tenant resource actions from the platform domain.
- **Escalation guard:** before creating/altering a role, binding or share, the hub checks that the acting principal holds every permission being granted on the target objects. The engine alone doesn't do this; the hub must.

Policy changes go through the hub API, are stored via a Casbin Postgres adapter, and are audited. Multiple hub instances are not in scope; the enforcer can stay in-process (use the synced/thread-safe enforcer).

## UI

```
Public          Login · Register (→ IdP) · Accept invitation
Every user      Org switcher · Profile (name, sessions incl. CLI)
Org admin       Org settings · Members (invite, remove, role) · Teams (members, team admins, limits) · Roles (custom,
                inheritance, allow/deny rules) · Limits per user and team · Shares · Org audit log
Platform admin  All orgs · All users · Org limits · Platform admins · Global audit log
```

The UI only shows actions the current principal is permitted to do (the hub exposes an "allowed actions" check); the hub enforces regardless.

## Later (designed for, not built)

- VM accounts with hub-issued tokens and creation-time role sets.
- SSH: the hub as SSH certificate authority; `observatory ssh <vm>` checks `vm:ssh`, audits, issues a short-lived certificate; VMs trust only that CA.
- Limit enforcement against consumption; the creator's (owner's) quota is charged, also for VMs created by VM accounts.
- Splitting the PoC's shared token into per-node machine credentials.
- Per-org SSO.
- **Org guardrails** (e.g. "VMs must expire"): typed org settings checked by the hub at create/update, rather than generic Casbin conditions. Only org admins edit them; changes are audited.
- **Secrets** as a resource type (after boxd env-secrets): owned by user or org, write-only (no action ever returns the value), used by reference so rotation reaches all VMs, optionally host-bound (VM sees a placeholder; the real value is substituted only in HTTPS to bound hosts). Permissions like `secret:set`, `secret:use`, `secret:delete`, no `secret:read`.
- **VM-to-VM** (after boxd vm-to-vm): a VM authenticates automatically by its own identity (the VM account), every call is checked. Reachability between VMs is a network concern (networks as segmentation, isolated VMs without account), not an IAM one.
- **Default rights of a VM account:** proposed to inherit the owner's rights at creation (boxd: "acts as its owner, fenced to its org"), optionally restricted by a role set; never more than the creator holds. In Casbin this is a `g, vm:<id>, user:<owner>, <org>` rule, or explicit role bindings when restricted.

## Left to the builder

- Go module layout, HTTP router, migrations tool, Casbin adapter choice.
- Exact API shape (REST), pagination, error format.
- Session storage and cookie details (secure defaults).
- UI layout within lumen's components; i18n keys.
- How append-only is enforced (DB grants/trigger vs. app-only).
- Test strategy, with heavy table tests on the authorization model (wildcards, deny precedence, inheritance, cross-org isolation, escalation guard).

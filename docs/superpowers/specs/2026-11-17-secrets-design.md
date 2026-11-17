# Environment variables and secrets

## Intent

Lay the foundation for environment variables and secrets now so the VM port, which comes right after, only has to ask "which variables does VM X get?". Variables exist on three levels (org, team, user), are stored encrypted with a key per org, and are governed by the existing permission system (Casbin catalog, custom roles, deny rules, audit).

## Goals

- One resource, **variable**, of two kinds: **env** (plain, value readable) and **secret** (sealed, write-only).
- Three scopes: **org**, **team**, **user** (private). Every variable belongs to exactly one org.
- Secrets: no API, UI or log ever returns the value after it is set. Updating replaces the value.
- Encryption of every value (env and secret) with a key per org, which is itself encrypted with a master key.
- Full permission model: catalog actions, built-in role defaults, custom roles and deny rules work on variables like on any other object.
- Every change is audited (name, kind, scope, owner, actor), never a value.
- A resolver that returns the decrypted variables for an owner (user or team in an org), filtered by the owner's `env:use`, ready for the VM port. It takes the enforcer. No HTTP endpoint exposes it.
- An **Environment** page in the main navigation.

## Non-goals

- VMs, injecting variables into VMs, the connect check (see "Rules for the VM port").
- Restricting a variable to individual VMs (vm1 yes, vm2 no). Later; then as a visibility setting on the variable, not via Casbin rules.
- Host-bound secrets (placeholder in the VM, real value substituted by an egress proxy) and the "Hosts" column. Come with the egress proxy after the VMs.
- External KMS / Vault as master key source, bring-your-own-key per org.
- Rotation tooling for master key and org keys (the data model allows it, see Encryption).

## Anti-goals

- A secret value that leaks through an API response, audit row, log line, error message or DB query text.
- Any value readable with the DB dump alone (without the master key).
- A second permission system next to Casbin.

## Approaches

- **A) Envelope encryption in the hub, stored in Postgres (picked).** Go standard library only, no new service, same transaction as the audit row. Master key comes from the hub's environment; an external KMS can later replace that source without a data migration.
- B) Vault/OpenBao from day one. Mature, but one more service in the stack (against the line of keeping external systems pluggable instead of bundled), and permissions plus audit would live twice.
- C) pgcrypto. Least code, but the key travels to the DB with every query and may end up in logs and `pg_stat_statements`.

## Which variables a VM gets

Decided: the VM's owner determines its variables. A VM belongs to a user or a team.

- **Team VM:** org variables + variables of that team.
- **User VM:** org variables + that user's private variables (in that org).
- Same name on two scopes: the more specific one (team or user) wins over org.
- A user VM gets no team variables, even if the user is in teams.
- Every VM gets all variables of its chain that its owner may use (`env:use`); no per-VM selection in this version. With the default roles that is the whole chain; an org can withhold variables with deny rules (e.g. `deny env:use@org/env` for a contractor role).
- For a team VM, the team subject is the owner whose `env:use` is checked.

## Rules for the VM port

Recorded here, built with the VMs:

- **Private secrets must not be in a VM while another user is connected.** Inside a VM, anyone connected can read process environments, files and memory, and a value once delivered cannot be withdrawn. So a VM with private secrets admits only its owner (shares, SSH, exec by others are rejected); a VM that is shared gets no private secrets. Later, host-bound secrets (placeholder only) may relax this.
- Variables are resolved by reference at delivery, so changes reach every VM on its next delivery.

## Data model

- `org_keys`: one row per org: the org key (32 random bytes) encrypted with the master key, plus the id of the master key used.
- `env_vars`: `id`, `org_id`, `kind` (`env` | `secret`), scope (`org` | `team` | `user`), `team_id` or `user_id` (exactly one for team/user, none for org), `name`, `ciphertext`, `nonce`, `created_by`, `updated_by`, `created_at`, `updated_at`. Unique on (org, scope, owner, name).
- Names are environment-variable names: `^[A-Z_][A-Z0-9_]*$`. *(Builder's call: max length and value size limit; 64 KiB proposed.)*
- Deleting a team deletes its variables. Removing a member from an org deletes their private variables in that org. *(Our call; private variables belong to the person's membership.)*
- Private variables live inside one org. A user who wants the same variable in two orgs sets it twice; this keeps the org key as the only key for everything in an org.

## Encryption

```
Master key  (OBSERVATORY_SECRETS_KEY, 32 bytes base64; later optionally an external KMS)
  └─ org key  (one per org, stored encrypted with the master key in org_keys)
       └─ variable value  (AES-256-GCM with the org key; env and secret alike)
```

- AES-256-GCM with a random nonce per encryption, also for wrapping the org key.
- Additional authenticated data binds each ciphertext to its row: the org id for the org key; org id and variable id for a value. A ciphertext copied into another row or org fails to decrypt. Rename and move keep the id, so they need no re-encryption.
- The org key is created on first use, in the same transaction as the first variable.
- The hub refuses to start when `OBSERVATORY_SECRETS_KEY` is missing or not 32 bytes.
- Rotation is possible by design, not built: master key → rewrap all org keys (values untouched); org key → re-encrypt that org's values. Storing the master key id per org key makes a rolling master rotation possible.
- Deleting an org's key makes all its values unreadable, including in backups.

## Permissions

New catalog entries:

| Action | Object |
|---|---|
| `env:list`, `env:set`, `env:delete`, `env:use` | `org/env` (org variables) |
| `env:list`, `env:set`, `env:delete`, `env:use` | `org/team-env/<team-id>` (variables of one team) |
| `env:list`, `env:set`, `env:delete`, `env:use` | `org/user-env/<user-id>` (private variables of one user in this org) |

- `env:list` returns names and metadata of every variable, and the value of `env` variables. It never returns a secret value; there is no action that does.
- `env:use` means "VMs I own receive these values". It is checked by the resolver, not by an API endpoint.
- All three use the existing pattern grammar: `org/team-env/*` means every team, `org/user-env/*` every user, `<id>` one of them. This lets an org write org-wide rules such as `deny env:set@org/user-env/*` ("no private variables here").
- Private variables are org-scoped objects on purpose (not `user:<id>/...`): the `user:` scope cannot be matched by org-wide rules. Each member gets the four actions on their own `org/user-env/<user-id>` through a generated rule, like team admins get `team:manage-members` today. Nobody else sees or manages another user's private variables.
- `env:use` goes through the escalation rule like every other action.

Defaults:

| | member | team admin (of that team) | admin | owner |
|---|---|---|---|---|
| list and use org variables | yes | | yes | yes |
| set/delete org variables | | | yes | yes |
| list variables of own teams | yes | yes | yes (all teams) | yes (all teams) |
| use variables of a team (team VMs) | team subject | team subject | team subject | team subject |
| set/delete team variables | | yes | yes (all teams) | yes (all teams) |
| own private variables (all four actions) | yes | yes | yes | yes |

- Team admins get their team rights the same way they get `team:manage-members` today (generated policy per team admin). Team members get `env:list` on their team through the team subject.
- Each team subject gets `env:use` on `org/env` and on its own `org/team-env/<id>` (generated), so team VMs receive org + team variables by default.

## Operations

- **List** (all scopes the caller may `env:list`), **add**, **edit** (name and value; for a secret the value field starts empty and an empty value keeps the old one), **move** (change scope; needs `env:set` on the target and `env:delete` on the source), **delete**.
- Kind: env → secret is allowed on edit; secret → env is not (it would reveal the value). *(Our call.)*
- Audit actions `env.set`, `env.move`, `env.delete` in the same transaction as the change; details: name, kind, scope, owner id, created vs. replaced. Denied attempts are audited like every other denial.
- Request bodies containing values must not be logged anywhere. *(Builder: check existing request logging.)*

## UI

**Environment** in the main navigation next to Dashboard, Nodes, VMs, at `/orgs/:orgId/env`.

```
Environment                                         [Documentation] [+ Add]
Variables and secrets injected into your VMs.

 All   Org   Teams   Private
NAME               TYPE     SCOPE          VALUE            
ANTHROPIC_API_KEY  secret   org            ••••••           Edit  Move  Delete
DATABASE_URL       secret   team Backend   ••••••           Edit  Move  Delete
LOG_LEVEL          env      team Backend   debug  ⧉         Edit  Move  Delete
GH_TOKEN           secret   private        ••••••           Edit  Move  Delete
```

- Tabs filter by scope; the Teams tab shows all teams the caller can see, the scope column names the team.
- Add dialog: Name, Value, Scope (Org · each team the caller may `env:set` on · Private), checkbox "Make this a secret (sealed, write-only)". Only scopes the caller may write are offered.
- `env` values are shown with a copy button; secret values never.
- Edit, Move, Delete shown only when permitted (`can()` as today). Move offers only target scopes the caller may write.
- The Documentation button links to a short page on how variables reach VMs. *(Builder's call whether this is in-app or docs/; may be deferred until VMs exist.)*

## Left to the builder

- Exact REST paths and payloads, pagination.
- UI details within the existing components; i18n keys (de + en).
- Value size limit and name length.
- Shape of the resolver function and where it lives (store vs. own package), as long as it is not reachable over HTTP.
- Test strategy; must include: wrong AAD/row swap fails, missing/short master key refuses start, no secret value in any response/audit row, secret → env refused, permission table above incl. deny and cross-org isolation, resolver drops variables the owner may not `env:use`.
- Update docs/permissions.md (catalog, defaults, the `env:use` meaning).

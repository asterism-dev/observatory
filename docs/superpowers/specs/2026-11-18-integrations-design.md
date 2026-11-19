# Integrations (GitHub, Claude, Linear)

## Intent

Project 5 of the VM port (roadmap in `2026-11-17-vms-nodes-design.md`; projects 1–4 and 6 are built). Today a member who wants `git`, `gh`, Claude Code or the Linear MCP server in a VM has to put a token into a variable by hand, know which environment variable or file each tool reads, and bind the right hosts. This project makes these logins a resource of their own: a member connects GitHub, Claude or Linear once, on the org, a team or privately, and every VM they are entitled to has working `git`/`gh`, Claude Code and Linear MCP without any setup inside the guest. By default the VM holds only placeholders and the egress proxy (project 4) puts the real token in; per integration the owner can choose to deliver the real token into the VM instead, as the PoC does.

Success: a member connects GitHub with a PAT (verified, account `octocat` shown) and Claude via sign-in; in a fresh user VM `gh api user` prints `octocat`, `git clone git@github.com:<org>/<private>.git` works through HTTPS, `claude -p "say ok"` answers, `claude mcp list` shows `linear` connected, and nothing in the guest (env, files, memory, a snapshot) contains a real token. Eight hours later Claude still works although the access token was rotated by the hub. The member switches Linear to "inside the VM" and within five minutes `LINEAR_API_KEY` holds the real key in new shells. They disconnect GitHub and `gh api user` fails on the next request. A team VM never gets a private integration; a node of another org never receives a token.

## Decided with the user

- Integrations are **their own resource type** (not a variant of variables), with the scopes **org / team / private** and their own `integration:*` permissions.
- **Kinds in this project:** GitHub (personal access token), Claude (`claude setup-token` or OAuth sign-in), Linear (API key). A GitHub App with short-lived installation tokens is a later option.
- **Both delivery modes**, selectable per integration: (a) **inside the VM** — the real token reaches the guest, as in the PoC (token service, git credential helper + `insteadOf`, `gh` wrapper, Claude credentials file, Linear MCP); (b) **proxy** — the guest gets a placeholder and the egress proxy substitutes it. **Default: proxy.**
- **From the reference design:** the Claude credentials file holds **only an access token** (refresh token stays on the hub; the file is rewritten periodically); `git` via `insteadOf` + credential helper + `gh` wrapper; a **token service authenticated per VM**, not only by source IP; Linear token for API and MCP.
- **Nodes are untrusted across orgs:** token values reach only the node that runs the VM.
- Existing rules apply unchanged: a user VM gets its owner's private scope, a team VM never a private one; the connect check is not relaxed (egress spec).

Everything else is marked *(Our call.)*

## Goals

- **Integration resource** with kind, source, scope, delivery mode, verified account, sealed token; at most one integration per kind and scope.
- **Resolution per VM** like variables: org + own scope (user VM: owner's private; team VM: the team's), narrowest scope wins per kind, gated by `integration:use`.
- **Proxy mode** feeds the existing hook: resolved proxy-mode integrations become `HostBoundSecret` entries next to the host-bound variables in `boot()` (`internal/fleet/fleet.go`); node and proxy need no change.
- **Token service on the node, over vsock**: the guest asks for "the GitHub token" and gets the placeholder (proxy mode) or the value (inside-VM mode). VM identity comes from the vsock channel itself.
- **Guest glue** shipped with the seed: `vmm-token`, git credential helper and gitconfig, `gh` wrapper, Claude credentials sync, Linear MCP and `LINEAR_API_KEY`.
- **Claude OAuth refresh on the hub**; the new access token reaches running VMs without a reboot.
- **Immediate effect** of connect, replace, mode switch and disconnect: next request (proxy mode) or next sync, at most 5 minutes (inside-VM mode).
- Permissions, audit, Integrations page, an Integrations line on the VM page; de + en.

## Non-goals

- GitHub App / short-lived GitHub tokens, OAuth sign-in for GitHub or Linear (paste only), GitHub Enterprise Server, `ghcr.io`/`docker login`.
- Other providers (Codex/OpenAI, Slack, Figma, …) and any generic provider or "toolkit" catalog *(Our call: three kinds as code; a fourth is a table entry plus guest glue)*.
- **Per-VM selection** of integrations: a VM gets every integration its owner is entitled to, like variables *(Our call: no new VM action or setting; add an opt-out when someone needs a VM without GitHub)*.
- Moving an integration between scopes (disconnect + connect).
- Installing `gh`, Claude Code or other tooling in the guest, and the agent context file: project 7. This project writes configuration that takes effect once the tool exists.
- Integrations inside containers in the VM (no credential helper, no trust; documented).
- An org setting "proxy mode only" *(Our call: add when an org asks; the mode is shown and audited)*.

## Anti-goals

- A Claude refresh token anywhere outside the hub's database (no node, no VM, no API response, no log).
- A proxy-mode token value in the guest (env, seed, files, memory, snapshots) — same guarantee as host-bound variables.
- Any token value in a log line, audit row, status message, `vm.json`, the seed or an API response.
- A private integration in a team VM or in another user's VM.
- A VM obtaining another VM's tokens from a token service, by any address or socket.
- A token reaching a node that does not run a VM entitled to it, in particular a node of another org.
- Overwriting what the user set up themselves: their own `claude /login`, their own git credential helpers, an existing `LINEAR_API_KEY` or `GH_TOKEN`.

## Approaches

- **A) Token service on the node over vsock, guest helpers pull, proxy mode via host-bound secrets (picked).** Firecracker maps a guest connection to host CID 2, port P onto the Unix socket `<vsock uds>_P` inside that VM's jail, so the socket a request arrives on *is* the VM's identity — nothing to spoof, no key to hand out, unreachable from other VMs and from the network. One guest code path serves both modes (the service returns placeholder or value), so a mode switch needs no reboot.
- B) PoC style: HTTP token service on the NAT gateway address, VM identified by source IP, plus a per-VM key in the seed. Needs per-tap filtering to stop IP spoofing on the bridge, and the key is exactly as exposed in the guest as the token it protects.
- C) Push values into guest files through the guest agent. No polling, but puts secrets on the host→guest agent channel (excluded by the lifecycle spec), works only while the agent is up, and leaves files behind on removal.
- D) Seed only (like variables). No refresh and no removal without a reboot; unusable for Claude OAuth.

## Integrations

- **Kinds** *(Our call)*:

| Kind | Sources | Verified with | Account shown | Bound hosts (proxy mode) | Headers |
|---|---|---|---|---|---|
| `github` | `pat` (classic or fine-grained) | `GET api.github.com/user` | login; scopes (`X-OAuth-Scopes`), expiry (`github-authentication-token-expiration`) when sent | `github.com`, `api.github.com`, `uploads.github.com` | `Authorization` |
| `claude` | `setup-token` (`sk-ant-oat…`), `oauth` (sign-in) | prefix + a no-cost authenticated call (builder: the profile endpoint Claude Code uses) | email when available | `api.anthropic.com` | `Authorization` |
| `linear` | `api-key` | GraphQL `{ viewer { email } }` | email | `api.linear.app`, `mcp.linear.app` | `Authorization` |

- **Why these hosts:** GitHub: `github.com` (git smart HTTP, Basic), `api.github.com` (`gh`), `uploads.github.com` (`gh release upload`). Archive and asset downloads redirect to `codeload.github.com` / `objects.githubusercontent.com` with a short-lived token in the URL, not ours, so they need no binding. Claude: the project-4 spike showed Claude Code sends the token only to `api.anthropic.com`; `claude.ai` is not bound (narrower CA). Linear: GraphQL API and the remote MCP server.
- **Scope and uniqueness:** one integration per (org, scope owner, kind). Two kinds of the same scope never conflict; the same kind on org, team and private is resolved like variables *(Our call: one per kind keeps "which token does `gh` use" answerable)*.
- **Delivery mode** `proxy` (default) or `vm`, changeable at any time by whoever may `integration:set` the scope.
- **Placeholder:** `obsph_` + 32 base32, generated at create, stable for the integration's life (survives replace, re-verify, refresh, mode switches), never returned by the API. Same format and rules as host-bound variables.
- **Replace:** paste a new token (or sign in again); it is verified before it is stored. Verification failure → nothing changes.
- **Status:** `ok`, `expired` (provider refused at refresh or re-check, Claude refresh failed with `invalid_grant`), shown with the last error *(Our call: no periodic re-verification of PATs; GitHub's expiry header is stored and shown as a warning 7 days ahead)*.

## Resolution

- `ResolveVMIntegrations(ctx, enforcer, org, owner)` next to `ResolveVMEnv`, same shape: owner `user:<id>` → scopes org + that user's private; `team:<id>` → org + that team. A scope counts only if the owner has `integration:use` on its object. Per kind the narrowest scope wins (private/team over org).
- In `boot()` (`internal/fleet/fleet.go`): each resolved integration in `proxy` mode is appended to `bound` as `HostBoundSecret{ID: "integration:<id>", Name: "<kind> integration", Placeholder, Value, Hosts, Headers}`; the egress CA's host set and the 100-host cap include them. Every resolved integration (both modes) goes into the new `DesiredVM.Integrations []proto.IntegrationToken{Kind, Token, ExpiresAt}` — `Token` is the placeholder in proxy mode, the value in `vm` mode.
- `DesiredVM.Integrations` follows the `Egress` rules: only in the desired list of the VM's node, only in node memory, never in `Boot`, the seed, `vm.json` or status messages.
- Changes to an integration (create, replace, refresh, mode, delete) and to rules that affect `integration:use` trigger a desired push for the affected VMs, as variable changes do.

## Token service

- **Transport** *(Our call)*: the node listens on `<jail>/v.sock_1051` for every VM it runs (Firecracker's guest-initiated vsock; CID 2, port 1051), created before boot/resume, removed at stop. The guest agent on port 1050 is unchanged and still never carries secrets.
- **Protocol:** one JSON line per connection: `{"op":"token","kind":"github"}` → `{"ok":true,"token":"…","expiresAt":null|"…"}`, or `{"ok":false,"error":"not-connected"|"unavailable"}`. Unknown kinds and malformed requests: `not-connected`. 4 KiB request limit, 2 s timeout, 16 concurrent connections per VM.
- **Who in the guest may ask:** any process. In `vm` mode the token is meant to be in the guest anyway; in proxy mode the answer is a placeholder *(Our call: restricting to root would only move the token into a user-readable file)*.
- **After a node restart** the service answers `unavailable` until the hub's first push; helpers keep what they already wrote.
- **Log:** one line per request in the node journal — VM id, kind, result; never the token.

## Guest glue

Shipped in the seed and installed by `vmm-boot.sh` at every boot, resume and takeoff (static files, no secrets). The guest user is `ubuntu` as today.

- **`/usr/local/bin/vmm-token <kind>`** (python3, `AF_VSOCK` is in the stdlib): prints the token, exit 0; exit 3 when not connected, 4 when unavailable. Rejects tokens with characters outside `[A-Za-z0-9._~+/=-]` (no newline injection into git's credential protocol), as in the PoC.
- **git:** `/etc/vmm/gitconfig` with `[credential "https://github.com"] helper = vmm` and `insteadOf` for `git@github.com:` and `ssh://git@github.com/`; included from `/etc/gitconfig` only while GitHub is connected (PoC logic: add the `[include]` on 200, remove it on not-connected). `git-credential-vmm` answers `get` for `https://github.com` only, with `username=x-access-token`. In proxy mode git sends the placeholder as Basic password; the proxy decodes and substitutes it.
- **`gh`:** wrapper `/usr/local/bin/gh` sets `GH_TOKEN` from `vmm-token github` only when `GH_TOKEN` is empty, then execs `/usr/bin/gh`. `GH_TOKEN` is not exported globally, so a user's own variable wins *(Our call)*.
- **Claude:** `vmm-integrations.service` (oneshot, as the user for file work) writes `~/.claude/.credentials.json` as `{"claudeAiOauth":{"accessToken":<token>,"refreshToken":null,"expiresAt":null,"scopes":["user:inference"]}}`, only if the file is missing or one we wrote (hash bookkeeping in `/var/lib/vmm`, as in the PoC), via temp file + rename, mode 0600. With `expiresAt: null` Claude Code never tries to refresh. On not-connected it removes only its own file. Written even before Claude Code is installed, so project 7's install picks it up.
- **Linear:** `/etc/profile.d/vmm-linear.sh` exports `LINEAR_API_KEY` from `vmm-token linear` when unset; the sync adds the MCP server with `claude mcp add --transport http --scope user linear https://mcp.linear.app/mcp --header "Authorization: Bearer <token>"` when `claude` exists and the token changed, and removes only an entry it added.
- **When the sync runs:** a timer every 5 minutes and 30 s after boot; the guest agent's `resume` and `takeoff` start it too, so a hibernated VM gets a fresh Claude token on wake *(Our call)*. In proxy mode the content is stable, so the sync is a no-op after the first run.
- **Not done:** tool installation (project 7); `credential.helper store` set by the user would persist an inside-VM token in `~/.git-credentials` (documented).

## Claude OAuth

- **Sign-in** *(Our call, as in the reference design)*: PKCE with Claude Code's public OAuth client; the hub builds the authorize URL, the user signs in in their browser, copies the code from the provider's callback page into the dialog, the hub exchanges it. Pending PKCE state (verifier, user, org, scope) lives in hub memory for 10 minutes; a hub restart means "start again".
- **Storage:** access token and refresh token sealed separately with the org key (AAD `integration:<org>:<id>:access` / `:refresh`), plus `expires_at`. The refresh token never leaves the hub.
- **Refresh:** a hub loop every minute picks `oauth` integrations whose access token expires within 60 minutes (row lock, so a slow refresh is not run twice), refreshes, stores the new access and the rotated refresh token in one transaction, then pushes. Proxy mode: the proxy table's value changes, the placeholder does not — effective on the next request. `vm` mode: the guest sync rewrites the file within 5 minutes, well inside the remaining hour.
- **Failure:** network or 5xx → retry with backoff until expiry; `invalid_grant` → status `expired`, logged, UI shows "Sign in again". VMs keep the last access token until it expires.
- **Disconnect:** the hub drops both tokens and calls the provider's revoke endpoint if one exists (builder); an access token already inside a `vm`-mode guest stays valid until it expires (hours).
- **Risk:** the sign-in depends on Claude Code's public client and endpoints; if they change, `oauth` breaks and `setup-token` keeps working. Client id, endpoints and scopes are taken from the current Claude Code and checked in the real-hardware run.

## Mode switches and revocation

| Change | Proxy mode | Inside-VM mode |
|---|---|---|
| Connect | Placeholder works at once; guest config within 5 min (or at boot) | Token in guest within 5 min |
| Replace token / refresh | Next request | Next sync (≤ 5 min); running `gh`/git calls fetch per call |
| proxy → vm | Real value delivered on next sync | — |
| vm → proxy | Placeholder delivered on next sync; the old real value was in the guest — UI says "rotate this token at the provider" | — |
| Disconnect | Next request fails; config removed within 5 min | Config removed within 5 min; the token stays valid at the provider — UI links to the provider's token page to revoke it |
| Owner loses `integration:use`, member leaves team | Next push | Next push + sync; same caveat |

## Security

- **Proxy mode** inherits every egress guarantee: value only on the node running the VM, only to the bound hosts over verified TLS, scrubbed from responses, not in snapshots or forks.
- **Inside-VM mode** is the PoC's model: any process in the guest, every memory snapshot, hibernate file and fork can hold the token. The UI says so in the mode picker; team-scope `vm` mode additionally warns that every member who can connect to a team VM can read it.
- **Claude**: in both modes only an access token (≤ hours) or a placeholder reaches the guest; a leaked guest cannot mint long-lived access or log the user out elsewhere.
- **Node:** tokens arrive only in the desired list of VMs on that node (all of one org) over the node connection, live in memory, are never written to disk; the vsock socket sits inside the VM's jail directory (0700, jail UID), so another VM's Firecracker cannot open it.
- **Connect check unchanged:** a user VM admits only its owner whatever its integrations; anyone connected to a team VM can use the team's and org's integrations (and read them in `vm` mode), like team variables.

## Data model (outline)

- `integrations`: id, org_id, kind, source, scope (`org`/`team`/`user`), team_id, user_id (exactly per scope), mode (`proxy`/`vm`), account, scopes_info, provider_expires_at, nonce + ciphertext (token or access token), refresh_nonce + refresh_ciphertext (oauth only), access_expires_at (oauth only), placeholder (unique), status, last_error, created_by/at, updated_by/at. Unique (org_id, scope, coalesce(team_id, user_id), kind). Same cascade rules as variables: team delete and member removal delete their integrations *(Our call)*.
- Seal AAD `integration:<org>:<id>:<field>`; row swaps fail to open.
- PKCE state, token service sockets: memory only.

## Failure semantics

| Failure | Behaviour |
|---|---|
| Provider unreachable at connect/replace | Refused with the reason; nothing stored. |
| Token service unavailable (node restart, hub not yet pushed) | Helpers keep existing config; `gh`/git get no token and fail auth. |
| Proxy down | Egress spec: bound hosts get the placeholder and refuse it. |
| Claude refresh fails | Retry until expiry, then `expired`; UI asks to sign in again. |
| Hub unreachable for hours | Proxy and token service keep the last values; a Claude access token expires after its lifetime. |
| Guest without `claude` or `gh` | Credentials file and gitconfig still written; MCP added once `claude` appears. |

## Logging and audit

- Audit, in the same transaction: `integration.set` (create, replace, sign-in, mode change; details: kind, source, scope, owner id, mode, account, created vs. replaced) and `integration.delete`. Refreshes are logged, not audited; refresh failures are logged with the integration id and provider status, never tokens.
- Proxy log lines name the integration as `vars=github integration` (from `HostBoundSecret.Name`).
- Request bodies of the integrations API are never logged (as for variables).

## Permissions

New catalog entries, mirroring variables:

| Action | Object |
|---|---|
| `integration:list`, `integration:set`, `integration:delete`, `integration:use` | `org/integrations` |
| same | `org/team-integrations/<team-id>` |
| same | `org/user-integrations/<user-id>` |

- `integration:list` shows kind, source, scope, mode, account, status, expiry — never a token or placeholder; no action returns them. `integration:use` means "VMs I own receive this integration", checked by the resolver.
- Defaults identical to the variables table: members list and use org integrations; admins and owners set/delete org and all team integrations; team admins set/delete their team's; team subjects get `integration:use` on `org/integrations` and their own team object (generated); every member gets all four on their own `org/user-integrations/<id>` (generated). Org-wide rules work as for variables, e.g. `deny integration:set@org/user-integrations/*` ("no private integrations").
- The mode is part of `integration:set`; no separate action *(Our call)*. All new actions go through the escalation rule.

## UI

- **Integrations** page in the main navigation after Environment, `/orgs/:orgId/integrations`. Tabs All / Org / Teams / Private as on Environment.

```
Integrations                                                   [+ Connect]
 All   Org   Teams   Private
KIND     SCOPE          ACCOUNT            DELIVERY       STATUS
GitHub   private        octocat            proxy          ok · expires in 41 days   Replace  Mode  Disconnect
Claude   private        me@example.com     proxy          ok                        Sign in again  Mode  Disconnect
Linear   team Backend   bot@example.com    inside VM      ok                        Replace  Mode  Disconnect
```

- **Connect dialog:** kind, scope (only scopes the caller may `integration:set`, without kinds already connected there), source (Claude: "Sign in" or "Paste setup token"), token field (write-only), delivery: "Through the proxy (recommended): VMs get a placeholder" / "Inside the VM: programs in the VM can read the token". Shows the verified account before saving.
- **Disconnect / vm → proxy:** confirmation with the provider link to revoke the token when it was ever delivered in `vm` mode.
- **VM detail:** "Integrations" line: per kind the scope and mode the VM gets (e.g. "GitHub · private · proxy"), from the resolver; `vm:read` holders other than the owner see kinds only.
- Actions shown only when permitted (`can()`); de + en.

## Left to the builder

- REST paths and payloads (list, connect, replace, mode, delete, OAuth start/finish), validation messages.
- Claude Code OAuth client id, authorize/token/revoke endpoints, scopes and the verify endpoint, taken from the current Claude Code; GitHub/Linear verify via injectable verifiers (as in the PoC).
- Exact vsock listener lifecycle in the node (per-VM goroutine, socket in the jail before Firecracker starts), and how `DesiredVM.Integrations` changes reach it without a VM restart.
- How the guest sync runs as the user (PoC: `runuser` with `HOME` set; files touched only as the user so planted links get no root access).
- Test strategy; must include: no token or placeholder in any API response, audit row, log, `Boot`, seed or `vm.json`; refresh token never in a desired message; private integration never in a team VM or another user's VM; narrowest scope wins per kind; `integration:use` deny drops it; token service of VM X never answers for VM Y (socket per jail); proxy mode produces `HostBoundSecret`s with the kind's hosts and `Authorization` only; mode switch without reboot; Claude refresh (fake token endpoint: rotation stored atomically, `invalid_grant` → `expired`, no double refresh); guest scripts against a fake `vmm-token` (PoC-style shell tests: gitconfig include added/removed, user's own credentials file left alone, MCP added/removed only when ours, `GH_TOKEN`/`LINEAR_API_KEY` not overridden, token with a newline rejected); permission table incl. deny rules and cross-org isolation.
- **Real-hardware run on Proxmox with dummy credentials only** (test VM 108 as node of org A, 107 as node of org B; hub on the Mac over the tailnet). The hub runs with a dev-only flag that skips provider verification (refused unless the hub runs in dev mode, logged at start). Checks: in proxy mode `vmm-token github` prints `obsph_…`, `gh api user` and `git ls-remote https://github.com/<public repo>` reach GitHub and the proxy log shows `vars=github integration` with GitHub's `401` for the dummy value, `env`/`/proc/*/environ`/the credentials file contain no dummy value; in `vm` mode the dummy value appears in the credentials file, `git credential fill` and `gh`'s environment within 5 minutes, and disappears after disconnect; `claude mcp list` shows the Linear entry when a `claude` binary is present (stub if project 7 is not built); Claude refresh against a local fake token endpoint (dev-only override of the endpoint URL) with the guest file rewritten; hibernate/resume triggers the sync; a team VM never shows the private dummy; dump the desired messages on 107 to show no org A token; another VM on 108 cannot open the first VM's vsock socket.
- **Real-credential checks the user runs themselves** (documented in `docs/integrations.md`, not run by agents): connect a fine-grained read-only GitHub PAT for one private repo, Claude via sign-in, a Linear key; then in a VM `gh api user`, `git clone git@github.com:<you>/<private>.git`, `claude -p "say ok"` (and again after 9 hours for the refresh), `claude mcp list`, `curl -s -H "Authorization: $LINEAR_API_KEY" -H 'Content-Type: application/json' -d '{"query":"{ viewer { email } }"}' https://api.linear.app/graphql`; switch each to `vm` mode and back; disconnect and see the next call fail. A script `scripts/integrations-check.sh <vm>` runs the in-guest part over SSH and prints pass/fail per line without echoing any token.
- Add `docs/integrations.md` (kinds, hosts, modes and their risks, guest files, what is overwritten and what not, revocation, known limits: containers, `credential.helper store`, tokens in snapshots in `vm` mode, OAuth client dependency); update `docs/permissions.md` (`integration:*`) and `docs/egress.md` (integrations as a source of host-bound secrets).

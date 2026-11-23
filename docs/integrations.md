# Integrations (GitHub, Claude, Linear, gcloud)

An integration is a login a member connects once (on the org, a team or privately), so every VM they are entitled
to has working `git`/`gh`, Claude Code, the Linear MCP server and `gcloud` without any setup inside the guest. By default the
VM holds only a placeholder and the egress proxy puts the real token into requests ([egress.md](egress.md)); per
integration the owner can deliver the real token into the VM instead.

```
vmm-token github                     # obsph_rhhfld5sx222jbgys6izpeqdyegykest (proxy mode)
gh api user                          # works: the proxy sends the real token to api.github.com
git clone git@github.com:me/private  # works: rewritten to HTTPS, the credential helper answers
```

## Kinds

| Kind | Sources | Verified with | Account shown | Bound hosts (proxy mode) |
|---|---|---|---|---|
| `github` | `pat` (classic or fine-grained) | `GET api.github.com/user` | login, scopes, expiry when GitHub sends it | `github.com`, `api.github.com`, `uploads.github.com` |
| `claude` | `setup-token` (`claude setup-token`, `sk-ant-oat…`), `oauth` (sign-in) | `GET api.anthropic.com/v1/models` | email after sign-in; none for setup tokens | `api.anthropic.com` |
| `linear` | `api-key` | GraphQL `{ viewer { email } }` | email | `api.linear.app`, `mcp.linear.app` |
| `gcloud` | `adc` (`authorized_user` ADC file of `gcloud auth application-default login`) | token refresh at `oauth2.googleapis.com`, email from the token | email | `*.googleapis.com`, `*.pkg.dev` |

The proxy substitutes the `Authorization` header only.

- **GitHub:** `github.com` carries git smart HTTP (Basic auth), `api.github.com` is `gh`, `uploads.github.com` is
  `gh release upload`. Archive and asset downloads redirect to `codeload.github.com` /
  `objects.githubusercontent.com` with a short-lived token in the URL, so they need no binding.
- **Claude:** Claude Code sends its token only to `api.anthropic.com`; `claude.ai` is not bound.
- **Linear:** the GraphQL API and the remote MCP server.
- **gcloud:** every `*.googleapis.com` host and Artifact Registry (`*.pkg.dev`); see [gcloud](#gcloud).

A token is verified before it is stored; if verification fails nothing changes. GitHub's expiry is shown as a warning
7 days ahead. There is no periodic re-verification.

## gcloud

gcloud, Terraform and Google SDKs (Python and others, REST and gRPC) use your Google login in your VMs. It is a
personal login: **private scope and proxy mode only**, one account; org and team scope and `vm` mode are refused.
The token page is <https://myaccount.google.com/permissions>.

Connect after your daily local login:

```
gcloud auth application-default login
observatory integration connect gcloud [--quota-project P] [--from-file F]
```

The command sends the local ADC file to the hub (or pastes it on the Integrations page) and creates the integration or
replaces its token, so the daily command is always the same. A quota project from `--quota-project` or the file is
stored and can be edited on the integration page (`PATCH /api/orgs/{org}/integrations/{id}` `{"quotaProject": "…"}`,
empty or a project id; audited as `integration:set`, action `quota-project`). The hub verifies the login by refreshing
it and refreshes the access token 10 minutes before it expires; failures retry after 1, then every 2 minutes. If Google answers `invalid_grant` the status becomes
`expired` ("sign in again"): run the two commands above.

What the guest gets:

- `~/.config/gcloud/application_default_credentials.json` with `type` `authorized_user`, the placeholder as
  `client_id`, `client_secret` and `refresh_token`, and `quota_project_id` when set. Written only if missing or still
  the file the sync wrote, removed only if still ours. Google libraries find it themselves; their refresh is answered
  by the proxy ([egress.md](egress.md#wildcard-bindings)).
- A `gcloud` wrapper in `/.observatory/bin` that sets `CLOUDSDK_AUTH_CREDENTIAL_FILE_OVERRIDE` to that file and
  `CLOUDSDK_CORE_CUSTOM_CA_CERTS_FILE` to `$SSL_CERT_FILE`. It does so only when gcloud is connected, neither variable
  is set already, the active gcloud configuration has no `[core] account` (no `gcloud auth login` in the VM), and the
  ADC file is still ours; otherwise your `gcloud` runs unchanged. Set the project with `gcloud config set project`.
- `GRPC_DEFAULT_SSL_ROOTS_FILE_PATH` among the system-wide trust variables, for gRPC clients.

Artifact Registry (`<region>-python.pkg.dev`, `<region>-docker.pkg.dev`) takes the login as Basic auth with the user
`oauth2accesstoken`; the proxy substitutes the placeholder there too. For Python packages with uv, install the keyring
and let uv ask it:

```
uv tool install keyring --with keyrings.google-artifactregistry-auth
export UV_KEYRING_PROVIDER=subprocess
uv pip install --index-url https://oauth2accesstoken@<region>-python.pkg.dev/<project>/<repo>/simple/ <package>
```

For Docker: `gcloud auth print-access-token | docker login -u oauth2accesstoken --password-stdin <region>-docker.pkg.dev`.
In a VM whose Docker daemon was already running when gcloud was connected (or before `*.pkg.dev` was bound), run
`sudo systemctl restart docker` first: the daemon reads trust at start.

Known limits of gcloud:

- **`revoke` on your computer** (`gcloud auth application-default revoke`, `gcloud auth revoke`) also kills the hub's
  login, which is a copy of your refresh token. A plain new local login does not.
- **`gcloud auth list`** shows no account while the override is active.
- **Team VMs never get gcloud**, because it is private and team VMs never receive private integrations.
- **Programs that call `gcloud` by absolute path** bypass the wrapper and get no override.
- **The quota project** is used by SDKs (Python and others) only. gcloud sends its own project, and Terraform only with
  `user_project_override`.
- **After expiry** Google answers 401; the proxy sends no synthetic `invalid_grant`.
- **A token in a query parameter** is not substituted (headers only).
- **Not in the standard image:** gcloud and Terraform are yours to install.

## Scopes and resolution

Integrations live in the same three scopes as variables: org (`org/integrations`), team
(`org/team-integrations/<team>`) and private (`org/user-integrations/<user>`). There is at most one integration per
kind and scope; to move one, disconnect it and connect it in the other scope.

- A **user VM** gets the org's and its owner's private integrations; a **team VM** the org's and its team's, never a
  private one.
- A scope counts only if the VM's owner has `integration:use` on it ([permissions.md](permissions.md#integrations)).
- Per kind the narrowest scope wins (private or team over org).
- There is no per-VM selection: a VM gets every integration its owner is entitled to.

Changes (connect, replace, refresh, mode switch, disconnect, rules affecting `integration:use`, team membership) reach
the affected VMs' nodes with the next push. A node only receives the integrations of VMs scheduled on it, so a node of
another org never receives them.

## Delivery modes

| Mode | What the VM gets | Risk |
|---|---|---|
| `proxy` (default) | The placeholder; the node's egress proxy puts the real token into requests to the bound hosts. | Same guarantees as host-bound variables: the value never enters the guest, snapshots, hibernate files or forks. Anything running in the VM can still *use* the token against the bound hosts while it runs. |
| `vm` ("inside the VM") | The real token. | Every process in the guest, every memory snapshot, hibernate file and fork can hold the token. A team-scope integration in `vm` mode is readable by every member who can connect to a team VM. |

The mode is changed by whoever may `integration:set` the scope, and audited.

**Placeholder:** `obsph_` followed by 32 base32 characters, generated when the integration is connected and kept for
its whole life (replace, sign in again, refresh, mode switches), so a token change needs no reboot. No endpoint returns
it; it is worthless in a VM not entitled to the same integration.

**Claude:** in both modes only an access token (valid for hours) or the placeholder reaches the guest. The refresh
token never leaves the hub, so a leaked guest cannot mint long-lived access.

## Connecting

Integrations page (`/orgs/:orgId/integrations`, tabs All / Org / Teams / Private), or the API:

| Method and path | Does |
|---|---|
| `GET /api/orgs/{org}/integrations` | List (kind, source, scope, mode, account, status, expiry; Claude also `accounts` and `lastSwitch`; never a token or placeholder) and the scopes the caller may write |
| `POST /api/orgs/{org}/integrations` | Connect a pasted token: `kind`, `source`, `scope`, `teamId`, `mode`, `token`; `dryRun` only verifies |
| `PUT /api/orgs/{org}/integrations/{id}/token` | Replace the token |
| `PATCH /api/orgs/{org}/integrations/{id}` | Change the mode |
| `DELETE /api/orgs/{org}/integrations/{id}` | Disconnect |
| `POST /api/orgs/{org}/integrations/{id}/accounts` | Add a Claude account by pasted setup token (`source`, `token`); 201 with the account id |
| `PUT /api/orgs/{org}/integrations/{id}/accounts/{accountId}/token` | Replace one account's token |
| `POST /api/orgs/{org}/integrations/{id}/accounts/{accountId}/activate` | Make the account active |
| `PUT /api/orgs/{org}/integrations/{id}/accounts/order` | Set the order: `{"ids":[…]}`, every account once |
| `DELETE /api/orgs/{org}/integrations/{id}/accounts/{accountId}` | Remove an account; the last one disconnects the integration (needs `integration:delete` too) |
| `POST /api/orgs/{org}/integrations/claude/sign-in`, `…/sign-in/finish` | Claude sign-in (below); `addToId` (integration) adds an account, `replaceAccountId` replaces one |

From the CLI, the token comes from stdin, never an argument: `printf %s "$TOKEN" | observatory integration connect github`
(see [cli.md](cli.md#secrets-input)).

Request bodies of these endpoints are never logged. Audit rows `integration:set` (create, replace, sign-in, mode
change) and `integration:delete` carry kind, source, scope, owner, mode and account, never a token; a mode change also
carries `previousMode`.

### Claude sign-in

The hub uses Claude Code's public OAuth client with PKCE: it builds the authorize URL, you sign in in your browser,
copy the code from the provider's callback page into the dialog, and the hub exchanges it. A pending sign-in lives in
hub memory for 10 minutes and is bound to you and the org; after a hub restart, start again.

- Access and refresh token are sealed separately with the org key. The hub refreshes an access token once it
  expires within 60 minutes (checked every minute) and stores the new access and rotated refresh token in one
  transaction.
- Failures back off 1, 2, 4 … up to 15 minutes and retry until the access token expires; then, or at once on
  `invalid_grant`, the status becomes `expired` and the page says "Sign in again". VMs keep the last access token until
  it runs out.
- Proxy mode: the proxy's value changes, the placeholder does not, so the next request uses the new token. `vm` mode:
  the guest sync rewrites the credentials file within 5 minutes, well inside the remaining hour.

### Several Claude accounts

A private Claude integration in `proxy` mode can hold several accounts (your own subscriptions, e.g. work and
private). Org and team Claude integrations, and `vm` mode, keep exactly one: the mode cannot be changed while there is
more than one account (409), and adding an account to such an integration is refused (409). Each account is a row with
its email (setup-token accounts: "Setup token"), position and active flag. The same email cannot be added twice.

- **One account is active**; all your VMs use it, with the same placeholder, so a switch needs no guest change.
  Flipping another row's switch activates it at once, even when it is limited. The active one cannot be switched off.
- **Order is priority:** it decides which account comes next. Reordering never changes the active account.
- **Switch automatically** (per integration, default off, also for integrations created before it existed): only
  when on does the hub switch on a limit and does the proxy retry with the next account (Limit, Retry and Transition
  only below). When off, the hub never switches by itself, the node gets only the active account's token (the other
  tokens never reach it), and a confirmed 429 reaches Claude Code; switch yourself. Usage, polling and manual
  switching work either way. Set it with `PATCH /api/orgs/{org}/integrations/{id}` `{"autoSwitch": true|false}`
  (`mode` may be sent alongside or alone); refused for non-Claude integrations (400); audited as `integration:set`
  with `action` = `auto-switch`. The list shows `autoSwitch` on each integration.
- **Add** with the same sign-in or a pasted setup token (`addToId`); the account is appended, switched off. **Sign in
  again** per account (`replaceAccountId` or the token route). **Remove** any account: removing the active one
  activates the next eligible one; removing the last disconnects.
- **Refresh:** the hub refreshes every `oauth` account that is due, not only the active one. An `expired` account is
  never switched to and not sent to the node.
- **Usage:** the hub polls each `oauth` account's usage (5-hour and weekly window, percent and reset) every minute for
  the active account and every 15 minutes for the others; errors back off 1 to 15 minutes and keep the last values.
  The egress proxy also reads the rate-limit headers of Anthropic's responses for the account that served the request; the newer
  reading wins and the row shows its source and age. Setup-token accounts that the usage endpoint rejects show "no
  usage data" until proxy readings arrive while active.
- **Limit:** a 429 with `anthropic-ratelimit-unified-status: rejected` and an overage status that is absent or
  `rejected`. Until is the unified reset header, else `retry-after`, clamped to 5 s to 7 days. A 429 without these
  headers passes through unchanged. A polled account counts as limited at 100 % in either window with the reset in
  the future.
- **Retry:** the hub sends each VM's proxy the active account's token plus the other non-`expired` accounts in order.
  On a confirmed limit the proxy resends the same request with the next account that is not limited, so Claude Code
  does not see the limit ([egress.md](egress.md#claude-requests-apianthropiccom)). It reports the limit at once; the
  hub activates the first account in order that is not limited, not `expired` and not the current one, and pushes to
  all your VMs' nodes.
- **Transition only:** an account you switched on while limited stays active until that limit ends; the proxy still
  retries, the hub does not move the switch. A later limit switches as usual. An account switched away from automatically is not switched back before its limit ends. If no
  account is eligible, the active one stays and the page says all are limited until the earliest reset. When the
  active, non-manual account is still limited and another account's limit has passed, the hub moves to the first
  available one.
- **Page:** after an automatic switch the integration shows "Switched automatically at <time>: <email> reached its
  limit" until the next manual action. Automatic switches are logged, not audited. Adding, replacing, removing,
  activating and ordering are audited as `integration:set` (`action` = `account-add`, `account-replace`,
  `account-remove`, `account-activate`, `account-order`); removing the last account is one `integration:delete` row.
- **Permissions:** `integration:set` on your private integration; no new actions.

List JSON (Claude): `accounts: [{id, account, source, position, active, status, lastError, usage: {fiveHourPct,
fiveHourResetsAt, weekPct, weekResetsAt, limitedUntil, source, at}}]` and `lastSwitch: {at, fromId, from, toId, to}`
or `null`. `from` and `to` are empty for setup-token accounts. Top-level `account`, `source` and `status` are the
active account's.

## In the guest

The init writes these files at every boot, resume and takeoff; none of them contains a token. The guest
user is `dev`. The helpers are the init binary under other names and live in `/.observatory/bin`, a read-only
directory that comes first in `PATH`, so they work in images without a shell.

| File | Does |
|---|---|
| `vmm-token <kind>` | Asks the node's token service; prints the placeholder (proxy) or the token (`vm`). Exit 0 printed, 3 not connected, 4 unavailable (also timeout, socket error, a token with characters outside `[A-Za-z0-9._~+/=_-]`, which keeps newlines out of git's credential protocol). |
| `git-credential-vmm` | git credential helper: answers `get` for `https://github.com` only, as `x-access-token`. |
| `/etc/vmm/gitconfig` | `helper = vmm` for `https://github.com`; `insteadOf` turns `git@github.com:` and `ssh://git@github.com/` into HTTPS. |
| `/etc/gitconfig` | Gets an `[include]` of `/etc/vmm/gitconfig` while GitHub is connected; the sync removes only that include. |
| `gh` | Wrapper: sets `GH_TOKEN` from `vmm-token github` and runs the image's own `gh`, unless `GH_TOKEN` or `GITHUB_TOKEN` is set or `gh auth login` stored a `github.com` login. The platform directory is read-only. |
| `~/.claude/.credentials.json` | `{"claudeAiOauth":{"accessToken":…,"refreshToken":null,"expiresAt":null,"scopes":["user:inference"]}}`, mode 0600. With no expiry Claude Code never tries to refresh. Written even before Claude Code is installed. |
| Linear MCP | `claude mcp add --transport http --scope user linear https://mcp.linear.app/mcp --header "Authorization: Bearer <token>"`, once `claude` exists. |
| `/etc/profile.d/vmm-linear.sh` | Exports `LINEAR_API_KEY` from `vmm-token linear` in login shells. |
| The sync | Runs inside the init: after boot, then every 5 minutes; also on resume and on `takeoff` (fork), so a woken or forked VM is current at once. In proxy mode the content is stable and the sync is a no-op after the first run. |

The sync runs as root for `/etc/gitconfig` and as `dev` for everything in the home directory, so a
planted link never gets root access. Its bookkeeping holds hashes, never tokens.

**Token service:** the guest connects over vsock to the host (CID 2, port 1051); Firecracker maps that to the socket
`v.sock_1051` inside the VM's jail, so the socket a request arrives on is the VM's identity
([nodes.md](nodes.md#integrations)). Any process in the guest may ask: in `vm` mode the token is meant to be there, in
proxy mode the answer is a placeholder. After a node restart the service answers "unavailable" until the hub's first
push; the helpers keep what they already wrote. The same holds while the hub cannot read a VM's integrations (for
example a database error): the push marks them unresolved, the node keeps the last tokens it had (or answers
"unavailable" if it had none) and the hub pushes again a minute later.

### What is never overwritten

- A Claude login of your own (`claude /login`): the sync writes the credentials file only if it is missing or holds
  the token it wrote itself, and removes only its own file.
- Your own git credential helpers for other hosts: the include adds a helper for `https://github.com` only. For
  `https://github.com`, git reads `/etc/gitconfig` first, so `vmm` answers before helpers in `~/.gitconfig`. To use
  your own, reset the list with an empty value and add yours after it:

  ```
  [credential "https://github.com"]
  	helper =
  	helper = <yours>
  ```
- SSH remotes: the include rewrites `git@github.com:` and `ssh://git@github.com/` to HTTPS. To keep SSH for a remote,
  spell it so neither prefix matches, e.g. `ssh://git@github.com:22/<owner>/<repo>` or a `Host` alias from
  `~/.ssh/config`; `GIT_CONFIG_NOSYSTEM=1` drops the include (helper and rewrite) for one command.
- `GH_TOKEN`, `GITHUB_TOKEN` or a `gh auth login`: the wrapper steps aside.
- `LINEAR_API_KEY`: set only when unset.
- A `linear` MCP entry of your own: the sync records only an entry it added itself ("we added it") and never adds over
  or removes an existing one. If removing its own entry fails, it keeps the record and retries on the next sync.

## Mode switches and revocation

| Change | Proxy mode | Inside-VM mode |
|---|---|---|
| Connect | Placeholder works at once; guest config within 5 min (or at boot) | Token in the guest within 5 min |
| Replace token / refresh | Next request | Next sync (≤ 5 min); `gh` and git ask per call |
| proxy → vm | Real value delivered on the next sync | — |
| vm → proxy | Placeholder delivered on the next sync; the real value was in the guest, so the page asks to rotate or revoke it at the provider | — |
| Disconnect | Next request fails; config removed within 5 min | Config removed within 5 min; the token stays valid at the provider, so the UI links to the provider's token page to revoke it |
| Owner loses `integration:use`, member leaves team | Next push | Next push + sync; same caveat |

Claude Code's OAuth client has no revoke endpoint: disconnecting a signed-in Claude integration drops both tokens on
the hub, and an access token already inside a `vm`-mode guest stays valid until it expires (hours).

## Dev flags

Only for local testing with dummy tokens; refused at hub start unless `OBSERVATORY_DEV=1`, and logged as a warning:

- `OBSERVATORY_DEV_SKIP_INTEGRATION_VERIFY=1` stores any well-formed token as account "unverified".
- `OBSERVATORY_DEV_CLAUDE_TOKEN_URL=<url>` sends the Claude code exchange and refreshes to a local fake token
  endpoint.
- `OBSERVATORY_DEV_CLAUDE_USAGE_URL=<url>` sends the usage poll to a local fake usage endpoint.

## Known limits

- **Containers** inside the VM get no credential helper, wrapper or trust; integrations do not work there.
- **`credential.helper store`** set by the user writes the token git received into `~/.git-credentials`; in `vm` mode
  that is the real token, and it stays there after disconnect.
- **Tokens in snapshots in `vm` mode:** snapshots, hibernate files and forks of a VM keep the token it held then.
- **Claude Code's OAuth client:** sign-in depends on Claude Code's public client id and endpoints
  (`claude.com/cai/oauth/authorize`, `platform.claude.com/v1/oauth/token`); if they change, `oauth` breaks and
  `setup-token` keeps working.
- **Claude in proxy mode:** Claude Code reads a placeholder (`obsph_…`, no `sk-ant-` prefix) as its access token.
  That it accepts and sends it is verified only by the real-credential run below.
- **Linear token on argv and in `~/.claude.json`:** `claude mcp add --header` puts the header (the placeholder in proxy
  mode, the key in `vm` mode) into the guest user's `~/.claude.json` and, for the second the call runs, on its argv. A
  `headersHelper` that calls `vmm-token` would avoid both; not done yet.
- **Finding `claude`:** the sync looks in the service's `PATH` and `~/.local/bin`. The standard image's
  `/usr/local/bin/claude` is found; a Claude Code installed through nvm or an npm global prefix elsewhere is not, so
  the Linear MCP entry is not added for it.
- **Hub crash during a Claude refresh** can require signing in again: the provider rotates the refresh token when it
  answers, and if the hub dies before storing the new one the stored token is no longer valid.
- **Transient resolve error in proxy mode:** while the hub cannot read a VM's integrations, the guest keeps its tokens
  but the node's proxy loses the integration's values, so placeholders reach the provider unsubstituted until the
  retry a minute later succeeds.
- **`LINEAR_API_KEY` after disconnect:** shells opened before the disconnect keep the exported value (the
  placeholder, or the key in `vm` mode) until they exit.
- **Several Claude accounts:**
  - The usage endpoint (`api.anthropic.com/api/oauth/usage`) and the `anthropic-ratelimit-unified-*` headers are
    undocumented and may change; polling or limit detection then stops working while `setup-token` and `oauth` keep
    working.
  - Setup-token accounts usually have no polled usage (the endpoint rejects them); they show "no usage data" until
    the proxy reads headers while they are active.
  - A revoked token (401) does not trigger a switch; the account stays active until you switch it.
  - Each other VM may send one more request to a limited account before the hub's push reaches its node; the proxy
    retries it locally with the next account.
  - Proxy readings are throttled to one per account per 30 s; readings within the window are dropped (a limit is
    never dropped).
  - Up to 32 MiB of a request body is buffered in the proxy per in-flight Claude request of a multi-account
    integration, 256 MiB per VM's proxy in total; a larger body, or one that would exceed the total, is sent without
    retry.
- **No GitHub App, no OAuth for GitHub or Linear, no GitHub Enterprise Server, no `ghcr.io`/`docker login`.**

## Real-credential checklist

Agents test with dummy tokens only. These checks need real tokens and are run by you:

1. Create a fine-grained, read-only GitHub PAT for one private repository, a Linear API key, and have a Claude
   subscription ready.
2. Connect all three in your private scope, proxy mode: GitHub and Linear by pasting, Claude with "Sign in". The page
   shows your GitHub login and Linear email.
3. Create a fresh user VM and, from a device, run
   `scripts/integrations-check.sh dev@<vm>.internal <you>/<private-repo>`. Every line must be `PASS`, including the
   leak check (Claude Code and `gh` come with the standard image ([guest.md](guest.md)); another image needs them
   installed).
4. After 9 hours (the first access token has expired and been refreshed by the hub), run `claude -p "say ok"` in the
   VM again.
5. Switch each integration to "Inside the VM", wait 5 minutes, rerun the script (the `vmm-token` lines report `vm`,
   the leak check is skipped), switch back to proxy and rotate the tokens at the providers as the page says.
6. Disconnect each integration and check that the next `gh api user`, `claude -p "say ok"` and Linear call fail.
7. Revoke the PAT and the Linear key at the providers.
8. Several accounts: in your private Claude integration (proxy mode) add a second account. Both rows show usage
   within two minutes. Exhaust the active account's window or simulate it (`OBSERVATORY_DEV_CLAUDE_USAGE_URL` with
   dummy accounts reports 100 % but does not exercise the proxy retry), run `claude -p "say ok"` in a VM, and check
   that it succeeds, the switch moves to the other account and the page shows the automatic-switch note (with
   "Switch automatically" on). Flip the switch back by hand and check that it stays.

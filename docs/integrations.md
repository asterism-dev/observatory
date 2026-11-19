# Integrations (GitHub, Claude, Linear)

An integration is a login a member connects once (on the org, a team or privately), so every VM they are entitled
to has working `git`/`gh`, Claude Code and the Linear MCP server without any setup inside the guest. By default the
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

The proxy substitutes the `Authorization` header only.

- **GitHub:** `github.com` carries git smart HTTP (Basic auth), `api.github.com` is `gh`, `uploads.github.com` is
  `gh release upload`. Archive and asset downloads redirect to `codeload.github.com` /
  `objects.githubusercontent.com` with a short-lived token in the URL, so they need no binding.
- **Claude:** Claude Code sends its token only to `api.anthropic.com`; `claude.ai` is not bound.
- **Linear:** the GraphQL API and the remote MCP server.

A token is verified before it is stored; if verification fails nothing changes. GitHub's expiry is shown as a warning
7 days ahead. There is no periodic re-verification.

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
| `GET /api/orgs/{org}/integrations` | List (kind, source, scope, mode, account, status, expiry; never a token or placeholder) and the scopes the caller may write |
| `POST /api/orgs/{org}/integrations` | Connect a pasted token: `kind`, `source`, `scope`, `teamId`, `mode`, `token`; `dryRun` only verifies |
| `PUT /api/orgs/{org}/integrations/{id}/token` | Replace the token |
| `PATCH /api/orgs/{org}/integrations/{id}` | Change the mode |
| `DELETE /api/orgs/{org}/integrations/{id}` | Disconnect |
| `POST /api/orgs/{org}/integrations/claude/sign-in`, `…/sign-in/finish` | Claude sign-in (below) |

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

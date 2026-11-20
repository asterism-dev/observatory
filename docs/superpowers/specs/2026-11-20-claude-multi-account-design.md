# Claude integration: several accounts, usage limits, automatic switch

## Intent

A member who owns several Claude subscriptions (e.g. one from work, one private; using both is agreed with their
employer) wants to keep them all in their private Claude integration, see each account's usage limits, and choose
which one their VMs use. When the active account hits its limit, the next one takes over without anyone touching a
VM, and without the running Claude Code session noticing. Today an integration holds exactly one Claude login per
scope, and nobody sees how much of a limit is left.

The member runs this today on their laptop with their own `claude-router` (`~/Work/claude-router`), which works
well; its limit detection, request retry and stream watchdog are the reference for this design.

Success: a member signs in twice in their private Claude integration (proxy mode). The integrations page shows two
rows, `me@work.example` and `me@private.example`, each with the 5-hour and weekly usage and when they reset; the work
account's switch is on. In a running VM an agent uses the work account. When the work account's 5-hour window is
exhausted, the rejected request is sent again with the private account and the agent carries on without an error;
the switch on the page now shows the private account, and every later request of every VM of that member uses it.
When the work window resets, the private account stays active until the member flips the switch back. Dragging the
private row above the work row changes which account comes next, not which one is active. A stream from Anthropic
that goes silent is cut off with a clean error instead of hanging the agent.

## Decided with the user

- **Private scope only.** Several accounts are for one person's own subscriptions, not a pool shared in an org or a
  team. Org and team Claude integrations keep exactly one account.
- **One Claude integration holding several accounts** (not a second kind like "Claude (proxy)"). With more than one
  account the integration is proxy mode only; `vm` mode is possible only with a single account.
- **Rows on the integrations page**, one per account, named by the account's **email** (read at sign-in, not
  editable), with 5-hour and weekly usage and reset time, and a switch.
- **Exactly one account is active** (switch on); turning another one on turns the previous one off.
- **Order is the priority**, changed by dragging rows. It decides which account comes next on an automatic switch.
- **An account changes only** when the active one hits its limit or the member flips a switch. No automatic return
  when a higher-priority account's limit resets.
- **Limits come from both sources:** the hub polls every account's usage regularly, and for the active account the
  egress proxy additionally reads the rate-limit headers from Anthropic's responses.
- **The proxy retries a rejected request with the next account**, as `claude-router` does, so Claude Code never sees
  the limit. For that the node holds the access tokens of the member's other eligible accounts too.
- **Hanging requests must not stall the agent:** a stream watchdog as in `claude-router`.

Everything else is marked *(Our call.)*

## Goals

- Several Claude accounts in one private Claude integration, each its own sign-in (or setup token), with email,
  order, active flag, last known usage.
- Usage per account: 5-hour and weekly window, utilization in percent and reset time; for the active account updated
  from live responses.
- On a confirmed limit: the proxy retries the same request with the next eligible account and tells the hub, which
  makes that account active for all the member's VMs.
- Manual switch by flipping a switch; reorder by drag and drop.
- A switch takes effect on the next request in every VM of the member, with no guest change (the placeholder stays).
- Stream watchdog on Anthropic streams.
- Audit for manual switches, adding and removing accounts; automatic switches logged and visible on the page.
- de + en.

## Non-goals

- Several accounts in org or team scope; sharing accounts between members.
- Several accounts in `vm` mode, or a switch that reaches the guest's credentials file.
- Per-VM choice of account. All VMs of the member use the active account.
- Several accounts for GitHub or Linear.
- Automatic return to a higher-priority account after its limit resets.
- Account labels set by the member (email only for now).
- Falling back to another provider or model (`claude-router` can; here only accounts of the same integration).
- Notifications (mail, push) on a switch or when all accounts are limited *(Our call: the page shows it)*.
- Usage history or charts; only the last known values are kept.

## Anti-goals

- A refresh token of any account outside the hub (unchanged from the integrations spec).
- Any token in the guest, a log line, an audit row, an API response or a usage report.
- A token of a member's account on a node that runs none of that member's VMs.
- A placeholder change on a switch (would need a guest sync and break running sessions).
- An account of one member used by another member's VM.
- Switch flapping: two accounts alternating because of stale or noisy data.
- A request sent twice to Anthropic *except* the retry after a confirmed limit (which Anthropic rejected, so it
  did not run).
- Breaking existing single-account Claude, GitHub or Linear integrations, or their `vm` mode.

## Approaches

How the hub learns the limits and when it switches:

- **1) Hub polls the usage endpoint only.** Only the hub changes; up to a minute of failed requests, and the
  request that hit the limit fails.
- **2) Proxy reads the rate-limit headers only** (what `claude-router` does). Immediate, but only the active account
  has data; the other rows stay stale.
- **3) Both (picked by the user).** Polling fills every row; the proxy's headers keep the active row live and trigger
  the switch at once.

What happens to the request that hits the limit:

- **A) Proxy retries it with the next account (picked by the user).** The node holds the access tokens of all the
  member's eligible accounts and buffers request bodies to Anthropic. Claude Code never sees the limit.
- B) No retry. Simpler and only the active token on the node, but Claude Code shows "limit reached" and an
  unattended agent stops although the switch already happened.

Data model: a child table of the integration *(Our call)*, so placeholder, mode, scope and the node delivery stay on
the integration. Rejected: several `integrations` rows sharing a placeholder (breaks the unique index, the "one per
kind and scope" rule and resolution).

## Data model

- New table `integration_accounts`: id, integration_id (cascade delete), position (int, unique per integration),
  active (bool, exactly one true per integration: partial unique index), activated_manually (bool), account (email),
  source (`oauth`/`setup-token`), nonce + ciphertext (token or access token), refresh_nonce + refresh_ciphertext
  (oauth only), access_expires_at, status (`ok`/`expired`), last_error, usage (five_hour_pct, five_hour_resets_at,
  week_pct, week_resets_at, usage_source `poll`/`proxy`, usage_at), limited_until, created_by/at, updated_by/at.
  Seal AAD `integration:<org>:<account id>:<field>`; a migrated first account reuses its integration's id, so its sealed bytes move unchanged.
- **Migration** *(Our call)*: every existing Claude integration gets one account row carrying its current token
  fields, source, account, status. From then on Claude tokens are read only from `integration_accounts`; whether the
  token columns of Claude rows in `integrations` are dropped or set null is the builder's call, reading from two
  places is not acceptable.
- The same email may not be added twice to one integration *(Our call)*. Setup-token accounts have no email; they are
  shown as "Setup token" plus when it was added.
- The integration row keeps id, kind, scope, mode, placeholder, `was_in_vm`. Its `account` and `status` in the list
  become those of the active account.

## Accounts

- **Adding:** "Add account" on the private Claude integration runs the existing sign-in (or setup-token paste) and
  appends the account at the bottom, switched off. The first account of a new integration is active. Connecting a
  private Claude integration works as today and creates its first account.
- **Sign in again / replace** per account, as today per integration.
- **Removing** an account: allowed for any account. Removing the active one activates the next eligible one in
  order (same rule as an automatic switch); removing the last one disconnects the integration (same confirmation as
  disconnect).
- **Mode:** the mode control is disabled with a hint while an integration has more than one account; "Add account"
  is offered only in proxy mode. Org and team Claude integrations show no "Add account".
- **Refresh:** the existing refresh loop refreshes every `oauth` account that is due, not only the active one, so an
  inactive account's access token on the node is valid the moment the proxy retries with it. An `expired` account is
  never switched to and not sent to the node.

## Usage

- **Poll** *(Our call on intervals)*: the hub calls the usage endpoint Claude Code uses for `/usage` (builder: exact
  URL, headers, response fields, required scope) with each `oauth` account's access token. Active account every
  minute, inactive accounts every 15 minutes, within the existing one-minute hub loop. A poll error keeps the last
  values, sets `last_error` on the account and backs off like refresh does. Setup-token accounts probably lack the
  scope: if refused, they show "no usage data" until proxy headers (while active) fill them.
- **Proxy headers:** for responses from `api.anthropic.com` to a request whose `Authorization` the proxy substituted
  with a Claude integration's value, the proxy reads the headers Claude Code itself evaluates (as `claude-router`
  does): `anthropic-ratelimit-unified-status`, `-reset`, `-overage-status`, per window `-5h-utilization`,
  `-5h-reset`, `-5h-status` and the same for `7d`, plus `retry-after`. A response without
  `anthropic-ratelimit-unified-status` is ignored (the limiter did not run). The headers stay in the response.
- **Report** *(Our call)*: a new node→hub message type `usage` with `[{integration, account, status, fiveHourPct,
  fiveHourResetsAt, weekPct, weekResetsAt, limitedUntil, observedAt}]`, where `account` is the account id the hub
  sent with the token (no token, no email). The VM's egress process writes observations as lines on its stdout
  (today it writes only `ready` there); the node reads them and sends the `usage` message. The node reports at most once every 30 seconds per account, and at once
  on a confirmed limit. The hub accepts a report only for an account of an integration in that node's desired list.
- **Precedence:** the newer observation wins, whichever source it came from; the row shows its age ("live", "2 min
  ago").

## Limits and switching

- **Confirmed limit** *(Our call, same condition as Claude Code and `claude-router`)*: a 429 whose
  `anthropic-ratelimit-unified-status` is `rejected` and whose `-overage-status` is absent or `rejected` (while
  overage / extra usage is available the request is not blocked, so no switch). `limited_until` is
  `anthropic-ratelimit-unified-reset`, else `retry-after`, clamped to 5 s … 7 days. A 429 without these headers is a
  short rate limit and passes through unchanged. From polling, an account counts as limited when the 5-hour or weekly
  window reports 100 % and its reset is in the future. Per-model limits are covered because the rejection is unified;
  the UI shows only the 5-hour and weekly windows (as `claude-router`).
- **Retry in the proxy:** the hub sends the node, per multi-account integration, the active account's access token
  plus the other accounts that are not `expired`, in order, each with its account id. On a confirmed limit for the
  account in use, the proxy marks it limited until `limited_until` locally, and sends the same request again with
  the next account in order that is not limited locally, holding back the 429 until it has an answer. If that one is
  limited too it continues down the list; if none is left, Claude Code gets the last 429 as is. The egress proxy is
  one hardened process per VM (`cmd/node/egress.go`), so this local limit state is per VM: further requests of that
  VM use the account the retry succeeded with, until the hub's next push replaces the table. Other VMs, on the same
  or another node, learn it from that push; before it, each may send one more request to the limited account, which
  is retried the same way *(Our call: no limit state shared across VMs of a node; it would save at most one rejected
  request per VM and needs the isolated proxy processes to talk to each other)*.
- **Several nodes, several users:** each node running a VM of the member gets the member's account list; each VM's
  proxy table holds only the integrations its owner is entitled to, so another user's VM on the same node never sees
  the member's tokens or limit state. The hub alone decides the active account, so nodes cannot disagree beyond one
  push.
- **Buffering** *(Our call)*: only requests to `api.anthropic.com` whose `Authorization` carries a multi-account
  placeholder are buffered, up to 32 MiB of body; a larger body is streamed without a retry. Nothing is buffered for
  other hosts or single-account integrations. The 429's body is small and held in memory until the retry decides.
- **Hub decides the active account:** the node's report of a confirmed limit makes the hub activate the first account
  in order that is not limited, not `expired` and not the current one (the same one the proxy picked, as both use the
  same order and limits), in one transaction, and push desired to the nodes of all the member's VMs. VMs on other
  nodes switch with that push; their first request before the push may still go to the limited account and is
  retried there the same way.
- **Only a transition triggers** *(Our call)*: an account the member switched on while already limited
  (`activated_manually`) stays active; the proxy still retries a rejected request with the next account, but the hub
  does not move the switch.
- **Nobody eligible:** the active account stays active; the page shows "All accounts limited until <earliest
  reset>". When an account's `limited_until` passes while the active one is still limited and was not chosen
  manually, the hub switches to the first available one in order.
- **No flapping** *(Our call)*: an account that was switched away from automatically is not switched back to
  automatically before its `limited_until`, even if a later poll looks better.
- **Manual switch:** flipping a row's switch on activates that account at once, even if it is limited (the row shows
  the warning). Turning the active switch off is not possible; turn another one on.
- **Reorder:** drag a row; saves the new order and pushes it (the retry order changes). Does not change the active
  account.

## Stream watchdog

- As in `claude-router` *(Our call on scope and value)*: on responses from `api.anthropic.com` with
  `Content-Type: text/event-stream`, the proxy aborts the upstream request when no bytes arrive for 120 s (Anthropic
  sends pings on long streams; Claude Code gives up after 3 minutes, so the proxy must fire first), appends an
  Anthropic `event: error` with an `api_error` "upstream sent nothing for 120s" to the stream so the client does not
  take the truncated stream for a normal end, and logs it. Non-streaming responses get no idle limit.
- Applies to every Claude integration in proxy mode, single- or multi-account; not to other hosts.
- A stall is not a limit: no switch, no retry (the request may already have been processed).

## API

Builder decides paths and payloads; at minimum:

- List response: a Claude integration carries `accounts: [{id, account, source, position, active, status,
  lastError, usage: {fiveHourPct, fiveHourResetsAt, weekPct, weekResetsAt, limitedUntil, source, at}}]`. Never a
  token or placeholder.
- Add account (sign-in start/finish and setup-token paste, extending the existing endpoints with the target
  integration), sign in again / replace per account, delete account, activate account, set order.
- Permissions: all of these are `integration:set` on the integration's scope object (always the member's own
  `org/user-integrations/<id>`); listing is `integration:list`. No new actions *(Our call)*.
- Audit in the same transaction: `integration:set` with details `{action: account-add | account-remove |
  account-activate | account-order | account-replace, account email}`. Automatic switches are logged (integration,
  from, to, limited until) and recorded on the account rows, not audited *(Our call: the system acted, not a
  member)*.

## UI

Integrations page, the private Claude integration expands into its account rows:

```
Claude · private · proxy                                                  [+ Add account]
 ⠿  me@work.example       5h ████████░░ 82 % · resets 14:30   week ███░░░ 31 %   live     [● on ]
 ⠿  me@private.example    5h █░░░░░░░░░  9 % · resets 17:05   week █░░░░░  6 %   2 min    [○ off]
```

- Row-based like the VM list (`ResourceList`/`ResourceRow`), drag handle on the left; native HTML drag and drop, plus
  keyboard reordering on the handle (up/down) for accessibility *(Our call: no new dependency)*.
- Per row: email, both usage bars with reset time, data age, status (`expired` → "Sign in again"), switch, and a
  menu with Sign in again / Remove.
- "Switched automatically at 13:12: me@work.example reached its limit" note on the integration after an automatic
  switch, until the next manual action *(Our call)*.
- Single-account integrations (all org/team ones, private with one account) look as today, plus the usage bars for
  Claude `oauth`.
- de + en.

## Failure semantics

| Failure | Behaviour |
|---|---|
| Usage endpoint unreachable, changed or refused | Last values kept, row shows data age and error; proxy headers still drive the switch. |
| Limit headers missing or changed | No retry, no switch from the proxy; the 429 reaches Claude Code; polling switches within a minute. |
| Retry account's access token expired on the node (hub down for hours) | Its 401 goes to Claude Code; no further retry. |
| Report to the hub lost (node restart, hub down) | Proxy keeps using the account its retry found; hub catches up via polling. |
| Stream silent for 120 s | Aborted with an error event; Claude Code retries as on any API error. |
| All accounts limited | The last 429 reaches Claude Code; page shows the earliest reset. |
| Hub down | Proxy keeps the last tokens and retries locally until their access tokens expire. |
| Active token revoked at the provider (401 on every request) | No switch; `expired` only after the next refresh fails with `invalid_grant` (setup tokens: never). Known limit *(Our call: add a 401 trigger when it bites)*. |

## Security

- Refresh tokens of all accounts stay sealed in the hub. Access tokens (hours) of the member's non-expired accounts
  reach only nodes that run a VM of that member, in node memory, as part of the integration's desired entry; never
  the guest, a log, `vm.json` or the seed.
- The account id sent with each token and returned in usage reports is not secret material.
- The proxy scrubs every account's token of the integration from responses, not only the active one.
- Buffered request bodies live only in proxy memory for the duration of the request.
- Usage figures are shown only to holders of `integration:list` on the scope (the member).

## Left to the builder

- The usage endpoint of Claude Code (URL, headers, fields, scopes); verified against the current Claude Code before
  building, with a fake for tests. The header semantics are taken from `claude-router`
  (`internal/proxy/limit.go`, `limits.go`, `idle.go`, `handler.go`), which the member runs in production.
- Migration details for existing Claude rows, REST paths and payloads.
- How the multi-account values reach the proxy (e.g. `HostBoundSecret` carrying an ordered list of `{account,
  value}`), how the proxy hooks retry and watchdog into `httputil.ReverseProxy` (e.g. a retrying `Transport` for the
  bound host), and where the node throttles reports.
- Test strategy; must include: confirmed-limit detection (status, overage, reset/retry-after, clamp; 429 without
  headers passes through); retry with the next account and chain through several; body over 32 MiB not retried;
  no retry for single-account integrations; exactly one active account under concurrent activate/auto-switch;
  auto switch skips limited and expired; no switch back before `limited_until`; manual activation of a limited
  account sticks; stale report for a no-longer-active account does not switch; placeholder unchanged across
  switches; tokens of a member only on nodes running their VMs; watchdog fires on a silent stream and appends the
  error event, not on non-streaming responses; "Add account" refused for org/team scope and in `vm` mode; mode
  change refused with more than one account; migration keeps existing integrations working; no token in API
  responses, audit, logs or usage reports.
- Docs: update `docs/integrations.md` (accounts, usage, switching, watchdog, dependency on undocumented endpoints
  and headers) and `docs/egress.md` (retry and watchdog for Claude).

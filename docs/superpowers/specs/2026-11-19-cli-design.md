# CLI

## Intent

Project 8 of the VM port (roadmap in `2026-11-17-vms-nodes-design.md`; projects 1–7 are built). Today `cmd/observatory` can only `login` (OIDC device flow) and `whoami`; everything else needs the web UI. This project turns it into a CLI that does from a terminal what the web UI does: create, start, stop, hibernate and delete VMs, connect to them over SSH and open their desktop, take and restore snapshots, fork, manage disks, networks, devices, variables, secrets and integrations, and look after nodes, teams, members and roles. It is a thin client of the existing REST API: anything the CLI can do, the API already allows, with the same permissions and audit.

Success: on a laptop with a device tunnel up, a member runs

```
observatory login --hub https://hub.example
observatory vm create dev --wait
observatory ssh dev
```

and is in the VM. `observatory vm list --json | jq` gives the API's JSON. `printf %s "$TOKEN" | observatory integration connect github` connects GitHub without the token ever being an argument. `observatory vm delete dev` asks before deleting, `--yes` does not. A script sees exit code 5 for a VM that does not exist and 6 when `--wait` times out. All tests run without a hub, a network or real credentials.

## Decided with the user

- One CLI that controls everything: VMs (create, list, start, stop, …), connecting, and the rest of what the web UI offers.
- No further questions: every other decision is made by best practice and marked *(Our call.)*

## Goals

- **Full coverage of the member-facing API**: VMs incl. power, auto-suspend, networks, disks, snapshots, fork, metrics, egress and integrations per VM; disks; networks; devices; SSH keys; variables and secrets; integrations incl. Claude sign-in; nodes incl. install commands; orgs, teams, members, invites, roles, bindings, limits, sessions, audit.
- **Connect**: `observatory ssh <vm>` and `observatory desktop <vm>` over the existing device/WireGuard path and `<vm>.internal`.
- **Scriptable**: `--json`, stable exit codes, `--wait` with a timeout, `--yes`, no prompts when stdin is not a terminal.
- **Safe with secrets**: secret values and tokens come from stdin or a file, never from argv; the CLI session token lives in one 0600 file; a WireGuard private key is written once and never kept by the CLI.
- **Names, not IDs**: every resource can be named by its name or its ID.
- **Hermetic tests** against an `httptest` fake hub with golden output.

## Non-goals

- New API endpoints *(Our call: none needed; see "API gaps")*.
- CPU/RAM resize of a VM: the API and the web UI have none; it is a lifecycle feature, not a CLI one *(Our call)*.
- Platform-admin commands (`/api/admin/*`: all orgs, all users, platform-admin flag, org limits as platform admin) *(Our call: operators use the web UI; each is one table row later)*.
- Bringing the WireGuard tunnel up (`wg-quick` needs root and differs per OS) — the CLI writes the config and says how to import it.
- Shell completion *(Our call: the command table makes a static `observatory completion zsh|bash` easy later; nobody needs it for this project)*.
- Windows support for `ssh`/`desktop` (they `exec` the system `ssh`; other commands build on Windows but are not tested there).
- An interactive TUI, a config file for defaults beyond hub and org, plugins, aliases.
- A Go client library for other programs: the REST API is the contract.

## Anti-goals

- A secret value, integration token, Claude sign-in code, device private key or CLI session token in argv, a log line, an error message or the output of a command other than the one that exists to show it once (`device create`).
- Storing a device's WireGuard private key or SSH private keys anywhere: `device create` writes the config to the path asked for, once, 0600, and forgets it.
- A destructive command that runs without confirmation when nobody passed `--yes` — including in a pipe (refuse instead of assuming yes).
- A CLI that bypasses a permission check or audit: it only calls `/api/...` with the user's own CLI session.
- Tests that touch the real `$HOME`, a real hub, the network beyond `httptest`, or a real `ssh`.

## Approaches

- **A) Standard-library `flag`, noun-verb dispatch from one command table, own small client in `cmd/observatory` (picked).** Fits the existing ~320 lines (already `flag`), no new module, everything the CLI needs (subcommands, help from the table, interspersed flags) is ~80 lines. Response types are small structs with the fields the CLI shows; `--json` passes the API's own body through, so nothing is lost.
- B) `spf13/cobra` (+ pflag). Gives help, completion and nested commands for free, but adds two modules for features we either do not need now (completion) or get from a table in a few lines, and its conventions (persistent flags, `RunE` trees) would be the largest part of the binary's code surface. Worth it only with dozens of nested levels or generated docs.
- C) Generate a client from an OpenAPI description. There is none; writing one for ~100 handlers is a project of its own and still leaves the CLI to be written.

## Command layout

`observatory <noun> <verb> [args] [flags]` *(Our call: noun-verb like `gh`, `kubectl`-style resources read naturally and group help by resource)*. Top-level shortcuts only where a verb has no natural noun: `login`, `logout`, `whoami`, `ssh`, `desktop`, `help`, `version`. Flags may appear anywhere after the noun (`observatory vm stop dev --yes` works; the CLI reorders, since `flag` stops at the first positional).

**Global flags** (on every command): `--hub URL`, `--org NAME|ID`, `--json`, `--yes`/`-y`, `--help`.

| Command | API | Notes |
|---|---|---|
| `login [--hub URL]` | `/api/auth/config`, `/api/auth/cli-token` | existing device flow; stores hub, token, expiry |
| `logout` | `POST /auth/logout` | revokes the CLI session, removes the token (keeps hub and org) |
| `whoami` | `GET /api/me` | user, orgs with role, current org, token expiry |
| `version` | — | `internal/version` |
| `org list` / `org use <org>` / `org show` | `GET /api/me`, `GET /api/orgs/{org}` | `use` stores the default org |
| `org create <name>` / `org rename <name>` | `POST /api/orgs`, `PATCH /api/orgs/{org}` | |
| `org audit [--limit N] [--action A] [--actor ID] [--since DURATION]` | `GET /api/orgs/{org}/audit` | `--since` → `from`; `--json` prints `next` for paging |
| `vm list` / `vm show <vm>` | `GET …/vms`, `GET …/vms/{id}` | show includes disks, networks, auto-suspend, image |
| `vm create <name> [--node N] [--team T] [--cpus 2] [--memory 4G] [--disk 20] [--wait]` | `POST …/vms` | node defaults to the org's only node |
| `vm start\|resume\|pause\|hibernate <vm> [--wait]` | `POST …/vms/{id}/<action>` | |
| `vm stop <vm> [--force] [--wait]` | `…/shutdown`, `--force` → `…/stop` | |
| `vm restart <vm> [--force] [--wait]` | stop/shutdown, wait `stopped`, `…/start` | composed client-side |
| `vm delete <vm> [--yes] [--wait]` | `DELETE …/vms/{id}` | confirms |
| `vm auto-suspend <vm> <minutes\|off>` | `PUT …/vms/{id}/auto-suspend` | 5–1440 or off |
| `vm metrics <vm>` / `vm egress <vm>` / `vm integrations <vm>` | `GET …/metrics`, `…/egress`, `…/integrations` | |
| `ssh <vm> [--user ubuntu] [-- ssh args…]` | `GET …/vms` | execs `ssh ubuntu@<vm>.internal …` |
| `desktop <vm> [--user ubuntu]` | `GET …/vms` | `ssh … vmm-desktop start`, then execs `xpra attach ssh://…/100` |
| `snapshot list <vm>` | `GET …/vms/{id}/snapshots` | |
| `snapshot create <vm> <name> [--memory] [--wait]` | `POST …/snapshots` | |
| `snapshot restore <vm> <snap> [--memory] [--restart] [--yes] [--wait]` | `POST …/snapshots/{sid}/restore` | confirms |
| `snapshot delete <vm> <snap> [--yes]` | `DELETE …/snapshots/{sid}` | confirms |
| `snapshot fork <vm> <snap> <new-vm> [--memory] [--wait]` | `POST …/snapshots/{sid}/fork` | "new VM from snapshot" |
| `disk list` / `disk show <disk>` | `GET …/disks`, `…/disks/{id}` | |
| `disk create <name> --size GB [--team T] [--wait]` | `POST …/disks` | |
| `disk copy <disk> <new> [--team T] [--wait]` / `disk grow <disk> --size GB [--restart]` | `…/copy`, `…/grow` | |
| `disk release <disk> [--yes]` / `disk delete <disk> [--yes]` | `…/release`, `DELETE` | confirm |
| `disk attach <disk> <vm> [--mount /mnt/<disk>] [--restart]` / `disk detach <disk> <vm> [--restart]` | `PUT …/vms/{id}/disks` | read-modify-write of the VM's list |
| `network list` / `network create <name> [--subnet CIDR]` / `network delete <net> [--yes]` | `…/networks` | |
| `network attach <net> <vm> [--restart]` / `network detach <net> <vm> [--restart]` | `PUT …/vms/{id}/networks` | read-modify-write |
| `device list` / `device delete <device> [--yes]` | `…/devices` | |
| `device create <name> [--out FILE\|-]` | `POST …/devices` | writes the WireGuard config once, 0600 |
| `ssh-key list` / `ssh-key add [--name N] [FILE]` / `ssh-key delete <key> [--yes]` | `/api/me/ssh-keys` | FILE defaults to `~/.ssh/id_ed25519.pub`, then `id_ecdsa.pub`, `id_rsa.pub` |
| `var list [--scope …]` | `GET …/env` | secret values never shown |
| `var set <NAME> [--secret] [--host H]… [--scope private\|team\|org] [--team T] [--value V \| --from-file F]` | `POST`/`PATCH …/env` | upsert by name and scope; secrets only from stdin/file |
| `var move <NAME> --to private\|team\|org [--to-team T]` / `var delete <NAME> [--yes]` | `…/env/{id}/move`, `DELETE` | |
| `integration list` | `GET …/integrations` | |
| `integration connect github\|linear\|claude [--scope …] [--mode proxy\|vm] [--from-file F]` | `POST …/integrations` | token from stdin/file |
| `integration sign-in claude [--scope …] [--mode …]` | `POST …/claude/sign-in`, `…/finish` | prints URL, reads the code from stdin |
| `integration replace <kind> [--scope …]` / `integration mode <kind> proxy\|vm` / `integration disconnect <kind> [--yes]` | `PUT …/token`, `PATCH`, `DELETE` | |
| `node list` / `node show <node>` | `GET …/nodes` | image state included |
| `node add <name>` / `node token <node>` | `POST …/nodes`, `POST …/nodes/{id}/credential` | print the install command |
| `node rename <node> <name>` / `node delete <node> [--yes]` | `PATCH`, `DELETE` | |
| `team list` / `team show <team>` / `team create <name>` / `team rename <team> <name>` / `team delete <team> [--yes]` | `…/teams` | |
| `team add <team> <member> [--admin]` / `team remove <team> <member> [--yes]` | `PUT`/`DELETE …/teams/{t}/members/{u}` | member by email, name or ID |
| `member list` / `member role <member> owner\|admin\|member` / `member remove <member> [--yes]` | `…/members` | |
| `invite list` / `invite create [--role member]` / `invite revoke <id> [--yes]` / `invite accept <token-or-url>` | `…/invites`, `/api/invites/accept` | create prints the URL once |
| `role list` / `role show <role>` / `role catalog` / `role create --file F` / `role update <role> --file F` / `role delete <role> [--yes]` | `…/roles`, `…/catalog` | rules as JSON, same shape as the API |
| `binding list` / `binding add <subject> <role>` / `binding delete <id> [--yes]` | `…/bindings` | subject as in the API (`user:<id>`, `team:<id>`); `user:<email>` resolved |
| `limit show` / `limit set <org\|team:T\|user:M> [--vcpu N] [--ram MB] [--disk GB] [--vms N] [--snapshots N]` | `…/limits` | `none` clears a value |
| `session list` / `session revoke <id> [--yes]` | `/api/me/sessions` | |

## Context: hub, org, team

- **Hub URL** *(Our call)*: `--hub` > `OBSERVATORY_HUB` > the hub stored at login > `http://localhost:8090`. `login --hub` stores it; trailing slashes trimmed.
- **Org** *(Our call)*: `--org` > `OBSERVATORY_ORG` > the default stored by `org use` > the user's only org. With several orgs and none chosen the command fails with exit 2 and lists them with the `org use` hint. Matched by ID or exact name against `GET /api/me`; the API path always uses the ID.
- **Team** *(Our call)*: never a stored default. A team is chosen per command (`--team`), because a remembered team would silently make new VMs, disks, variables and integrations team-owned and visible to the whole team. The default owner is "me", the default scope for variables and integrations is `private` — the narrowest.
- **Scopes** in flags are `private`, `team`, `org` (the API's `user` is shown as `private`, matching the web UI). `--team T` alone implies `--scope team`.

## Configuration and credentials

- **One file** `config.json` in the config directory *(Our call: hub, token, token expiry and default org are one user's settings; splitting secrets from settings buys nothing when both are 0600)*: `$XDG_CONFIG_HOME/observatory/` if set, else `os.UserConfigDir()/observatory/` (`~/.config/observatory` on Linux, `~/Library/Application Support/observatory` on macOS). Directory 0700, file 0600, written via temp file + rename and `chmod` (so an older looser file is tightened). An existing `credentials.json` from the old CLI is read once and replaced by `config.json`.
- **Token from the environment** *(Our call)*: `OBSERVATORY_TOKEN` overrides the file and is never written, for CI that copies a CLI session token; it never comes from a flag.
- **Token lifetime** *(Our call)*: the hub issues 30-day CLI sessions and has no refresh endpoint; the CLI stores `expiresAt` from `/api/auth/cli-token`, warns on stderr when fewer than 3 days remain, and on `401` says "session expired or revoked, run observatory login" with exit 3. No new refresh endpoint: a monthly device-flow login is the same cost as the browser's weekly one, and a refresh token would be one more long-lived secret on disk.

## Output

- **Human output** *(Our call)*: `text/tabwriter` tables with upper-case headers for lists, `key: value` lines for `show`, one short sentence for actions (`VM dev started.`). Times in local time, relative for the last hour; sizes as `4 GB`. Tables are golden-tested.
- **`--json`** *(Our call)*: the API's response body unchanged (indented); composed commands print the final resource (e.g. the VM after `restart --wait`); actions with `204` print nothing. Scripts get the REST contract, the CLI adds none of its own.
- **stderr** carries prompts, warnings, progress of `--wait` (only on a terminal) and errors (`observatory: <message>`); stdout only the result.

## Errors and exit codes

*(Our call)*

| Code | When |
|---|---|
| 0 | success |
| 1 | any other failure (API 4xx/5xx not below, network error, VM ended in `error` while waiting) |
| 2 | usage: unknown command or flag, wrong number of arguments, no org chosen, confirmation needed but stdin is not a terminal |
| 3 | not logged in, or `401` |
| 4 | `403` |
| 5 | not found: `404`, or a name that matches nothing |
| 6 | `--wait` timed out |

The API's `{"error": "…"}` message is shown as is. `ssh` and `desktop` exit with the code of the program they exec.

## Confirmation

*(Our call)* Destructive commands — `vm delete`, `snapshot restore`, `snapshot delete`, `disk delete`, `disk release`, `network delete`, `device delete`, `ssh-key delete`, `var delete`, `integration disconnect`, `node delete`, `team delete`, `team remove`, `member remove`, `invite revoke`, `role delete`, `binding delete`, `session revoke` — ask `Delete VM dev? [y/N]` on a terminal. With `--yes` they do not ask; without a terminal and without `--yes` they fail with exit 2 ("pass --yes to confirm"). `vm stop --force` (hard stop) and attach/detach with `--restart` do not ask: they lose no data that a reboot would keep, and `--restart` is already the explicit consent the API asks for.

## Waiting

*(Our call)* Commands that start async work return as soon as the API accepted it; `--wait` polls the resource every 2 s until a target state, `--timeout` (default `5m`) bounds it.

| Command | Done when | Fails when |
|---|---|---|
| `vm create`, `start`, `resume`, `restart`, `snapshot fork` | `state == running` | `state == error` (shows `error`) |
| `vm stop` | `stopped` | `error` |
| `vm pause` / `hibernate` | `paused` / `hibernated` | `error` |
| `vm delete` | `GET` returns 404 | — |
| `snapshot create` | snapshot `state == ready` | snapshot `state == error` (shows `error`) |
| `snapshot restore` | VM back in the power state it had (running → `running`) | `error` |
| `disk create` / `copy` | disk state ready (exact values from `internal/api/disks.go`) | disk error |

`vm restart` always waits for `stopped` between the two calls (bounded by `--timeout`); `--wait` then also waits for `running`. On a terminal, `--wait` prints the state when it changes, on stderr.

## Connect

- **`observatory ssh <vm>`** *(Our call)*: resolves the VM in the current org, then `exec`s the system `ssh` (`syscall.Exec`, so the terminal, agent forwarding, `~/.ssh/config`, `known_hosts` and the exit code are plain `ssh`'s) as `ssh <user>@<dnsName> [args after --]`, user `ubuntu` by default. It uses the VM's `dnsName` (`<vm>.internal`), so `known_hosts` entries survive IP changes; the management IP only when `dnsName` is empty. Checks before exec: VM `stopped`, `paused` or `hibernated` → exit 1 with "run observatory vm start <vm>" (a `suspended` VM wakes on the first packet, so it is allowed); no device of the user in this org → a warning on stderr pointing at `device create` (not fatal: the user may reach the gateway another way).
- **The CLI holds no keys**: authentication is the user's own SSH key (registered with `ssh-key add`, delivered by the hub at boot) and the device tunnel; the CLI never generates, stores or forwards a private key.
- **`observatory desktop <vm>`**: runs `ssh <user>@<host> vmm-desktop start` and on success `exec`s `xpra attach ssh://<user>@<host>/100`. `xpra` is looked up in `PATH`, then `/Applications/Xpra.app/Contents/MacOS/Xpra`; missing → exit 1 with the install hint from `docs/guest.md`.
- **`device create`** writes the config returned by the API to `--out` (default `./<name>.conf`) with `O_EXCL` and mode 0600 — never overwrites — or to stdout with `--out -`, then prints `sudo wg-quick up ./<name>.conf` or "import it in the WireGuard app". The private key exists only in that file.

## Secret input

*(Our call)*

- `var set --secret`, `integration connect`, `integration replace`, the Claude sign-in code and `invite accept`'s token read the value from `--from-file F` (`-` = stdin) or, without the flag, from stdin: on a terminal with a prompt and echo off (`golang.org/x/term`), otherwise all of stdin with one trailing newline removed. At most 64 KiB; empty → error.
- `--value V` exists only for plain variables (`env` kind), whose values the API shows to readers anyway; for `--secret` it is refused with "pass the value on stdin".
- Values never appear in errors, `--json` output of the CLI's own making, or the process title.

## Name resolution

*(Our call)* Every `<vm>`, `<disk>`, `<net>`, `<node>`, `<team>`, `<role>`, `<device>`, `<key>` argument is matched first as an exact ID, then as an exact name, by listing the collection (the API addresses by ID only). VM, disk and node names are unique per org (database constraints); for other kinds two matches are an error listing both IDs. Members match by ID, email or name. Variables and integrations are matched by name/kind plus scope (and team). One extra `GET` per command is accepted; there is no cache.

## API gaps

*(Our call: no new endpoint in this project)*

| Need | Decision |
|---|---|
| Restart | Composed: shutdown (or stop), wait `stopped`, start. |
| Lookup by name | Client-side from the list endpoints. |
| Token refresh | None; 30-day sessions, warning before expiry, `login` again. |
| CPU/RAM resize | Not in the API or UI; non-goal. |
| Upsert of variables | Client-side: list, then `PATCH` or `POST`. |
| Attach/detach one disk or network | Client-side read-modify-write of the VM's full list (`PUT`); a concurrent change by someone else can be overwritten, exactly as in the web UI's dialogs. |
| Desktop tunnel through the hub | Not built (guest-experience spec); the CLI uses SSH. |

## Dependency

- **`golang.org/x/term`** becomes a direct requirement for echo-off secret prompts and terminal detection. It is the Go team's module and already in `go.sum` through `golang.org/x/crypto`, so no new third-party code. Everything else is standard library plus the existing `go-oidc` and `x/oauth2` for login *(Our call)*.

## Testing

- **Fake hub**: `httptest.Server` with a route table (method + path → handler); unknown routes fail the test, every request's method, path, auth header and body is recorded so tests assert exactly what was called. Response fixtures copy the JSON shapes of `internal/api` (field names from the handlers).
- **Hermetic**: each test gets `HOME` and `XDG_CONFIG_HOME` under `t.TempDir()`, `OBSERVATORY_*` cleared, the CLI's `getenv`, stdin, stdout, stderr, terminal flag, poll interval, `exec` and `lookPath` injected; `ssh`/`xpra` are never run (the fake `exec` records argv). No real credentials: tokens are `tok-test`.
- **Golden output** for every list/show table and the help text in `cmd/observatory/testdata/*.golden`, regenerated with `go test ./cmd/observatory -update`.
- **Must-haves**: secret values never in argv-derived output, errors or the request log; `config.json` 0600 / dir 0700 even when a looser file existed; `--json` equals the API body; every exit code in the table; destructive command without terminal and `--yes` makes no request; `--wait` timeout → 6 and `error` state → 1; device config written once with 0600 and never overwritten; `ssh` argv for running, suspended and stopped VMs; org selection precedence; read-modify-write of disks/networks keeps the other entries.
- Run with `make test`.
- **Real run** against a dev hub (test users with dummy credentials, nodes on test VMs 108 = org A and 107 = org B): the full lifecycle by CLI (see the plan's last task) and `docs/cli.md`.

## Left to the builder

- Exact table columns per list (follow the web UI's lists), help wording, relative-time format.
- Exact ready/failed states for disks, read from `internal/disks` and `internal/api/disks.go`.
- Whether `--json` for list commands that return wrappers (`{"vms": …}`) prints the wrapper as is (default) — it does, unless the builder finds a reason in the real run.
- `docs/cli.md`: install (`make build` → `bin/observatory`), login, org selection, every command group with an example, exit codes, `--json` recipes, connecting (device, SSH key, `ssh`, `desktop`), secrets input, config file location; link from `docs/network.md` (devices), `docs/guest.md` (desktop), `docs/integrations.md` (connect from the CLI).

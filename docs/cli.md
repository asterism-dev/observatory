# CLI

`observatory` does from a terminal what the web UI does: VMs and their power states, SSH and desktop, snapshots and
forks, disks, networks, devices, variables, secrets, integrations, nodes, teams, members and roles. It is a thin client
of the hub's REST API (`/api/...`) and of the iam API (`/api/iam/v1/`, see [permissions](permissions.md#iam-api)) with
your own session, so every command has the same permissions and audit as the web UI.

```
observatory login --hub https://hub.example
observatory vm create dev --wait
observatory ssh dev
```

## Install

Download `observatory_<version>_<os>_<arch>.tar.gz` from the
[latest release](https://github.com/asterism-dev/observatory/releases/latest), check it against `SHA256SUMS`, and put
`observatory` somewhere on your `PATH`. From a checkout, `make cli` builds `bin/observatory` instead.
`observatory version` prints the build version.

## Updates

Once a day the CLI asks GitHub for the latest release; when it is newer, commands print a one-line hint on stderr.
`observatory update` downloads the release for this OS and architecture, verifies it against `SHA256SUMS` and replaces
the binary in place (with `sudo` if it lives in a root-owned directory). `OBSERVATORY_NO_UPDATE_CHECK=1` turns the
daily check off; builds that are not a release tag never check and cannot update themselves.

## Login and sessions

`observatory login --hub URL` runs the browser device flow: it prints a URL and a code, you approve it in the browser,
and the CLI stores the hub, a CLI session token and its expiry. CLI sessions last 30 days and are not refreshed; fewer
than 3 days before the end every command warns on stderr, and after it (or after a revoke) commands fail with
"session expired or revoked, run observatory login" (exit 3).

- `observatory whoami`: user, orgs with your role, and where the session comes from.
- `observatory logout`: revokes this CLI session at the hub and removes the token (hub and default org stay). With
  `OBSERVATORY_TOKEN` set it revokes that session and keeps the stored one.
- `observatory session list` / `session revoke <id>`: your sessions (web and CLI); `*` marks the current one.
- `OBSERVATORY_TOKEN`: a CLI session token for CI. It overrides the file and is never written to it; it never comes
  from a flag.
- The stored token is only sent to the hub it was issued for: with `--hub` or `OBSERVATORY_HUB` naming another hub,
  commands fail with exit 3 until you log in there.

## Hub and org selection

| Setting | Precedence |
|---|---|
| Hub | `--hub` > `OBSERVATORY_HUB` > the hub stored by `login` > `http://localhost:8090` |
| Org | `--org` > `OBSERVATORY_ORG` > the default stored by `org use` > your only org |

With several orgs and none chosen, a command fails with exit 2 and lists them; `observatory org use <org>` stores the
default. Orgs are matched by ID or exact name. There is no stored default team: pass `--team T` per command, because a
remembered team would silently make new VMs, disks, variables and integrations team-owned. Without it, VMs and disks
belong to you and variables and integrations are `private`. `--team T` alone implies `--scope team`.

Flags go after the full command path: `observatory vm list --org orga`, not `observatory --org orga vm list`.

## Command reference

`observatory help` (or `observatory help <noun>`) prints the same list. Every `<vm>`, `<disk>`, `<net>`, `<node>`,
`<team>`, `<role>`, `<device>`, `<key>` is matched as an ID first, then as an exact name; members by ID, email or name.

Global flags: `--hub URL`, `--org NAME|ID`, `--json`, `--yes`/`-y`, `--timeout DURATION` (default `5m`), `--help`.

### Session and connecting

| Command | Does |
|---|---|
| `login [--hub URL]` | log in with your browser (device flow) |
| `logout` | revoke this CLI session |
| `whoami` | show the user, orgs and session |
| `version` | print the CLI version |
| `help [noun]` | show the help |
| `ssh <vm> [--user dev] [-- args...]` | ssh into a VM |
| `desktop <vm> [--user dev]` | start the VM desktop and attach with xpra |
| `ssh-key list` / `ssh-key add [FILE] [--name N]` / `ssh-key delete <key>` | your SSH public keys (default `~/.ssh/id_ed25519.pub`, then `id_ecdsa.pub`, `id_rsa.pub`) |
| `device list` / `device create <name> [--out FILE\|-]` / `device delete <device>` | WireGuard devices |

### Orgs

| Command | Does |
|---|---|
| `org list` / `org use <org>` / `org show` | your orgs; set the default; the current org and your permissions |
| `org create <name>` / `org rename <name>` | create an org; rename the current one |
| `org audit [--limit N] [--action A] [--actor ID] [--since 24h]` | the audit log from the audit service (`--json` prints its response, `nextPageToken` for paging) |

### VMs

| Command | Does |
|---|---|
| `vm list [--deleted]` / `vm show <vm>` | list (`--deleted`: [deleted VMs](vm-lifecycle.md#history)); show incl. disks, networks, auto-suspend, delete-at, image |
| `vm create <name> [--node N] [--team T] [--image REF] [--cpus 2] [--memory 4G] [--disk 20] [--delete-after 2d] [--wait]` | create a VM (node defaults to the org's only node; image defaults to the owner's [default image](images.md#default-image); `--delete-after` takes `30m`, `1h`, `2d` or an RFC 3339 time) |
| `vm start\|resume\|pause\|hibernate <vm> [--wait]` | power actions |
| `vm stop <vm> [--force] [--wait]` | graceful shutdown; `--force` powers off |
| `vm restart <vm> [--force] [--wait]` | stop, wait for `stopped`, start |
| `vm delete <vm> [--wait]` | delete (confirms) |
| `vm auto-suspend <vm> <minutes\|off>` | suspend after 5–1440 idle minutes |
| `vm delete-after <vm> <30m\|1h\|2d\|time\|off>` | [delete the VM automatically](vm-lifecycle.md#delete-after) at that time; `off` clears it |
| `vm metrics <vm>` / `vm egress <vm>` / `vm integrations <vm>` | latest CPU/RAM/disk usage; egress proxy hosts and CA; integrations the VM gets |
| `snapshot list <vm>` | snapshots of a VM |
| `snapshot create <vm> <name> [--memory] [--wait]` | snapshot (`--memory` includes RAM) |
| `snapshot restore <vm> <snap> [--memory] [--restart] [--wait]` | roll back (confirms; a running VM needs `--restart`) |
| `snapshot delete <vm> <snap>` | delete a snapshot (confirms) |
| `snapshot fork <vm> <snap> <new-vm> [--memory] [--wait]` | new VM from a snapshot |

### Disks and networks

| Command | Does |
|---|---|
| `disk list` / `disk show <disk>` | list; show |
| `disk create <name> --size GB [--team T] [--wait]` / `disk copy <disk> <new> [--team T] [--wait]` | create; copy |
| `disk grow <disk> --size GB [--restart]` | grow |
| `disk release <disk>` / `disk delete <disk>` | force-release from its VM; delete (both confirm) |
| `disk attach <disk> <vm> [--mount PATH] [--restart]` / `disk detach <disk> <vm> [--restart]` | mount defaults to `/mnt/<disk>` |
| `network list` / `network create <name> [--subnet CIDR]` / `network delete <net>` | delete confirms |
| `network attach <net> <vm> [--restart]` / `network detach <net> <vm> [--restart]` | |

### Variables, secrets and integrations

| Command | Does |
|---|---|
| `var list [--scope private\|team\|org] [--team T]` | secret values are never shown |
| `var set <name> [--secret] [--host H]... [--scope S] [--team T] [--value V \| --from-file F]` | create or update by name and scope |
| `var move <name> [--scope S] [--team T] --to private\|team\|org [--to-team T]` | move to another scope |
| `var delete <name> [--scope S] [--team T]` | delete (confirms) |
| `integration list` | integrations you can see |
| `integration connect github\|linear\|claude [--scope S] [--team T] [--mode proxy\|vm] [--from-file F]` | connect with a pasted token |
| `integration connect gcloud [--quota-project P] [--from-file F]` | hand over the local `gcloud auth application-default login` (default `$CLOUDSDK_CONFIG/application_default_credentials.json`); replaces the token if gcloud is already connected; private scope, proxy mode |
| `integration sign-in claude [--scope S] [--team T] [--mode proxy\|vm] [--replace]` | prints a URL, reads the code from stdin |
| `integration replace <kind> [--scope S] [--team T] [--from-file F]` | replace the token |
| `integration mode <kind> proxy\|vm [--scope S] [--team T]` | change how the token reaches VMs |
| `integration disconnect <kind> [--scope S] [--team T]` | disconnect (confirms) |

### Images

See [images.md](images.md). `<ref>` is `name`, `name:tag`, `name@sha256:…` or qualified (`me/`, `<team>/`, `org/`).

| Command | Does |
|---|---|
| `image list` / `image show <ref>` | images you can see; one image with tags and builds |
| `image create <name> [--scope S] [--team T] [--description D]` | create an image (private unless `--scope`/`--team`) |
| `image update <ref> [--name N] [--description D]` | rename an image or change its description |
| `image delete <ref>` | delete an image and its versions (confirms) |
| `image visibility <ref> --scope private\|team\|org [--team T]` | change who sees the image; widening deletes its versions (confirms) |
| `image recipe pull <ref> <dir>` | download the current revision; prints its `--ref` flags |
| `image recipe push <ref> <dir> [--ref secret:SCOPE/NAME]... [--ref integration:SCOPE/KIND]...` | upload a directory as the next revision; scope `org`, `private` or `team:<team>` |
| `image build <ref> [--follow]` | build the current recipe; `--follow` streams the log |
| `image auto <ref> [--schedule off\|daily\|weekly] [--on-base-change=true\|false] [--approve]` | automatic rebuilds on a schedule or when a base image changes; turning them on or `--approve` approves the current recipe revision; without flags shows the settings |
| `image builds <ref>` / `image log <build-id> [--follow]` | builds of an image (automatic ones marked); a build's log |
| `image cancel <build-id>` | cancel a queued or running build |
| `image login` | set docker's credential helper for the hub registry |
| `image push <ref>[:tag] <local-image>` | tag and push a local image with `docker` (tag defaults to `latest`) |
| `image tag <ref> <tag> <digest>` / `image tag delete <ref> <tag>` | point a tag at a version; remove a tag (`latest` cannot be set or removed by hand) |
| `image versions <ref>` / `image versions delete <ref> <digest>` | list versions; delete a version and its tags (confirms) |
| `image default set <org\|user\|team:T> <ref>` / `image default show <level>` / `image default clear <level>` | default image of a level |
| `image builders` | running builder VMs of the org (admins) |

### Nodes, teams, members and roles

| Command | Does |
|---|---|
| `node list` / `node show <node>` | incl. image state |
| `node add <name>` / `node token <node>` | register a node / issue a new install command; both print it. `token` revokes the current credential, so an enrolled node disconnects until reinstalled (confirms) |
| `node rename <node> <name>` / `node delete <node>` | delete confirms |
| `team list` / `team show <team>` / `team create <name>` / `team rename <team> <name>` / `team delete <team>` | delete confirms; `me`, `org` and `observatory` are reserved names |
| `team add <team> <member> [--admin]` / `team remove <team> <member>` | remove confirms; removing yourself leaves the team |
| `member list` / `member role <member> owner\|admin\|member` / `member remove <member>` | remove confirms; removing yourself leaves the org |
| `invite list` / `invite create [--role member]` / `invite revoke <id>` | create prints the link once; revoke confirms |
| `invite accept [--from-file F\|-]` | join an org; the token or link comes from stdin or the file |
| `role list` / `role show <role>` / `role catalog` | roles, their rules, and the permissions a rule can name |
| `role create --file F` / `role update <role> --file F` / `role delete <role>` | rules as JSON, same shape as the API |
| `binding list` / `binding add <subject> <role>` / `binding delete <id>` | subject `user:<member>` or `team:<team>` (name, email or ID) |
| `limit show` | resource limits of the org, teams and members (`images` is the image storage) |
| `limit set <team:T\|user:M> [--vcpu N] [--ram MB] [--disk GB] [--vms N] [--snapshots N]` | `none` clears a value; org limits are set by a platform admin in the web UI |
| `session list` / `session revoke <id>` | revoke confirms |

## Output and `--json`

Lists are tables, `show` prints `key: value` lines, actions print one sentence. With `--json` the CLI prints the API's
response body unchanged (indented), so scripts get the REST contract; composed commands print the final resource (the
VM after `vm restart --wait`), and actions without a body print nothing. List endpoints keep their wrapper
(`{"vms": [...]}`).

The account, org, team, member, invite, role, binding, session and SSH key commands (and `whoami`) print the iam
API's JSON response: camelCase fields, the response message as the wrapper (`{"members": [...]}`, `{"team": {...}}`) and
no fields that hold their default value (`false`, `0`, `""`, empty lists). `role show --json` prints the role, which
`role create --file` and `role update --file` accept as is.

```
observatory vm list --json | jq -r '.vms[] | select(.state=="running") | .name'
observatory vm show dev --json | jq -r .state
observatory org audit --since 24h --json | jq -r '.events[] | [.type, .result] | @tsv'
```

stdout carries only the result; prompts, warnings, `--wait` progress (on a terminal) and errors
(`observatory: <message>`) go to stderr.

## Exit codes

| Code | When |
|---|---|
| 0 | success |
| 1 | any other failure: any other API error, a network error, a VM ended in `error` while waiting |
| 2 | usage: unknown command or flag, wrong arguments, no org chosen, confirmation needed but stdin is not a terminal |
| 3 | not logged in, or the session expired or was revoked (`401`) |
| 4 | forbidden (`403`) |
| 5 | not found: `404`, or a name that matches nothing (also a VM of another org) |
| 6 | `--wait` timed out |

`ssh` and `desktop` exit with the code of `ssh` / `xpra`.

## Waiting

Commands return as soon as the hub accepted the request. `--wait` polls every 2 s until the target state, bounded by
`--timeout` (default `5m`); on a terminal it prints each state change on stderr.

| Command | Done when |
|---|---|
| `vm create`, `start`, `resume`, `restart`, `snapshot fork` | VM `running` |
| `vm stop` / `pause` / `hibernate` | `stopped` / `paused` / `hibernated` |
| `vm delete` | the VM is gone |
| `snapshot create` | snapshot `ready` |
| `snapshot restore` | the restore finished and the VM is back in its power state (`suspended` counts as `running`) |
| `disk create` / `copy` | disk `ready` |

A VM or snapshot in `error` ends the wait with exit 1. Hub errors (5xx) and network errors while polling are retried
until the timeout. `vm restart` always waits for `stopped` between stop and start.

## Confirmation

Destructive commands (`vm delete`, `snapshot restore`, `snapshot delete`, `disk delete`, `disk release`,
`network delete`, `image delete`, `image versions delete`, `image visibility` (when it deletes versions), `device delete`, `ssh-key delete`,
`var delete`, `integration disconnect`, `node token` (enrolled node), `node delete`, `team delete`, `team remove`,
`member remove`, `invite revoke`, `role delete`, `binding delete`, `session revoke`) ask
`[y/N]` on a terminal. `--yes` skips the question; without a terminal and without `--yes` they make no request and exit
2 ("pass --yes to confirm"). `vm stop --force` and attach/detach with `--restart` do not ask.

## Connecting

1. **SSH key:** `observatory ssh-key add` registers `~/.ssh/id_ed25519.pub` (or `id_ecdsa.pub`, `id_rsa.pub`, or the
   file you name). The hub delivers keys at boot, so restart VMs that were running before you added the key.
2. **Device:** `observatory device create laptop` writes the WireGuard config to `./laptop.conf` (`--out FILE`, or
   `--out -` for stdout), mode 0600, and never overwrites a file. The private key exists only in that file. Bring it up
   with `sudo wg-quick up ./laptop.conf` or import it in the WireGuard app; the CLI does not manage the tunnel. See
   [network.md](network.md#devices).
3. **SSH:** `observatory ssh dev` runs `ssh dev@dev.internal` as your system `ssh`, so `~/.ssh/config`,
   `known_hosts` and agent forwarding work as usual. Arguments after `--` go to `ssh`, e.g.
   `observatory ssh dev -- uptime`. A stopped, paused or hibernated VM is refused with a hint to start it; a suspended
   VM wakes on the first packet. Without a device in the org the CLI warns but still runs `ssh`. For files, use
   `sftp dev@dev.internal` or `scp` directly, or VS Code / Zed remote access to `dev@dev.internal`; SFTP works in
   every image ([guest.md](guest.md#own-images)).
4. **Desktop:** `observatory desktop dev` runs `vmm-desktop start` over SSH and then `xpra attach
   ssh://dev@dev.internal/100`. The xpra 6 client is looked up in `PATH`, then
   `/Applications/Xpra.app/Contents/MacOS/Xpra`; install it as in [guest.md](guest.md#installing-the-xpra-client).

The CLI never generates, stores or forwards a private key.

## Secrets input

Secret variable values (`var set --secret`), integration tokens (`integration connect`, `integration replace`), the
Claude sign-in code and invite tokens never come from an argument. They are read from `--from-file F` (`-` for stdin)
or, without the flag, from stdin: on a terminal with a prompt and echo off, otherwise all of stdin with one trailing
newline removed. At most 64 KiB; empty is an error.

```
printf %s "$TOKEN" | observatory integration connect github
observatory var set NPM_TOKEN --secret --host registry.npmjs.org --from-file ~/.npm-token
observatory var set NODE_ENV --value production
```

`--value` is only for plain variables, whose values readers see anyway; with `--secret` it is refused.

## Config file

`$XDG_CONFIG_HOME/observatory/config.json`, else `~/.config/observatory/config.json` on Linux and
`~/Library/Application Support/observatory/config.json` on macOS. It holds the hub, the session token and its expiry,
and the default org. The directory is 0700 and the file 0600, written via a temp file and rename (a looser older file is
tightened). A `credentials.json` from the old CLI is read once and replaced.

## Known limits

- **No resize:** CPU and RAM of a VM cannot be changed (not in the API or the web UI).
- **Attach/detach is read-modify-write:** `disk attach|detach` and `network attach|detach` read the VM's list and `PUT`
  it back, so a concurrent change by someone else can be overwritten, as in the web UI's dialogs. With `--restart` they
  return before the VM is back; follow with `vm show` or retry `ssh`.
- **No shell completion.**
- **No Windows `ssh`/`desktop`:** they exec the system `ssh`; other commands build on Windows but are not tested there.
- **No platform-admin commands** (all orgs, all users, org limits): use the web UI.

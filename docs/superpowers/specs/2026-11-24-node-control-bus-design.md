# Node control over a message bus

## Intent

observatory should grow into a set of independent services around two central pieces: Postgres as the single source
of truth and NATS as the message bus. Every other part (hub, storage, gateway, metrics, …) should be stateless or own
only its local data, run on its own host and be updated on its own schedule. A full rewrite next to `main` was tried
and dropped as too big; this goes there step by step on `main`, each step releasable.

Roadmap:

1. **This project:** node control over NATS; the hub keeps no state the next hub instance would miss.
2. Several storage servers as independent services on the bus (the main need: more disk space than one machine has).
3. Gateway and the remaining services the same way.

The control connection comes first so storage is built on the bus once instead of on today's HTTP polling and then
moved.

## Goals

- A hub restart or update does not disconnect nodes. Nodes stay connected to NATS; VMs keep running untouched; nodes
  do not flip to `offline` in the GUI.
- Two or more hub processes can run at the same time against one Postgres and one NATS, with correct results: every
  node is reachable from every hub, background jobs run once.
- Postgres stays the only source of truth. NATS stores nothing; losing NATS state loses nothing.
- User actions on VMs reach the node as named commands with a direct reply (accepted / rejected with reason).

## Non-goals

- Storage servers, gateway, registry, telemetry stores: unchanged, they keep polling the hub over HTTP (projects 2, 3).
- Splitting the hub into separate API and worker processes.
- A NATS cluster. One NATS server; restarting it is a short control-plane gap (see Failure behaviour).
- Load balancing / rolling update tooling for several hub instances in `deploy/compose.yaml`. The hub must be correct
  with several instances; how a deployment runs them is out of scope.
- Hubs on different machines (user decision). Several hub instances run on one host only, sharing the registry volume;
  registry GC keeps running on that volume, only its push hold (`GC.PushAllowed()`, `BuildsMayStart`) moves to
  Postgres so every instance sees it. The registry becomes its own service in a later project.

- Nodes shared by several orgs (platform machines users deploy onto). Not built here, but nothing may block it:
  - org never comes before the node in a subject; if a subject carries the org, it sits after the node
    (`compute.v1.node.{node}.org.{org}.vm.{vm}.…`) and is informational, the hub takes the org from Postgres;
  - an org event reaches the nodes that run VMs of that org (looked up via the VMs), not the nodes whose `org_id` matches;
  - a sync list is built per node across all orgs.
  Placement across orgs, network isolation on shared chassis and platform-admin node updates belong to that later
  project.

## Anti-goals

- A second source of truth: no JetStream, no KV bucket, no node-held state the hub reads back as authority.
- Reads that ask the nodes live (scatter-gather). Lists come from Postgres.
- A second reconcile path on the node: commands and sync must end in the same node-side reconcile.

## Today

- Node → hub: WebSocket + yamux at the hub, one JSON control stream (`internal/fleet/conn.go:63`,
  `internal/node/agent.go:190`). Message types: `register`, `status`, `update`, `processes`, `usage`, `disks`,
  `image`, `metrics` (ignored).
- Hub → node: `pushLoop` sends the node's full desired VM list (`proto.DesiredVM`, rebuilt from Postgres on every push)
  plus underlay config / chassis cert, update requests and `processes` requests (reply matched by `ReqID`).
- Hub-process-local state that breaks a second instance:
  - `Fleet.conns` (which node is attached here), `NotifyOrg` only kicks local connections (called from ~10 API files).
  - `diskErrs`, `retrying`, `imageRetries`, `runs` / `updateErrs` (node update runs), `seq` (an in-process counter
    seeded from the clock and `MaxReleaseSeq`).
  - `network.Hub` chassis status map (`internal/network/state.go:29`).
  - Background loops in `cmd/hub/main.go:99–188` run in every process: snapshot sweep, egress CA sweep, build logs,
    integration refresh, usage poll, version check, registry GC, build scheduler, auto rebuilds, VM expiry.
- `Online(node)` = a connection is attached to this process. `Disconnect(node)` closes it on credential revoke /
  re-enroll (`internal/api/nodes.go:231,248`).
- Registry GC and the build scheduler are not independent loops: request handlers read their in-process state
  (`GC.PushAllowed()` gates push tokens in `internal/api/registry.go:96`, `BuildsMayStart`, `sched.Kick()`,
  `TagSync.Resume()`), and GC runs `registry garbage-collect` on the hub-local `registry-data` volume.

## Approaches considered

- **A — nodes on NATS, hubs interchangeable (chosen).** Nodes connect to NATS instead of the hub. Hubs answer as a queue
  group. Node control survives hub restarts; storage and gateway can join the same bus later.
- **B — keep WebSocket, coordinate hubs via Postgres `LISTEN/NOTIFY`.** Smallest change, no new component. Rejected: a
  hub update still drops that hub's nodes (reconnect elsewhere), which is exactly what should stop, and later services
  would have no bus to join.
- **A with desired state in JetStream KV.** Rejected by the user: Postgres must be the source of truth.
- **Pure desired-state push, no commands.** Rejected by the user: named commands give a direct reply and a clear
  meaning per action; desired state is for resync.
- **A connection proxy holding the WebSockets and forwarding to NATS.** Rejected: the proxy itself has to be updated, so
  the disconnect only moves.

## Design

### Subjects

`<service>.<version>.<…>`; the service comes first, the version right after it so a service can serve two versions
during a rolling update. Node-addressed subjects always contain the literal token `node` followed by the node ID, so a
node ID is never confused with a resource name.

| Pattern | Subject | Listener |
|---|---|---|
| hub → one node, VM command | `compute.v1.node.{node}.vm.{vm}.{verb}` | that node |
| hub → one node, full sync push | `compute.v1.node.{node}.sync` | that node |
| hub → one node, other | `fleet.v1.node.{node}.update`, `network.v1.node.{node}.underlay` | that node |
| node → hub, request | `compute.v1.node.{node}.sync.get`, `network.v1.node.{node}.underlay.get` | hub queue group |
| node → hub, report | `compute.v1.node.{node}.vm.{vm}.status`, `fleet.v1.node.{node}.{heartbeat,usage,disks,image,update.result}` | hub queue group |
| hub → node, query | `compute.v1.node.{node}.vm.{vm}.processes` | that node |
| any worker | `<service>.v1.<resource>.<verb>` | service queue group (not used in this project) |
| hub → hub | `fleet.v1.org.{org}.changed` | hub queue group |

Exact verb and report names are the builder's; the patterns are fixed.

A node's NATS permissions are exactly: publish and subscribe on `*.v1.node.{its id}.>` (plus `_INBOX.>` for replies).
NATS enforces it, so a hub may take the node ID from the subject, as today it takes it from the credential and never
from the message. Adding a service later (`disk.v1.node.{id}.>`) needs no new node credentials.

### Commands (user actions on a VM)

1. The API handler writes the change to Postgres first (as today), bumping the VM's generation.
2. It sends `compute.v1.node.{node}.vm.{vm}.{verb}` as a NATS request. Verbs follow user actions: create, start, stop,
   delete, snapshot, restore, fork, … The payload is the full `DesiredVM` for that VM built from Postgres (for delete:
   the VM ID and generation).
3. The node applies it as "set the desired entry of this VM" (delete: remove it) and runs its existing reconcile
   (`internal/node/reconcile.go`). Commands and sync end in the same code path.
4. The node replies `accepted` or `rejected` with a reason after validating, not after the VM is up (a create can pull
   an image for minutes); progress arrives as status.
5. A rejection sets the VM to `error` with the reason in Postgres. No reply (node offline, timeout) changes nothing:
   Postgres already holds the intent and the next sync delivers it. The API reports that the node did not answer.
6. Commands are idempotent and ordered by version (next section): the node drops anything older than what it has.
   Sending twice, or a late command after a newer sync, is harmless.

### Versions: commands and syncs in any order *(design call)*

With several hubs and two paths (commands, sync pushes) nothing arrives in order anymore. The node's reconcile treats a
desired list as complete and destroys VMs missing from it (`internal/node/reconcile.go:16`), so a stale sync could
destroy a VM a newer create brought, or revive one a newer delete removed. Rules:

- Every write that changes what a node should run (any column that ends up in `DesiredVM`, including settings that do
  not bump today's generation such as auto-suspend, env, integrations, egress; attaching/detaching disks; deleting the
  VM) stamps the VM row with a `desired_version` from one Postgres sequence, in the same transaction.
- Deleting a VM keeps a tombstone (the row stays soft-deleted with its version) until the node has confirmed it.
- A command carries that VM's version; a sync carries, per VM, its version and, for the whole list, the sequence value
  read at the start of the transaction it was built in (`built_at_version`).
- Node merge: an entry replaces the node's local entry only if its version is higher. A VM missing from a sync is
  removed only if its local version is lower than the sync's `built_at_version`; otherwise it was created after the sync
  was built and stays. A delete command leaves a local tombstone with its version, so an older sync cannot revive the VM.
- The node keeps versions and tombstones in memory; after a node restart the first sync is authoritative.
- Today's `generation` (restart on spec change) stays as it is; `desired_version` is only for ordering.

### Disk release confirmation *(design call)*

Today the node echoes the `seq` of the full list it reconciled, and the hub frees a releasing export only for reports
after the detach (`internal/fleet/conn.go:175`). With commands: the release is tagged with the `desired_version` of the
detach/delete. The node reports disks in use together with the highest version up to which it has applied **and**
reconciled every entry it received (a sync counts for everything up to its `built_at_version`). The hub confirms the
release only when that reported version is at least the release's version and the disk is not in use. After a detach the
hub pushes a sync to the node so this happens within seconds. This replaces the in-process `seq` counter.

The audit entry stays where it is today: written by the API for the user's action, not for the bus message.

### Sync (everything that is not a single user action)

- The node requests `compute.v1.node.{node}.sync.get` on start, on every NATS reconnect and every 5 minutes; any hub
  replies with today's full desired list (`Fleet.desired`), unchanged in content. This heals lost commands.
- Today every change goes through `NotifyOrg`, VM user actions included (`vms.go`, `snapshots.go`, `disks.go`,
  `expiry.go`). Those callers split: single-VM user actions become commands; changes that touch many VMs at once
  (network, env, members, integrations, images, SSH keys, roles) stay org events and are not turned into commands: the hub publishes `fleet.v1.org.{org}.changed`; each hub then pushes
  `compute.v1.node.{node}.sync` with the full list to the org's nodes. The event goes to the hub queue group, so
  exactly one hub does the push.
- The release sequence (`seq`) is replaced by `desired_version` (see Disk release confirmation).

### Reports from nodes

- Statuses can now be processed by different hubs concurrently. Each node stamps every report with a per-node
  monotonic sequence; the store ignores a VM status older than the one it has. (Today ordering comes for free from one
  stream.)
- The node re-sends all VM statuses after every sync and every 5 minutes, so a lost status heals.
- `register` becomes: heartbeat + full status after connect; machine info / version / image / hypervisor go with the
  first heartbeat after (re)connect.

### Online, heartbeat, disconnect

- Nodes publish a heartbeat every 10 s; the hub stores `last_seen_at`. A node is online while `last_seen_at` is younger
  than 30 s. `Online()` reads Postgres instead of the connection map.
- What today happens on disconnect (`failSnapshots`, disk-error cleanup) runs in the leader's sweep when a node goes
  stale.
- Revoking a node credential / re-enroll: the hub marks it in Postgres and kicks the node's NATS connection through the
  NATS system account; the next connect is refused by auth (below).

### NATS authentication

NATS auth callout: when a node connects, NATS asks the hub queue group; the hub checks the node's existing credential
in Postgres (`NodeByCredential`) and returns a short-lived user with the permissions above. No new node credentials, no
keys to distribute, revocation stays in Postgres. While no hub is running, new connections fail; existing ones stay up.
Hubs and later services authenticate with their own service token the same way.

### State that moves out of the hub process

| Today (in memory) | Then |
|---|---|
| `conns`, `Online` | `last_seen_at` in Postgres |
| `diskErrs` | from the stored VM status (`Disks[].Error`), no separate map |
| node update runs (`runs`, `updateErrs`) | table in Postgres, driven by the leader |
| `retrying`, `imageRetries` | retry timers owned by the leader, or the 5-minute sync — builder's call |
| `seq` | `desired_version` (Postgres sequence) |
| chassis status (`network.Hub`, filled from the gateway's HTTP status report, `internal/network/state.go:141`) | Postgres, written by whichever hub receives the gateway's report *(design call; the gateway joins the bus in project 3)* |

### Background jobs: one leader

All loops in `cmd/hub/main.go:99–188` run only in the hub holding a Postgres advisory lock (session lock on a dedicated
connection). Losing the connection stops the loops; another hub takes the lock. Request handlers and NATS subscriptions
run in every hub.

### Deployment

- `deploy/compose.yaml` gains a `nats` service (config with auth callout), port 4222 published for nodes. NATS has its
  own image version, so updating the hub does not restart NATS.
- Nodes reach NATS directly on its own port (user decision); no Caddy or WebSocket in front, and never through the hub's
  HTTP port (that would tie them to the hub process again).
- TLS on 4222 is supported and follows the hub: when the hub runs with TLS, NATS does too; plain otherwise, as the hub
  URL is today *(design call: the payloads carry the same secrets the hub's HTTP connection carries today)*.
- The hub tells nodes the NATS URL in the install config; dev setup gets a `make` target like the other stores.

### Delivery and migration (user decision)

No long-lived branch next to `main` (that is what sank the 1.0 rewrite: `internal/fleet` and `internal/node` keep
changing on `main`). Work lands on `main` in small releasable PRs, in three kinds of steps:

1. **Transport-neutral rework**, released normally and working with today's WebSocket connection: state out of the
   hub process into Postgres (online, update runs, GC push hold, chassis status), leader lock, `desired_version` and
   tombstones, release confirmation by version, `NotifyOrg` split into commands and org events, the node-side merge of
   commands and syncs. This is most of the work.
2. **NATS transport, off by default.** Hub and node get a transport seam; with `OBSERVATORY_NATS_URL` set they talk
   over NATS, otherwise over WebSocket as today. This is a development switch used on the test VMs, not a supported
   migration path.
3. **Hard cut**, a small final PR: delete the WebSocket transport and the switch, NATS becomes required. That release
   is the one where existing nodes are moved with the installer's `--update` (prod has one node). No period where old
   and new nodes are supported side by side.

### Failure behaviour

| Event | Effect |
|---|---|
| Hub restart / update | Nodes stay connected; VMs untouched; API down only if it was the only hub; commands sent meanwhile are retried by the user or delivered by sync. |
| One of several hubs dies | Others answer; leader lock moves within seconds. |
| NATS restart | Nodes reconnect (client auto-reconnect) and sync; VMs untouched; heartbeat gap stays below the 30 s offline threshold if NATS is back in time. |
| Node offline | Commands time out, intent stays in Postgres, sync on reconnect. |
| Postgres down | As today: hub cannot work; nodes keep running VMs. |

## Success criteria (checked end to end on the pve01 test VMs)

- Restarting / updating the hub container while VMs run: no node goes offline, no VM is touched, a VM started right
  after the hub is back starts.
- Two hub containers against one Postgres + NATS: VM create/start/stop/delete/snapshot/restore/fork, disk attach and
  release, processes, node update run, integrations refresh all work; background jobs run in one hub only (logs).
- Killing the leader hub: the other takes over the jobs.
- Restarting NATS: nodes come back and resync without user action.
- Rejected command (e.g. image missing): API returns the reason, VM is `error` with that reason.
- Revoking a node credential cuts its NATS connection at once.

## Left to the builder

- Exact verb/report names, payload structs (reuse `proto` types), timeouts (command reply, API wait).
- NATS client library (`nats.go`) and auth-callout wiring.
- Schema changes for `last_seen_at`, report sequence, update runs, release sequence.
- Where retry timers and chassis status live (table above).
- Order of work within the three kinds of steps, as long as each step is releasable.

# observatory: updating nodes from the hub

## Intent

Admins and users should stay current. Project 1 (version checks, PR #21) shows when a node runs an older version than
the hub, but updating it still means: regenerate the credential on the node page (which disconnects the node), SSH to
the host and re-run the installer. With several nodes that is tedious and gets skipped.

This project lets an org admin update nodes from the web UI: one node from its page, or a selection of nodes as a
rolling update, one after the other.

Success: an admin ticks the outdated nodes, clicks once, watches them update one by one, and their VMs end up in the
state they were in before, with memory kept unless the update changes the hypervisor.

## Goals

- Update a single node from its page.
- Update a selection of nodes as a rolling update driven by the hub, one node at a time.
- VMs return to their previous power state after the update.
- Keep VM memory across the update whenever the hypervisor version does not change.
- Tell the admin before the update whether VMs will briefly pause or restart.

## Non-goals

- VMs running through the update without interruption (Firecracker, qemu-nbd and egress proxies surviving a node
  restart and being re-adopted). Possible later project if the pause hurts.
- Updating the hub or the CLI (project 1).
- Scheduling updates (maintenance windows) or updating nodes automatically.
- Updating offline nodes; they keep the manual re-install path.

## Anti-goals

- An update must never lose a VM or its disks, and never leave the node without a working credential.
- A failing node must stop the rollout, never take the next node down too.
- No regeneration of the node credential and no offline phase like the manual re-enroll.

## Current state (what this builds on)

- Nodes keep one websocket to the hub (`internal/node/agent.go`, `internal/fleet/conn.go`); the hub sends `desired` and
  `underlay`, the node sends `register` (with `Version`), `status`, `disks`, `metrics`, `image`.
- `cmd/hub/install.sh` (served at `/node/install.sh`, embedded in the hub) installs packages, Firecracker
  `FIRECRACKER_VERSION`, host network setup, enrolls with a single-use token, installs `/usr/local/bin/observatory-node`
  and the systemd unit, then restarts it. Re-running it is idempotent but needs a new token.
- On SIGTERM the node's `stopAll` hibernates VMs (memory to disk) when space allows, else stops them gracefully, and
  kills what is left when the stop budget runs out. On start `restoreAll` brings VMs back to their desired state,
  resuming hibernated ones; a memory state that cannot be loaded (e.g. after a Firecracker upgrade) is discarded and the
  VM cold-boots with the reason shown as the VM error (`docs/vm-lifecycle.md`).
- Project 1 adds `outdated` to node JSON (`version.Older(node, hub)`).
- Permission `node:update` exists (used for rename).

## Approaches

**How the node updates itself**
- **Re-run `install.sh` in an update mode (picked).** One path keeps doing all host setup (packages, Firecracker,
  network, unit), so an update never leaves a node half-updated.
- Swap only the node binary. Smaller, but any release that changes host setup (e.g. a Firecracker bump) would need the
  manual installer again. Rejected.

**What happens to running VMs**
- **Hibernate as today; shut down only when the Firecracker version changes (picked).** Memory survives most updates;
  only a Firecracker change, which would make memory states unloadable anyway, turns into a clean shutdown and cold boot.
- Always shut down and cold-boot. Simpler, but loses all VM memory on every update. Rejected.
- Keep VMs running through the update. Large rework (see non-goals). Deferred.

**Who drives the rolling update**
- **The hub (picked).** Survives closing the tab, other admins see progress, one place enforces "one at a time".
- The browser. Less hub code, but closing the tab stops the rollout and only its starter sees progress. Rejected.

## Design

### Node reports its hypervisor

- `proto.Register` gains `Hypervisor *Hypervisor` with `Hypervisor{Type string; Version string}` (JSON
  `{"type":"firecracker","version":"v1.18.0"}`), chosen generic so other hypervisors can be added later.
- The Firecracker provider already runs `firecracker --version`; it reports the parsed version.
- The hub stores it with the node (next to `version`) and shows it in node JSON as `hypervisor`.
- The hub knows the target Firecracker version by reading `FIRECRACKER_VERSION=` from its embedded `install.sh`.
  Node JSON gains `updatePausesOnly bool` *(name is the builder's call)*: true when the node's hypervisor version equals
  the target. Unknown hypervisor version (old node) counts as "VMs restart".

### Update mode of `install.sh`

- `install.sh --update --hub URL` runs everything except enrollment: no token, `node.json` and its credential are left
  as they are. It refuses to run without an existing `/etc/observatory-node/node.json`.
- Before restarting the service it compares the installed Firecracker version with `FIRECRACKER_VERSION`. If they differ
  it writes a marker file *(path is the builder's call, under the node's state dir)* telling the node to shut VMs down
  instead of hibernating them. Order matters: the check happens before the new Firecracker binary is installed.
- The manual installer keeps working as before; `docs/nodes.md` documents `--update` for hosts updated by hand.

### Node handles `update`

- New hub→node message `{"type":"update"}`.
- The node downloads `/node/install.sh` from its hub and starts it detached with
  `systemd-run --unit observatory-node-update --collect sh <script> --update --hub <hub>` so the script survives the
  service restart it triggers. A second `update` while that unit is active is ignored.
- The node reports only failures to the hub (`{"type":"update","update":{"error":…}}`) if starting the unit fails or
  the unit exits unsuccessfully; success shows as a re-register on the new version. A report sent while disconnected
  is dropped and the hub falls back to its timeout. The node watches the unit's
  result (`systemctl show -p Result,ExecMainStatus`) while it is still the old binary; a successful script ends in a
  service restart, so the old binary never sees success. *(our call: fail fast instead of waiting for the timeout)*
- `install.sh --update` removes the Firecracker marker (and anything else it wrote for the restart) on any failure
  before the restart point, via `trap`, so a later unrelated restart hibernates as usual. A failure after packages or
  Firecracker were installed leaves them installed; the old node binary keeps running and the next update retries.
  *(our call)*

### VMs during the restart

- `stopAll` checks the marker: present → stop VMs gracefully (the existing 240 s stop budget, then kill) instead of hibernating; the marker
  is removed after the node starts again.
- The hub never changes a VM's desired power for an update. After the restart `restoreAll` brings every VM back to its
  desired state: hibernated memory is resumed when it could be kept, otherwise the VM cold-boots (running, suspended),
  matching the existing restore behavior. Builder: check that paused and user-hibernated VMs end in a sensible state
  after a shutdown (paused → cold-boot then pause, hibernated without memory → stopped is acceptable; document it).

### Hub: update runs

- A run is a list of node IDs of one org, processed in list order, one at a time. At most one run per org.
- Per node: send `update`, then wait until the node registers again with `version == hub version`, then until every VM
  on the node has reached its desired power state or reported an error → done. Failure when it reports `failed`, is
  not back on the hub version within 15 minutes, a VM stays in error for over 2 minutes while restoring (the failure names the VM and its error; disk-attached VMs
  error briefly while the underlay comes up), or
  the VMs are not all back within a further 10 minutes. Re-registrations with the old version while
  the update runs are ignored: the installer touches networking, so the old binary may reconnect mid-update.
  *(our call)* A failure stops the run.
- Selecting a node that is offline or not outdated is rejected when the run is created (API 400/409), not skipped.
- Cancel: the current node finishes, no further node starts.
- State lives in hub memory *(builder's call where, likely `internal/fleet`)*; a hub restart ends the run, and starting
  a new one picks up the nodes that are still outdated.
- Audit log: run started (node list), each node's result, run cancelled. *(our call)*

### API *(paths are the builder's call, following existing node routes)*

- `POST /api/orgs/{org}/nodes/update` with `{"nodes": [id, …]}` starts a run (a single node is a run of one), needs
  `node:update` on every node listed.
- `GET` of the org's current/last run: state (`running`, `done`, `failed`, `cancelled`), nodes with their status
  (`pending`, `updating`, `done`, `failed` + reason), started by, times.
- `POST …/cancel` stops the run after the current node.
- Node JSON gains `updating bool` for the node currently being updated.

### Web

- **Node list:** checkboxes on nodes that are online and outdated (none while a run is active); header checkbox selects
  all outdated online nodes; button "Auswahl aktualisieren (N)" / "Update selected (N)". A run banner above the list
  shows progress ("1/2 · node-1 wird aktualisiert"), the failure reason when it stopped, and "Abbrechen" while running.
  Nodes being updated show an `UPDATING` tag.
- **Node page:** "Node aktualisieren" for an outdated online node, starting a run of one.
- **Confirmation dialog:** lists the nodes and VM counts and says per node whether VMs briefly pause (memory kept) or
  restart (hypervisor update, memory lost).
- The page polls as it does today (5 s); no push channel needed.
- Visible to users with `node:update`; others see the run banner read-only.

## Left to the builder

- Exact API paths, JSON field names, store column/migration for the hypervisor, marker file location.
- How the hub detects "back online with the new version" and "all VMs back" (hooks in the fleet's register and status
  handling).
- Message shape for update errors from the node.
- Copy, styling with the existing `ui/` components, en/de strings.

## Constraints

- Nodes are linux/amd64 with systemd; `systemd-run` is available there.
- The node service runs as root (installed by `install.sh` with sudo), so it may run the installer.
- Nodes on a release without this feature ignore unknown message types (`internal/node/agent.go`), so they can never
  be updated from the hub. A node is updatable only if it reported a `hypervisor` (the marker that it understands
  `update`). Others are outdated but not selectable; the UI says "update by hand once (re-run the install command)" and
  the API rejects them. Every existing node therefore needs one manual update after this ships. *(our call, forced by
  the code)*

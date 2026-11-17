# VM disks (storage server and NBD)

## Intent

Project 3 of the VM port (roadmap in `2026-11-17-vms-nodes-design.md`). After project 2 a VM has only its root disk, which lives on its node and dies with the VM. This project brings the PoC's standalone disks: data that outlives VMs, lives on one storage server per hub and is attached to a VM over NBD (`qemu-nbd` on the node, `nbdkit` on the storage server), with create, copy, grow, attach/detach and release as in the PoC. It plugs disks into what observatory has: org/team/user ownership, Casbin permissions, limits, audit, and the node trust model from project 2 (nodes of different orgs are not trusted).

The PoC's weak spot is security: its NBD port is unauthenticated, so anyone who reaches it can read and write every disk. Here a disk is reachable only by the one node that currently runs the VM it is attached to, and only while it is attached.

Success: a member creates a 20 GB disk, attaches it to their VM at `/data`, writes files, detaches it, attaches it to another of their VMs on another node and finds the files; a copy of it appears in seconds on a reflink-capable volume; a node of another org — even one with root on its host and a working underlay — cannot read a byte of it; a storage restart pauses writes in running VMs and loses none.

## Decided

The user asked us to decide without questions, using best practice. Every decision below is ours and marked *(Our call.)*; the roadmap's "storage server, NBD via `qemu-nbd`, create/copy/grow/release as in the PoC, one storage server per hub" is the given.

## Goals

- **Disks as a resource.** A disk belongs to an org and is owned by a user or a team, like a VM; it has a name, a size in GB, a state and at most one attachment.
- **Create, copy, grow, delete, attach, detach, release** with the PoC semantics (states `creating`, `ready`, `releasing`, `missing`, plus `growing`), including release confirmation by the node and force release.
- **Per-attachment access.** Each attachment gets its own NBD export (own port, own TLS-PSK key) that only the node running the VM receives; detach/release ends the export.
- **Cheap copies.** Copy uses reflink (`FICLONE`) when the storage filesystem supports it and falls back to the PoC's sparse copy, so project 6 snapshots build on the same operation.
- **Limits:** standalone disks count against the existing disk GB limit (user, team, org), together with root disks.
- **Permissions, audit, UI** for disks and VM disk attachment.
- **Storage server** (`cmd/storage`) ported from the PoC, run by the dev compose next to the gateway.

## Non-goals

- Snapshots, hibernate, restore, new VM from snapshot (project 6; see "Path to snapshots").
- Encryption at rest *(Our call: deferred; see Security)*.
- More than one storage server, storage HA, replication, backups, quotas per storage server, moving disks between storage servers.
- Shrinking disks; resizing the filesystem online without a restart.
- Shared disks (attached to several VMs), read-only attachments.
- Root disks on the storage server (they stay node-local as in project 1).
- Attaching disks in the VM create dialog *(Our call: attach after create; a stopped or pending VM takes disks without a restart)*.
- vz / macOS nodes.

## Anti-goals

- A node, a device or any other host reaching a disk that is not attached to a VM running on that node — including nodes of the same org and the node that ran it before a release.
- A disk blanked or recreated silently after its file was lost.
- Two nodes writing the same disk at once (release race).
- Export keys or the storage token in logs, audit rows, node status, `vm.json` permissions wider than root, or any API response other than the storage state.
- An org admin reading a member's disk (IAM: admins may delete, not look inside).

## Approaches

- **A) Port the PoC storage and NBD path, add per-attachment exports with TLS-PSK over the WireGuard underlay (picked).** `nbdkit` runs one process per attached disk on its own port with a fresh pre-shared key; `qemu-nbd` on the node connects with that key through the underlay to the gateway address. Node side (attach, reconnect, guest mount) stays the proven PoC code with one extra option. Cost: one `nbdkit` process per attached disk, a port range.
- B) One `nbdkit` for all disks, auth by an export-name capability (random export names). Fewer processes, but every authenticated or unauthenticated client can try names, listing must be reliably disabled, and revocation means renaming files under a running server.
- C) Own NBD server in Go with per-connection authorization. Full control, but re-implements a block protocol the PoC deliberately took from mature tools.

## Data model (outline)

- `disks`: id, org_id, owner (`user_id` or `team_id`, exactly one), name (unique per org among live disks, `^[a-z][a-z0-9-]{0,30}$` like networks), size_gb, ready_gb (size the storage server has confirmed; 0 = file never seen), copy_from (pending copy source), created_by, created_at, deleted_at (soft delete until the storage server reports the file gone).
- `vm_disks`: vm_id (cascade), disk_id (unique: one attachment per disk), mount_path, position. Order by position = guest device order (`vdb`, `vdc`, …).
- `disk_exports`: disk_id (pk), node_id (cascade), port (unique), sealed key (master key, AAD `disk-export:<disk id>`), releasing, release_seq. An export row is "node N may reach disk D on port P with key K"; it exists from attach until the release is confirmed or forced.
- Storage reports (files, allocated bytes, export errors, version, last poll time) are kept in memory in the hub, like node connection state; everything durable (`ready_gb`, deletions) is written to Postgres.

## Storage server

- Ported from the PoC (`internal/storage`, `cmd/storage`): one file per disk `<disk id>.img` in `/disks`, built in `.partial/` and renamed, `fsync` of file and directory; creates only disks marked `create` (never recreates a seen disk), grows, never shrinks, deletes only listed deletions, never touches unknown files (orphans).
- **Copy:** `FICLONE` first *(Our call)*; on `EOPNOTSUPP`/`EXDEV`/`EINVAL` the PoC sparse copy. The dev compose volume works either way; production docs recommend XFS (`reflink=1`) or btrfs for `/disks`.
- **Hub link:** polls `GET /api/storage/state` every 2 s and posts `POST /api/storage/status`, like the gateway, instead of the PoC WebSocket *(Our call: one pattern for hub infrastructure)*. Credential `OBSERVATORY_STORAGE_TOKEN` (≥ 32 chars, constant-time compare, separate from the gateway token so a leaked gateway token reveals no disk keys) *(Our call)*.
- **Hub unreachable:** the storage server keeps applying its last state; it never stops exports or deletes files because a poll failed.
- **Exports:** for each export in the state, one `nbdkit --foreground --exit-with-parent --ipaddr 10.198.0.1 --port <port> --tls=require --tls-psk=<file> file file=/disks/<id>.img`; the PSK file (`<disk id>:<hex key>`, mode 0600) lives in `/run/storage`, never on the disk volume. Exports missing from the state are killed; changed port or key restarts the process; a crashed process is restarted on the next poll; bind/start errors are reported per export.
- **Runs** in the dev compose as service `storage` in the `gateway` profile with `network_mode: service:gateway`, volume `disks:/disks` *(Our call: shares the gateway's network namespace so it listens on the underlay address only, nothing published on the host)*.

## Network path and node authentication

- **Path:** node → WireGuard underlay → `10.198.0.1:<port>` (gateway address, storage listens there) *(Our call)*. The gateway's INPUT firewall accepts TCP `10810–11809` from `wg-underlay`; nothing else exposes the ports. Disk traffic is encrypted twice (WireGuard, TLS); accepted for simplicity.
- **Ports:** `10810–11809` (1 000 concurrent exports per hub), lowest free port allocated at attach. *(Our call; ceiling noted, widen the range when reached.)*
- **Authentication = possession of the per-export key.** At attach the hub generates 32 random bytes, stores them sealed, and sends them only (a) to the storage server in its state and (b) to the node the VM runs on, inside that VM's `DesiredVM` attachment (`url = nbd://10.198.0.1:<port>/<disk id>`, `psk` separate from the URL). The underlay limits who can try (enrolled nodes only); TLS-PSK limits who can succeed (only that node). A node of another org gets nothing: the desired list only carries its own VMs.
- **Revocation = fencing.** Release confirmation or force release deletes the export; the storage server kills that `nbdkit`, so the old node's `qemu-nbd` cannot reconnect. A later attach gets a new port and a new key.
- **Node side:** `AttachNBD` writes the key to `<vm dir>/nbd-<disk id>-tls/keys.psk` (0600) and adds `--object tls-creds-psk,…,username=<disk id>` and `file.tls-creds` to the existing `qemu-nbd` arguments (`reconnect-delay` 7 days as in the PoC). `vm.json` (which now carries the key) is written 0600. The key is never in the process arguments, node logs, `status`/`register` or the URL.

## Disks lifecycle

- **Create:** name, owner (me, or a team where I may `disk:create` for the team), size 1–4 096 GB *(Our call)*. Limits checked under the org lock. State `creating` until the storage server reports the file at full size → `ready`.
- **Copy:** new name and owner; size of the source; source must not be `missing`; copying an attached or releasing disk is allowed with a warning that the copy is only crash-consistent (PoC). A disk that is the source of a pending copy cannot be deleted. Needs `disk:use` on the source (reading the data) and `disk:create` for the new owner.
- **Grow:** larger size only, limits checked; not while releasing; attached to a running VM → `restartAllowed` required (PoC). The VM gets the new size (generation bump, restart) once the storage server reports the grown file; until then the disk shows `growing`.
- **Delete:** only when not attached, not exported and not a pending copy source. The row is soft-deleted (limits freed, name reusable at once) and purged when the storage server reports the file gone *(Our call)*.
- **Attach/detach:** the full disk list of a VM is set at once (like networks): up to 8 disks per VM *(Our call)*, each with a mount path (PoC rules: clean absolute path, not `/`, not reserved trees such as `/etc`, `/usr`, `/home/ubuntu/.ssh`, no duplicates). Attach requires: same org, **same owner as the VM** *(Our call: no data crosses owners by attaching; copies do that explicitly)*, disk not `missing`, not attached elsewhere, no export (not releasing). A running VM restarts and needs `restartAllowed`. Attaching a `creating` disk is allowed; the VM fails with `storage not ready` until it is ready (PoC).
- **Release:** detach or VM delete marks the export `releasing`. The node confirms with its `disks` message (or `register`): the disk is not in `disksInUse` and `appliedSeq` ≥ the seq of the first desired list sent without the disk. Desired seqs become hub-wide monotonic (start at the hub's start time in ns) so a node's `appliedSeq` stays comparable across reconnects and hub restarts *(Our call)*. A releasing disk cannot be attached, grown or deleted.
- **Force release:** for a releasing disk whose node stays offline or is gone. Deletes the export (fencing); unflushed writes of the old VM may be lost — the UI says so. Deleting a node deletes its exports.
- **Missing:** a disk the storage server has seen (`ready_gb > 0`) whose file is absent from a fresh report. Never recreated blank; cannot be attached or copied; comes back when the file is restored.
- **Ownership blocks:** a member who owns disks cannot be removed from the org; a team that owns disks cannot be deleted (as with VMs).

## Failure semantics

| Failure | Behaviour |
|---|---|
| Storage server down / restarting | Running VMs: `qemu-nbd` holds I/O and resumes (up to 7 days), nothing written is lost; node reports the disk error, shown on the VM. New boots fail `storage unreachable`. Disks page shows "storage offline" (no status for 10 s). API keeps accepting changes; they apply when it is back. |
| Hub down | Storage keeps its exports; nodes keep running VMs and restart them from `vm.json` (project 1 rule). |
| Gateway restart | Storage shares its network namespace: same as a storage restart. |
| Node offline | Its exports stay; detach/delete on the hub leaves the disk `releasing` until the node returns or force release. |
| Disk volume lost | Seen disks → `missing`; nothing recreated; orphan files are never deleted. |
| Copy source gone mid-copy | Copy stays `creating`; source delete is refused while the copy is pending. |
| Port range exhausted | Attach returns 409 "no free storage port". |
| Export cannot bind (underlay not up yet) | Reported per export, retried every poll; VM boot fails `storage unreachable` meanwhile. |

## Security

- Per-export TLS-PSK + underlay-only listener + per-node delivery of keys (see above). The storage server is hub infrastructure (trusted like the gateway): it holds every disk and every export key.
- Keys sealed with `OBSERVATORY_SECRETS_KEY` in Postgres; unsealed only to build the storage state and a node's desired list.
- **Encryption at rest: deferred** *(Our call)*. Operators put `/disks` on an encrypted volume (LUKS) if they need it; documented. Upgrade path: a per-disk key held by the hub and `nbdkit`'s LUKS filter, without node changes.
- Known limits, documented: the node that runs a VM can read its attached disks (root on the host); org admins of that node's org are therefore trusted for disks attached to VMs on their nodes, as they are for root disks today.

## Path to snapshots (project 6)

- Disk files are plain files on one filesystem, named by id: a disk snapshot is a storage-side reflink of `<id>.img` into a new file — the same operation as copy, instant and space-sharing on XFS/btrfs.
- Project 6 adds a `kind` (disk / snapshot) and a parent to `disks`, a read-only flag on the storage desired list and a "clone now" step that the hub triggers after Firecracker pauses the VM and the node flushes its NBD devices, so memory and disk snapshots are consistent. Restore = clone snapshot into a new disk id and swap the attachment; new VM from snapshot = copy + attach.
- Root disks stay node-local; project 6 snapshots them on the node (reflink where the node's `paths.vms` filesystem supports it).
- Nothing in this project needs to change for that: copy is generic (`copy_from`), files are never rewritten in place, and exports address a disk id, not a path.

## Permissions

| Action | Object |
|---|---|
| `disk:create` | `org/disks` (user disks), `org/team-disks/<team-id>` (team disks) |
| `disk:read`, `disk:update`, `disk:delete`, `disk:use` | `org/disks/<id>` |
| `vm:disks` | `org/vms/<id>` (change a VM's disk list) |

- `disk:update` = grow and force release; `disk:use` = attach and copy from (access to the data). Attach needs `vm:disks` on the VM and `disk:use` on every newly attached disk; detach needs only `vm:disks`.
- Disk objects are org-scoped (`org/disks/<id>`) like VMs, so org-wide deny rules cover user disks.
- Generated: user disk owner → `disk:read`, `disk:update`, `disk:delete`, `disk:use`; team disk → team subject `disk:read`, `disk:use`, team admins also `disk:update`, `disk:delete`; team admins `disk:create` on `org/team-disks/<team>`. `vm:disks` is generated like `vm:network` (user VM owner; team admins).
- Built-in, explicit action lists, never `disk:*`: members `disk:create@org/disks`; admins also `disk:create@org/team-disks/*`, `disk:read`, `disk:update`, `disk:delete@org/disks/*` and `vm:disks@org/vms/*`. **Admins never get `disk:use`** (IAM: delete, not look inside); combined with the same-owner rule they can detach and delete, never attach or copy another person's disk.
- All new actions go through the escalation rule.

| | member | team admin (of that team) | admin | owner |
|---|---|---|---|---|
| create own disks | yes | yes | yes | yes |
| create team disks | | yes | yes (all teams) | yes (all teams) |
| own disks: all actions | yes | yes | yes | yes |
| team disks: read, attach, copy | team member | yes | | |
| team disks: grow, release, delete | | yes | yes | yes |
| every disk: read, grow, force release, delete | | | yes | yes |
| change disks of a VM (`vm:disks`) | own VMs | team VMs | all (detach only, in effect) | all |

- Audit (colon form): `disk:create`, `disk:copy`, `disk:grow`, `disk:delete`, `disk:release` (force), `vm:disks` (details: disk names and mount paths). Release confirmation by a node is logged, not audited *(Our call)*.

## UI

- **Disks** page (`/orgs/:orgId/disks`, sidebar next to VMs): table with name, owner, size, used (allocated), state, attached to (VM, mount path); tabs All / Mine / Teams. Create dialog (name, owner, size). Row actions by permission: copy (name, owner; shows the crash-consistency warning), grow (new size; restart confirm when attached to a running VM), force release (only `releasing`; confirm text that unflushed writes may be lost), delete. Banner "Storage server offline" when the hub has no status for 10 s.
- **VM detail → Disks** section: attached disks (name, mount path, size, state, error from the node), "Edit disks" dialog listing the owner's attachable disks with mount path fields, restart confirm for a running VM.
- Actions shown only when permitted (`can()`); de + en.

```
Disks                                                            [+ Create disk]
 All   Mine   Teams
NAME      OWNER          SIZE    USED     STATE      ATTACHED TO
data      me             20 GB   3.1 GB   ready      dev:/data        Copy  Grow  Delete
cache     team Backend   50 GB   0 B      releasing  —                Force release
```

## Left to the builder

- Exact REST paths and payloads (start from: `GET/POST /api/orgs/{org}/disks`, `GET/DELETE …/disks/{id}`, `POST …/disks/{id}/copy|grow|release`, `PUT …/vms/{id}/disks`; storage `GET /api/storage/state`, `POST /api/storage/status`).
- How much PoC code is copied unchanged (`internal/storage/dir.go` nearly as is).
- Exact `nbdkit` / `qemu-nbd` TLS-PSK flags against the versions in Debian trixie and Ubuntu 24.04 (verified on Proxmox).
- Whether the gateway image or a separate image runs the storage server (separate `deploy/storage.Dockerfile` suggested).
- Test strategy; must include: limits refused at the boundary for create, copy and grow (user, team, org, root + standalone disks summed); attach refused for another owner's disk, a missing or releasing disk, a ninth disk, a reserved mount path; release confirmed only with `appliedSeq ≥ release_seq` and the disk absent from `disksInUse` (and never by a stale message from before the detach); force release deletes the export and a re-attach gets a new port and key; a node's desired list never contains another node's attachments; keys absent from API responses, audit rows, logs and node status; the storage server never recreates a seen disk, never deletes unknown files and keeps exports when the hub is unreachable; permission table above including deny rules and "admins never `disk:use`".
- **Real-hardware run on Proxmox** (test VM 108 as node of org A, 107 as node of org B, gateway + storage in Docker): create, attach, write, detach, re-attach to a second VM on 108 and find the files, copy (reflink on an XFS volume, sparse fallback on ext4), grow with restart; restart the storage container during a `dd` in the guest and verify the checksum afterwards; from 107 (org B, underlay up) try `qemu-nbd`/`nbdinfo` against the export without and with a wrong key → refused; force release while 108 is offline, bring 108 back → its `qemu-nbd` cannot reconnect.
- Update `docs/permissions.md`; add `docs/disks.md` (operations, filesystem recommendation, encryption-at-rest note, known limits).

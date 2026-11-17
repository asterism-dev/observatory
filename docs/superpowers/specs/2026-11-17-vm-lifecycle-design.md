# VM lifecycle (hibernate, snapshots, auto-suspend, fast boot, metrics)

## Intent

Project 6 of the VM port (roadmap in `2026-11-17-vms-nodes-design.md`; projects 1–3 are built). Today a VM is either running or stopped, and stopping loses everything in memory. This project uses what Firecracker can do and the PoC never used: pause, memory snapshots and restore. A VM can be parked with its memory on the node's disk and come back exactly where it was; a person can take named restore points of a VM (root disk, attached disks, optionally memory), roll back to one or start a new VM from one; idle VMs park themselves and wake on the first real packet; new VMs start from a pre-booted template instead of a cold boot; and the UI shows live CPU and RAM.

Success: a member hibernates their VM with an editor and a build running, the node frees its RAM, an hour later "Resume" brings both back with the clock right; they take a snapshot `before-upgrade` (with a 20 GB attached disk), break the system, restore the snapshot and find root disk, data disk and running processes as they were; they fork the snapshot into `dev-2`, which comes up on the same node with its own name, IP, MAC and SSH host keys; a VM with auto-suspend after 15 minutes hibernates on its own once only DNS and NTP chatter remain and wakes when `ssh dev.internal` hits it; `systemctl restart observatory-node` pauses every VM for a few seconds instead of rebooting it; the second VM of a size boots in under a second; the VM page shows RAM used of total and CPU load, updated every 10 s.

## Decided

The user asked us to decide without questions, using best practice. Every decision below is ours and marked *(Our call.)*; the list of features (hibernate, pause, named snapshots with restore and fork, auto-suspend on network idle, template fast boot with identity set afterwards, balloon memory stats, node restart behaviour) is the given.

## Goals

- **Power states** `paused` (standby: process and RAM kept, vCPUs stopped) and `hibernated` (memory and device state written to the node's disk, Firecracker gone, RAM freed), next to `running` and `stopped`. Resume restores exactly where the VM was.
- **Named snapshots** of a VM: root disk (on the node) + every attached disk (storage server, reflink) + memory when the VM is live. Restore a VM to one of its snapshots; fork a snapshot into a new VM. Counted against a new snapshot limit.
- **Auto-suspend:** per VM, off or after N idle minutes; idle = no real TCP/UDP traffic on the VM's taps (DNS, DHCP, NTP, mDNS, LLMNR, ARP, ICMP, ND/MLD/IGMP do not count). The node hibernates the VM and wakes it on the first inbound real packet, on "Start" in the API, or on any change that needs the VM up.
- **Template fast boot:** a node keeps pre-booted templates per (image, vCPU, RAM, NIC count); a new VM restores one and the guest takes on its identity afterwards ("takeoff": hostname, keys, variables, NIC MACs/IPs, root disk size, fresh SSH host keys and machine id).
- **Live metrics:** RAM from the virtio-balloon statistics (no agent needed for it), CPU from the host's view of the Firecracker process; shown on the VM page.
- **Node restart** no longer reboots VMs: the node hibernates them on stop and resumes them on start.

## Non-goals

- Live migration, cross-node fork or restore, moving snapshots between nodes *(Our call: snapshot memory and root disks are node-local; fork lands on the snapshot's node)*.
- Diff/incremental memory snapshots, UFFD lazy loading, memory overcommit through balloon inflation *(Our call: Full snapshots and the File memory backend only)*.
- Fork to another owner *(Our call: a fork keeps the source's owner, like the same-owner rule for disk attachment; memory and root disk carry the owner's variables and files)*.
- Scheduled snapshots, retention policies, snapshot export/download.
- Resizing vCPU/RAM of an existing VM; templates for VMs with attached disks at first boot.
- Keeping VMs running across a node crash or a host power loss (they cold-boot as today).
- Metrics history beyond 10 minutes, alerts, per-process data from inside the guest.

## Anti-goals

- Restoring a memory state against a disk that changed after it was taken (silent filesystem corruption). Memory state is only ever resumed with the exact root disk and disk files it was taken with.
- A hibernated or snapshotted VM's memory (which holds its variables) readable by anyone but root on its node, or included in a template.
- Two VMs with the same SSH host keys, machine id, MAC or IP because they came from the same template or snapshot.
- A template memory file writable by any jailed Firecracker.
- Discarding a hibernated VM's memory while it has attached disks, except on an explicit hard stop or restore (unflushed writes would be lost).
- An org admin reading a member's VM through a fork (admins never get `vm:fork`).
- A node of another org learning anything about a VM's snapshots or metrics.

## Approaches

- **A) Firecracker snapshots driven by the node, hub as the bookkeeper (picked).** The node pauses, snapshots and restores through Firecracker's API inside the existing jail; the hub stores desired power, snapshot rows and gates (storage copies done, restore done) and pushes them in the desired list like everything else. Disk snapshots reuse the storage server's reflink copy. One mechanism (Full snapshot + File backend) serves hibernate, node restart, named snapshots, fork and templates.
- B) Guest-level suspend (ACPI S3/S4 inside the VM). Needs guest cooperation, Firecracker has no ACPI sleep, and memory survives only on the guest's own disk.
- C) Copy whole VM dirs for snapshots and rely on cold boots. No memory state, no fast boot; only covers half the scope.

## Power states and hibernate

- **Desired power** (hub): `running`, `stopped`, `paused`, `hibernated`. **Reported state** adds `paused`, `hibernated` and `suspended` (hibernated by auto-suspend while desired is `running`).
- **Pause:** `PATCH /vm {"state":"Paused"}`; resume with `"Resumed"`. Process, taps, NBD and RAM stay.
- **Hibernate:** pause → flush NBD devices (`fsync` on each `/dev/nbdN`, which `qemu-nbd` passes on as NBD flush) → `PUT /snapshot/create {"snapshot_type":"Full","snapshot_path":"/vmstate","mem_file_path":"/mem"}` inside the jail → move both out to `<vm dir>/state/` → `fsync` root.img, both files and the dir → set `Hibernated` in `vm.json` (last, so a crash in between leaves a cold-bootable VM) → kill Firecracker, detach NBD. Taps are removed as on stop. *(Our call.)*
- **Resume from hibernation:** reseed, attach NBD, rebuild the jail (root.img, seed.iso, NBD device nodes, vmstate and mem hard-linked), start Firecracker without `--config-file`, `PUT /snapshot/load` (File backend, `resume_vm: false`, `network_overrides` for the taps), resume, unlink `state/` (the mapping keeps the inode alive), then tell the guest agent to set the clock and re-apply keys and variables from the current seed. *(Our call: keys and variables are refreshed on every resume, as on every boot; running processes keep their old environment.)*
- **Stable jail names:** NBD device nodes in the jail are named by guest device (`/vdc`, …) instead of `/nbdN`, so a state taken with one NBD index loads with another *(Our call)*.
- **Disk-space guard:** hibernate and memory snapshots need free space on `paths.vms` ≥ RAM + 1 GiB; otherwise the request fails with "not enough space on node" and the VM keeps running.
- **Stale or incompatible state:** a cold start always deletes `state/` first. A failed `snapshot/load` (e.g. Firecracker upgraded across a snapshot version) discards the state, cold-boots and reports the reason as the VM error until the next successful start.
- **Ending a hibernated VM:** hard stop and restore discard the memory state. Graceful shutdown, delete, and a change of NICs or attachments of a hibernated VM **with attached disks** resume it first and shut it down gracefully, so guest writes reach the disks; without attached disks they discard the state. *(Our call.)*
- **Hub view:** `paused`/`hibernated` VMs count against limits like any VM; `start` on a paused/hibernated/suspended VM resumes it.

## Guest agent

- A small Python script `vmm-agent.py` shipped in the seed and installed by `vmm-boot.sh` as a systemd unit; it listens on vsock port 1050 (Firecracker vsock `guest_cid` 3, host side `/v.sock` in the jail, host connects with `CONNECT 1050\n`) *(Our call: python3 is in the image already and AF_VSOCK needs no extra package; existing VMs get it at their next boot)*.
- One JSON request per connection, one JSON reply (`{"ok":true}` or `{"ok":false,"error":…}`); requests: `ping` (ready once `cloud-init status --wait` returned), `resume {time}` (set clock, re-run `vmm-boot.sh` from the seed), `takeoff {time, macs[], entropy}` (resume + new identity: set NIC MACs in device order, apply the seed's network config and hostname, regenerate SSH host keys and `/etc/machine-id`, mix `entropy` into `/dev/urandom`, `growpart`/`resize2fs` the root, mount the seed's disks, restart sshd). Secrets never travel over vsock: keys and variables come from the seed as on every boot.
- Only the host can reach the agent (vsock connections from other guests do not exist; guest-initiated connections to the host find no listener).

## Snapshots

- **What:** name (unique per VM, `^[a-z][a-z0-9-]{0,30}$`), memory yes/no, the VM's vCPU, RAM, root GB, NICs and attachments at that moment, size on the node, per disk: file on the storage server. Files: `<vm dir>/snapshots/<id>/{root.img, vmstate, mem}` on the node; `<snapshot disk id>.img` on the storage server.
- **Take:** the hub adds the snapshot to the VM's desired list. The node, for a live VM: pause → flush NBD → (memory) `snapshot/create` into the snapshot dir → reflink `root.img` (full copy where the filesystem has no reflink); for a hibernated VM: hard-link the state files and reflink `root.img`; for a stopped VM: root only (memory forced off). The VM **stays paused** and the node reports the snapshot; only then the hub inserts the storage copies of the attached disks (consistent, since the guest is paused and flushed) and marks the snapshot `ready` once the storage server reports every copy at full size. `complete` in the desired list lets the node resume. No attached disks → `ready` at once. *(Our call: pause only as long as the reflinks take; production docs recommend XFS for `paths.vms` and `/disks`.)*
- **Timeout:** a snapshot not `ready` 120 s after the node reported it becomes `error`, its storage copies are deleted and the node resumes the VM. The node also resumes on its own after 180 s paused for a snapshot (hub gone).
- **Delete:** row removed at once (limit freed); the node deletes snapshot dirs not in the desired list; storage copies are soft-deleted until the storage server reports them gone (as disks). A snapshot that is the source of a pending fork copy or restore cannot be deleted. Deleting a VM deletes its snapshots.
- **Limits:** new limit `snapshots` (count, user/team/org, null = unlimited) plus a hard cap of 10 snapshots per VM. Snapshot sizes are shown, not counted against disk GB (reflinked copies share blocks) *(Our call; ceiling: on non-reflink filesystems a snapshot costs a full copy)*.

## Restore and fork

- **Restore** (VM to its own snapshot): memory restore requires the VM's current vCPU, RAM, NICs and attachment list to equal the snapshot's; otherwise only a disk restore is offered (root disk + the snapshot's disks still attached to this VM). Sequence: hub sets the VM's restore request → node hard-stops the VM, discards `state/`, replaces `root.img` with a reflink of the snapshot's (temp file + rename), hard-links the snapshot's state into `state/` when memory is restored, echoes the request's seq → hub then asks the storage server to restore each disk in place (reflink snapshot file into `.partial/`, rename over `<id>.img`, restart that disk's `nbdkit`, report the restore token) → when every disk is done the hub marks the restore ready → node applies the desired power (resume or boot). *(Our call: in place, so a disk keeps its id, name, permissions and audit trail.)* Restore needs `restartAllowed` when the VM is not stopped.
- **Fork** (new VM from a snapshot): same owner, same node, same size; name given by the user; new management IP/MAC; joins the networks the source is on now (memory fork requires the same NIC count as the snapshot, else 409); each snapshot disk becomes a new disk `<disk>-<4 hex>` copied from the snapshot file and attached at the same mount path. The hub keeps the new VM out of the node's desired list until its disks are ready. The node builds the VM dir from the snapshot (reflink root; memory: reflink-copy of vmstate/mem, not a hard link, since the fork runs under another jail UID) and starts it with takeoff (memory) or cold-boots it (no memory). Limits are checked for the VM and the disks.

## Auto-suspend

- **Setting:** per VM, `autoSuspendMinutes` 0 (off, default) or 5–1440 *(Our call)*.
- **Idle detection (node):** an `AF_PACKET` socket per tap of a running VM; a frame counts as activity if it is IPv4/IPv6 TCP or UDP and neither port is 53, 67, 68, 123, 546, 547, 5353 or 5355. After an activity frame the sockets are closed for 30 s (cheap on busy VMs; idle error ≤ 30 s), then reopened. Idle for the setting → hibernate, report `suspended`. *(Our call: CPU load does not count, as documented for users; only traffic.)*
- **Wake:** while suspended the node recreates the VM's taps (so the OVN port stays bound and the gateway can deliver) and reads them itself; the first inbound activity frame, a bumped `wakeSeq` in the desired list (API "Start"), or a desired change that needs the VM triggers the resume. Frames that arrive while waking are lost; TCP retransmits the SYN.
- Explicit pause/hibernate never wakes on traffic. Snapshot, restore, attach and network changes on a suspended VM behave as on a hibernated one.

## Template fast boot

- **Per node, built lazily:** when a VM of shape (base image file + mtime, vCPU, RAM, NIC count, guest script version) is created and no template exists, it cold-boots as today and the node builds that template in the background (one build at a time). Templates live in `<paths.vms>/.templates/<key>/` (same filesystem as VM dirs, so hard links work); at most 4, least recently used evicted. Node config `"fastBoot": false` turns it off *(Our call)*.
- **Build:** clone the base image to 5 GB (the API's smallest root disk), seed without keys or variables and with random MACs, NAT tap on `vmm0` (cloud-init needs the network on first boot), extra NIC taps unattached; boot, wait for agent `ping`, pause, `snapshot/create`, kill. Template files are root-owned 0644 and jailed read-only *(Our call: they hold no secrets by construction, and a shared inode must never be writable by a VMM)*.
- **Use:** first boot of a VM without attachments whose shape has a template: reflink the template root to `root.img` and grow it to the VM's size, write the VM's seed, jail the template's vmstate/mem read-only, `snapshot/load` with `network_overrides` to the VM's taps, `PATCH /drives/root` so the guest sees the new size, resume, agent `takeoff`. Any failure kills the restored VM and falls back to a cold boot; the template is deleted after a takeoff failure.

## Metrics

- Firecracker gets a balloon device at boot (`amount_mib` 0, `deflate_on_oom`, `stats_polling_interval_s` 5); `GET /balloon/statistics` gives total and available memory without a guest agent. CPU = delta of `utime+stime` of the Firecracker process over the interval ÷ (interval × vCPUs), from `/proc`.
- The node samples running VMs every 10 s and sends one `metrics` message; the hub keeps the last 60 samples per VM in memory (no Postgres), accepts samples only for VMs of that node and clamps values (CPU 0–100 %, memory ≤ the VM's RAM).
- VMs booted before this project have no balloon until their next cold boot: RAM shows "—".

## Node restart

- Firecracker stays a child of the node (Pdeathsig, PID namespace) *(Our call: adopting running VMMs after a restart would need re-attaching taps, NBD clients, console offsets and watchers; not cheap)*. Instead, on SIGTERM the node hibernates all running and paused VMs in parallel (240 s budget, then hard stop for the rest) and on start resumes every VM whose `vm.json` says `Hibernated` and whose desired power is running/paused. The installer sets `TimeoutStopSec=300`. A host reboot through systemd therefore keeps VM memory too. A crash or power loss cold-boots as today.

## Data model (outline)

- `vms`: power check gains `paused`, `hibernated`; `auto_suspend_minutes`, `wake_seq`; `restore_snapshot_id`, `restore_memory`, `restore_seq`, `restore_state` (`''`, `node`, `storage`, `done`); `fork_snapshot_id`, `fork_source_vm`, `fork_memory`.
- `vm_snapshots`: id, org_id, vm_id (cascade), name, memory, state (`creating`, `ready`, `error`), error, size_bytes, cpus, memory_mb, disk_gb, networks (jsonb, network ids in NIC order), attachments (jsonb: disk id, mount path, position), reported_at, created_by, created_at.
- `snapshot_disks`: id (= storage file id), snapshot_id (set null on delete), source_disk_id (no FK: the source may be deleted later), disk_name, mount_path, position, size_gb, ready_gb, deleted_at.
- `disks`: `copy_from_snapshot` (fork copies), `restore_from` + `restore_token` (pending in-place restore).
- `limits`: `snapshot_count`.
- Metrics and node-side template state: memory only.

## Failure semantics

| Failure | Behaviour |
|---|---|
| Node crash while hibernating | `Hibernated` not yet set: cold boot, partial `state/` deleted. |
| Firecracker upgraded across a hibernation | Load fails → state discarded, cold boot, error shown. |
| Storage offline during a snapshot | VM stays paused ≤ 120 s, snapshot `error`, VM resumed. |
| Storage offline during a restore | VM stays stopped/hibernated with "restoring disks"; continues when storage is back. |
| Hub offline | Node keeps hibernated/paused/suspended VMs as they are; auto-suspend and wake keep working; snapshots in progress resume after 180 s. |
| Node disk full | Hibernate/snapshot refused, VM untouched. |
| Takeoff fails | Restored VM killed, cold boot, template dropped. |
| Wake while hub changed attachments | Desired change applies after the resume (graceful stop if disks changed). |

## Security

- State and snapshot files hold guest memory (variables, keys in RAM): files 0600, inside the 0700 VM dir, owned by the VM's jail UID; never sent to the hub. Template files contain no owner data (built without keys and variables).
- Clones never share identity: takeoff regenerates SSH host keys and machine id, sets MACs/IPs from the seed and mixes host entropy into the guest RNG. Fork without memory cold-boots with a new instance id (cloud-init regenerates host keys).
- Metrics are VM-level numbers only; nodes are untrusted, so the hub validates and clamps them and stores nothing durable.
- Snapshot disk files follow the disk rules: never exported, deleted only when listed, visible only to the storage server.
- Known limit, documented: root on the node can read state files, as it can read root disks today.

## Permissions

| Action | Object |
|---|---|
| `vm:power` (now also pause, hibernate, resume, auto-suspend setting) | `org/vms/<id>` |
| `vm:snapshot` (take, delete) | `org/vms/<id>` |
| `vm:restore` | `org/vms/<id>` |
| `vm:fork` (+ `vm:create` for the owner) | `org/vms/<id>` |

- Listing snapshots and reading metrics need `vm:read`. *(Our call: hibernate is a power state, so `vm:power`; snapshots are VM sub-resources, so actions on the VM object, no own object type.)*
- `vm:fork` is data access (the fork contains the source's memory and disks): like `vm:connect` and `disk:use`, **admins never get it**.
- Generated: user VM owner + `vm:snapshot`, `vm:restore`, `vm:fork`; team admins of a team VM the same; team members (team subject) + `vm:snapshot`. Built-in admin: + `vm:snapshot`, `vm:restore` (explicit, never `vm:*`).
- All new actions go through the escalation rule.

| | member | team admin (of that team) | admin | owner |
|---|---|---|---|---|
| own VMs: pause, hibernate, auto-suspend, snapshot, restore, fork | yes | yes | yes | yes |
| team VMs: pause, hibernate, auto-suspend, take/delete snapshots | team member | yes | yes | yes |
| team VMs: restore | | yes | yes | yes |
| team VMs: fork | | yes | | |
| every VM: power incl. hibernate, snapshots, restore | | | yes | yes |
| every VM: fork | | | | |

- Audit (colon form): `vm:power` (details `action`: start, shutdown, stop, pause, hibernate, resume; or `autoSuspendMinutes`), `vm:snapshot` (name, memory, disk names), `vm:snapshot-delete`, `vm:restore` (snapshot name, memory), `vm:fork` (on the source; details new VM) plus `vm:create` for the new VM. Node-driven suspend and wake are logged, not audited.

## UI

- **VM detail → power bar:** Start/Resume, Pause, Hibernate, Shut down, Stop; state badges for paused, hibernated, suspended ("suspended — wakes on traffic"). Auto-suspend select (Off, 15 min, 1 h, 4 h, custom minutes).
- **VM detail → Snapshots:** table (name, created, memory, disks, size, state), "Take snapshot" dialog (name, include memory — only when live), row actions Restore (confirm; "memory" only when the shape matches; restart warning), Fork (name), Delete.
- **VM detail → Usage:** CPU % and RAM used/total with a 10-minute sparkline, polled every 10 s.
- **Limits page:** snapshots field. Actions shown only when permitted (`can()`); de + en.

```
dev   running   [Pause] [Hibernate] [Shut down] [Stop]     Auto-suspend: 15 min ▾
Usage   CPU 12 %  ▁▂▅▂▁    RAM 1.8 / 4 GB  ▃▃▄▄▃
Snapshots                                                   [Take snapshot]
NAME            CREATED      MEMORY  DISKS   SIZE     STATE
before-upgrade  10:42        yes     data    5.1 GB   ready    Restore  Fork  Delete
```

## Left to the builder

- Exact REST paths and payloads (start from: `POST …/vms/{id}/pause|hibernate|resume`, `PUT …/vms/{id}/auto-suspend`, `GET/POST …/vms/{id}/snapshots`, `DELETE …/snapshots/{sid}`, `POST …/snapshots/{sid}/restore|fork`, `GET …/vms/{id}/metrics`).
- Whether a `snapshot/load` of a resized root needs `PATCH /drives` before or after resume on Firecracker v1.18; how takeoff keeps cloud-init from treating the next reboot as yet another new instance.
- Test strategy; must include: memory state never resumed after a cold start or with a different root (stale `state/` deleted); graceful stop/delete of a hibernated VM with disks resumes and shuts down, hard stop discards; snapshot waits for `complete` without blocking reconcile, times out and resumes; restore order node → storage → ready and never storage first; memory restore refused on a shape mismatch; fork refused for admins and for another owner, never in the desired list before its disks are ready; idle classifier for every ignored protocol; wake on inbound TCP SYN, not on ARP/DNS; template files jailed read-only, takeoff gives distinct host keys; metrics for another node's VM ignored and values clamped; permission table incl. deny rules and "admins never `vm:fork`".
- **Real-hardware run on Proxmox** (test VM 108 as node of org A, 107 as node of org B, gateway + storage in Docker; nested KVM): hibernate/resume with a running `sleep`-loop writing timestamps (no gap besides the hibernation, clock correct); snapshot with an attached disk on XFS (reflink) and ext4 (copy), restore memory and disk, checksum; fork with memory: distinct hostname, MAC, IP, host keys; auto-suspend at 5 min with `ping` and `dig` running (stays suspended) then `ssh` from a device (wakes); `systemctl restart observatory-node` with two VMs (both resume); second VM of a size boots from template (boot time logged); balloon stats against `free -m` in the guest.
- Update `docs/permissions.md`; add `docs/vm-lifecycle.md` (states, snapshots, filesystem recommendation, auto-suspend rules for users, known limits).

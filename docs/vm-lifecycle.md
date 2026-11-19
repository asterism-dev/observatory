# VM lifecycle

A VM can do more than run or stop: it can be paused, hibernated with its memory on the node's disk, rolled back to a
named snapshot, forked into a new VM, park itself when idle and wake on the first real packet. New VMs start from a
pre-booted template where possible, and the VM page shows live CPU and RAM.

Permissions are in [permissions.md](permissions.md#vm-lifecycle); attached disks in [disks.md](disks.md); the node
host in [nodes.md](nodes.md). Every lifecycle action is also an `observatory vm …` / `observatory snapshot …` command
([cli.md](cli.md#command-reference)).

## Power states

| Desired | Reported | Meaning |
|---|---|---|
| `running` | `running` | The VM runs. |
| `stopped` | `stopped` | Firecracker is gone; the next start is a cold boot. |
| `paused` | `paused` | Standby: the Firecracker process, its RAM, taps and disks stay; the vCPUs are stopped. |
| `hibernated` | `hibernated` | Memory and device state are written to the node's disk, Firecracker is gone and its RAM freed. |
| `running` | `suspended` | Hibernated by auto-suspend; wakes on traffic (see below). |

- **API:** `POST /api/orgs/{org}/vms/{id}/pause`, `…/hibernate`, `…/resume` (and `…/start`, which also resumes a
  paused, hibernated or suspended VM). The VM page shows the power actions that fit the VM's state when permitted.
- **Resume** continues exactly where the VM was: running processes, open files and network connections that did not
  time out meanwhile. The guest agent sets the clock and re-applies SSH keys and variables from the current seed, as on
  every boot; running processes keep their old environment.
- **What hibernate keeps:** the whole guest RAM and device state in `<vm dir>/state/` (one file the size of the VM's
  RAM, plus a small device state file), next to the unchanged root disk. Attached disks are flushed to the storage
  server before the state is written.
- **Space:** hibernate (and a snapshot with memory) needs free space on the node's VM directory of at least the VM's
  RAM + 1 GiB; otherwise it fails with "not enough space on node" and the VM keeps running.
- Paused and hibernated VMs count against limits like running ones.

**Ending a saved memory state.** A hard stop or a restore discards it. A graceful shutdown, deleting the VM, or
changing the NICs or attachments of a paused or hibernated VM **with attached disks** first resumes it and shuts it
down gracefully, so guest writes reach the disks; without attached disks the state is simply discarded. A cold start
always deletes a leftover state first, so a memory state is never resumed against a root disk it was not taken with.

If a memory state cannot be loaded (for example after a Firecracker upgrade on the node, see [Known limits](#known-limits)),
the node discards it, cold-boots the VM and shows the reason as the VM error until the next successful start.

## Snapshots

A snapshot is a named restore point of one VM: its root disk, every attached disk and, unless the VM is stopped,
its memory.

- **Take:** VM page → Snapshots → "Take snapshot", or `POST /api/orgs/{org}/vms/{id}/snapshots`
  (`{"name": "before-upgrade", "memory": true}`). Name `^[a-z][a-z0-9-]{0,30}$`, unique per VM. Memory is only offered
  for a live (running, paused, hibernated or suspended) VM; a stopped VM gets a disk-only snapshot.
- **What is stored:** on the node `<vm dir>/snapshots/<id>/{root.img, vmstate, mem}` (files 0600); on the storage
  server one copy per attached disk, `<snapshot disk id>.img`. The snapshot also records the VM's vCPU, RAM, root
  size, networks and attachments at that moment.
- **Pause:** a live VM is paused and its disks flushed while the node writes the memory and reflinks the root disk; it
  stays paused until the storage server has copied every attached disk, then resumes. Measured on a nested test
  node with 512 MB VMs: 0.7 s without attached disks, about 4 s with a 20 GB disk on XFS, about 6 s with a 2 GB disk
  on ext4; without reflinks the pause grows with the disk data.
  A snapshot that is not `ready` 120 s after the node reported it becomes `error`, its disk copies are deleted and
  the VM resumes. If the hub is gone the node resumes the VM on its own after 180 s, and a node restart during the
  pause resumes it too; in both cases the snapshot becomes `error`, as its disk copies would no longer match the
  memory. The hub also fails a snapshot still waiting for copies when its node disconnects. A snapshot no node picks up within 10 minutes becomes `error` too.
  A snapshot cannot be taken while a restore of the VM is in progress (`409`).
- **States:** `creating`, `ready`, `error`. Only `ready` snapshots can be restored or forked.
- **Size:** the node reports the snapshot's size on disk; it is shown, but not counted against the disk GB limits.
- **Limits:** the `snapshotCount` limit (user, team, org; `null` = unlimited) plus a hard cap of 10 snapshots per VM.
- **Delete:** the snapshot disappears at once (limit freed); the node deletes its directory and the storage server
  its disk copies shortly after. A snapshot that a pending restore or fork still copies from cannot be deleted
  (`409`). Deleting a VM deletes its snapshots.

**Filesystem.** Put the node's VM directory (`paths.vms`, default `/var/lib/observatory-node/vms`) and the storage
server's `/disks` volume on XFS with reflinks (`mkfs.xfs -m reflink=1`, the default in current xfsprogs) or btrfs.
Snapshot, restore and fork then copy root and attached disks as reflinks in well under a second, and copies share
blocks with their source. On ext4 they fall back to full copies: a snapshot then costs the full size of every disk
and pauses the VM for as long as the copies take.

## Restore and fork

**Restore** sets a VM back to one of its snapshots: `POST …/snapshots/{sid}/restore` (`{"memory": true|false}`).

- **Memory restore** is offered only when the VM's vCPU, RAM, networks and attached disks still equal the snapshot's.
  The VM then continues exactly from the snapshot moment, with root disk and attached disks as they were.
- **Disk restore** (otherwise, or by choice) resets the root disk and every snapshot disk that is still attached to
  the VM, then applies the VM's desired power (a running VM boots fresh). The disks are as a power cut at the
  snapshot moment would have left them: the guest is not asked to flush, so writes still in its page cache exist only
  in the snapshot's memory. Run `sync` in the guest before a snapshot meant for disk-only restores.
- Disks are restored in place: they keep their id, name, permissions and audit trail.
- A VM that is not stopped is restarted, so the request needs `restartAllowed`. Order: the node stops the VM and
  replaces its root disk; then the storage server restores each disk; only when all are done does the VM start. While
  storage is unreachable the VM waits with "restoring disks".
- Not possible while a snapshot of the same VM is still `creating` (`409`).

**Fork** creates a new VM from a snapshot: `POST …/snapshots/{sid}/fork` (`{"name": "dev-2", "memory": true|false}`).

- Same owner, same node and the snapshot's vCPU, RAM and root size; new name, management IP, MACs, SSH host keys and
  machine id. It joins the networks the source is on now; a memory fork needs the same NIC count as the snapshot.
- Each snapshot disk becomes a new disk `<disk>-<4 hex>` with the same owner, attached at the same mount path. Limits
  are checked for the VM and the disks.
- The new VM starts once its disks are copied: with memory it continues from the snapshot moment under its new
  identity (see takeoff below); without memory it cold-boots as a new instance.
- A VM whose fork is not provisioned yet (or failed) cannot be deleted (`409`); delete the fork first.

## Auto-suspend

Per VM: VM page → Auto-suspend (Off, 15 min, 1 h, 4 h or custom), or `PUT …/vms/{id}/auto-suspend`
(`{"minutes": 0}` = off, otherwise 5–1440).

- **Idle** means no real network traffic: no IPv4/IPv6 TCP or UDP frame on any of the VM's NICs, in either
  direction, whose ports are not DNS (53), DHCP (67, 68, 546, 547), NTP (123), mDNS (5353) or LLMNR (5355). ARP,
  ICMP (`ping`), ND, MLD, IGMP and frames to broadcast or multicast addresses do not count either.
- **CPU load does not count.** A VM that computes without network traffic (a long build, a training job) is
  suspended after the idle time. Turn auto-suspend off for such VMs.
- Idle detection has a resolution of 30 s, so suspension happens between N and N + 0.5 minutes after the last
  activity.
- **Suspended** VMs are hibernated: their RAM is freed and the GUI shows "Suspended, wakes on traffic".
- **Wake:** the first inbound TCP or UDP packet that would count as activity (e.g. `ssh dev.internal` from a device),
  "Start" in the GUI or API, or any change to the VM's desired state (power, networks, disks, snapshot, restore,
  auto-suspend setting) resumes it. Waking takes a few seconds; the packets that arrive meanwhile are lost, so the
  first TCP connection is delayed by a SYN retransmit (typically 1–3 s). UDP-only protocols should retry.
- Traffic only wakes a VM through its management and network NICs. The NAT NIC (`192.168.64.x`, reachable only from
  the node host) wakes it only if the host still has the guest's address in its ARP cache; use the management
  address instead.
- An explicitly paused or hibernated VM never wakes on traffic; a suspended VM stays suspended across a node restart.

## Templates (fast boot)

The first boot of a new VM restores a pre-booted template instead of booting the image from scratch, and the guest
then takes on its own identity ("takeoff"): hostname, SSH keys and variables, NIC MACs and IPs, root disk size, fresh
SSH host keys and machine id, entropy mixed into its RNG.

- **Per node and shape** (image digest, vCPU, RAM, NIC count, init version). The first VM of a shape cold-boots
  as before and the node builds that shape's template in the background (one build at a time); later VMs of the same
  shape use it. Templates live in `<paths.vms>/.templates/`; at most 4 are kept, the least recently used is evicted.
- Only VMs without attached disks at first boot and with a root disk of at least 10 GB use templates. Every later
  boot is a normal cold boot.
- Template files are root-owned, read-only for the VMs, and hold no owner data: they are built without SSH keys or
  variables.
- Any failure falls back to a cold boot; a failed takeoff also deletes the template.
- Turn it off with `"firecracker": {"fastBoot": false}` in `node.json`.
- **Boot time:** a template boot is meant to take under 1 s to a reachable guest; on a 2-core nested test host it
  measured about 4 s (8 s right after a node restart, with a cold page cache).

## Metrics

The VM page shows CPU % and RAM used of total with a 10-minute sparkline, polled every 10 s
(`GET …/vms/{id}/metrics`, needs `vm:read`).

- **RAM** comes from Firecracker's balloon device statistics (total and available memory as the guest kernel reports
  them, no agent needed). "Used" is total minus available, which is what `free -m` shows as `used` in current
  Ubuntu images. VMs started before this feature have no balloon until their
  next cold boot and show "—".
- **CPU** is the host's view of the Firecracker process (user + system time) over the 10 s interval divided by the
  vCPU count, so 100 % means every vCPU busy. It includes some VMM overhead, e.g. for heavy disk or network I/O.
- The hub keeps the last 60 samples per VM in memory only; a restart of the hub starts empty. Samples older than 30 s
  are not shown as live values (e.g. for a stopped VM).

## Node restart

`systemctl restart observatory-node` (and a host reboot through systemd) no longer reboots the VMs. On stop the node
hibernates every running and paused VM in parallel, within 240 s (VMs not done by then are stopped hard); on start it
resumes them. The installer sets `TimeoutStopSec=300` and `KillMode=mixed` so systemd waits for that; nodes installed
before must re-run the installer ([nodes.md](nodes.md#re-enroll)), otherwise systemd kills the VMs at once and they
cold-boot. A crash of the node service or a power loss still cold-boots them.

## Known limits

- Root on the node host can read state, snapshot and template files, as it can read root disks and attached disks.
- A Firecracker upgrade on the node (re-running the installer) can make existing memory states unloadable:
  hibernated and suspended VMs then cold-boot and their memory is lost, and memory restores from older snapshots boot
  fresh from the restored disks. When upgrading by re-enrolling, shut such VMs down first.
- A node restart after a Firecracker change (hub-driven or not) shuts VMs down cleanly instead of hibernating them (the
  node also detects a changed Firecracker binary itself); they cold-boot and lose their memory, not their disks
  ([nodes.md](nodes.md#updating)).
- Snapshot memory and root disks live on the node: no restore or fork on another node, no export.
- **Any desired change wakes a suspended VM**, including ones that would not need it (for example a changed
  auto-suspend setting).
- **Wake over the NAT NIC** needs a cached ARP entry on the node host; use the management network.
- **Fast-booted VMs share kernel boot-time secrets** with their template and each other until their first reboot:
  the kernel's network hash keys (siphash) and `/proc/sys/kernel/random/boot_id`. SSH host keys, machine id and the
  RNG state are new after takeoff. The image has no VMGenID device that would let the kernel reseed these. Reboot a
  fast-booted VM if this matters for your workload. Forks with memory share the same values with their source.
- **Template build RAM** (one VM of the shape's RAM while the template boots, under a minute) is not counted against
  the node's capacity.
- The under-2-s template boot is a bare-metal target; nested or loaded hosts are slower.
- Metrics history is 10 minutes and lives only in the hub's memory.

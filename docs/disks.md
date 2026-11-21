# Disks

A disk is a block device that outlives VMs. It belongs to an org, is owned by a user or a team (like a VM), lives as
one file on the hub's storage server and is attached to at most one VM at a time. Inside the VM it is formatted (ext4,
on first use) and mounted at the path chosen when attaching, e.g. `/data`. Root disks are not disks in this sense:
they stay on the node and are deleted with their VM.

Permissions are in [permissions.md](permissions.md#disks); the network path in [network.md](network.md).

## Lifecycle

| State | Meaning |
|---|---|
| `creating` | The hub has the disk; the storage server has not reported the file at full size yet. |
| `ready` | The file exists at its size. |
| `growing` | A larger size was set; the storage server has not reported the grown file yet. |
| `releasing` | Detached (or its VM deleted), but the node has not confirmed that it stopped using the disk. |
| `missing` | The storage server had the file and no longer reports it. |

- **Create:** name (`^[a-z][a-z0-9-]{0,30}$`, unique per org), owner (me, or a team I may create disks for), size
  1–4 096 GB. Counts against the disk GB limits of the user, team and org, together with root disks.
- **Copy:** a new disk with the source's size, a new name and owner. Needs `compute.disks.use` on the source. Copying an attached
  or releasing disk is allowed; the copy is then only crash-consistent (the API returns a warning).
- **Grow:** larger only. The new size reaches the VM on its next start: an attached running VM is restarted, so the
  request needs `restartAllowed`. The guest's filesystem is grown on boot. The restart waits until the storage server
  has grown the file; a VM that was stopped at grow time and started before that is not restarted, it gets the new
  size on its next stop and start.
- **Delete:** only when the disk is not attached, not releasing and not the source of a pending copy. The disk
  disappears from the API (limits and name are freed) at once; the row is purged once the storage server reports
  the file gone.
- **Attach / detach:** `PUT /api/orgs/{org}/vms/{id}/disks` sets a VM's whole disk list (up to 8 disks, each with a
  mount path; not `/`, not a system tree such as `/etc` or `/usr`, no duplicates, none inside another). Only disks with the
  same owner as the VM can be attached; copy a disk to move data between owners. A running VM restarts, so the
  request needs `restartAllowed`; a stopped VM takes the change on its next start.
- **Missing:** never recreated blank. A missing disk cannot be attached or copied; it becomes `ready` again when the
  file comes back (for example after restoring the volume).

### Snapshots, restore and fork

A VM snapshot ([vm-lifecycle.md](vm-lifecycle.md#snapshots)) includes every disk attached to the VM. The VM is paused
and its disks flushed, then the storage server copies each disk to `<snapshot disk id>.img` in `/disks` (a reflink on
XFS or btrfs, a full copy otherwise) and the VM resumes once every copy is complete. Snapshot disk files are not
disks: they are not listed, cannot be attached and never get an export. Deleting the snapshot deletes them the same
way as a disk (gone from the API at once, file deleted on the next poll). Deleting a disk is refused (`409`) while a
snapshot copy of it is still in progress.

- **Restore** resets each snapshot disk that is still attached to the VM **in place**: the storage server reflinks
  (or copies) the snapshot file into `.partial/`, renames it over `<disk id>.img` and restarts that disk's NBD server.
  The disk keeps its id, name, owner, permissions and audit trail. The VM starts only after every disk is restored. A
  disk grown since the snapshot gets the snapshot's content and keeps its new size.
- **Fork** turns each snapshot disk into a new disk `<disk>-<4 hex>` owned by the source VM's owner, copied from the
  snapshot file like a disk copy and attached to the new VM at the same mount path. It counts against the disk limits.

### Release and force release

Detaching a disk, or deleting its VM, does not free the disk at once. It stays `releasing` until the node confirms
that the VM is gone and its NBD device closed (the node reports the disks in use with the sequence number of the
desired list it applied; a report from before the detach never counts). Then the export is deleted and the disk can be
attached again, normally within a few seconds.

If the node stays offline (or was deleted), a user with `compute.disks.update` can **force release** a `releasing` disk. This
deletes the export at once: the storage server kills the disk's NBD server, and the old node can never reconnect,
because a later attach gets a new key (the port may repeat). Writes the old VM had not flushed to storage are lost. Use it
only when the node will not come back soon or the VM no longer matters. Deleting a node force-releases its disks.

## Storage server

`cmd/storage` runs one per hub. It keeps one file per disk, `<disk id>.img`, in `/disks`, polls the hub every 2 s
(`GET /api/storage/state`) and reports after each poll (`POST /api/storage/status`). Each poll runs in this order:
apply the state (create, grow, delete files, start copies; disk files are built in `.partial/` and renamed), start or
stop the NBD exports, scan the directory, report. A deletion is therefore always reported after the file is gone, so the hub
never purges a disk whose file is still there.

- It only creates disks the hub marks as new, grows but never shrinks, deletes only disks the hub lists as deleted,
  and never touches files it does not know (orphans stay).
- If the hub is unreachable it keeps its exports and files as they are; nothing is stopped or deleted because a poll
  failed.
- The hub shows "Storage server offline" when it has had no status for 10 s.
- It authenticates with `OBSERVATORY_STORAGE_TOKEN` (at least 32 characters, separate from the gateway token because
  the storage state holds every export key).

### Running it

In the dev setup `make gateway-up` builds and starts it as compose service `storage` next to the gateway (see
[network.md](network.md#running-it)), sharing the gateway's network namespace. `make storage-token` adds a random
`OBSERVATORY_STORAGE_TOKEN` to `deploy/dev/hub.env` if it is missing; the hub and the container read the same file.

Disk files live in the `disks` volume:

- **Filesystem:** put it on XFS created with `mkfs.xfs -m reflink=1` (the default in current xfsprogs) or on btrfs.
  Copies are then reflinks: they take no time of their own (a 10 GB disk with 1 GB of data was `ready` 1.5 s
  after the request, i.e. on the next poll) and share blocks with their source until either is written.
  On other filesystems (ext4) copies fall back to a sparse copy that reads the whole source; a 10 GB disk with 1 GB
  of data took about 8 s in testing, and a multi-TB disk takes hours. Copies run in the background, so exports,
  other disks and status reports carry on meanwhile; the copy stays `creating` until it is done. Use XFS or btrfs
  in production.
- **Used space:** the "used" figure per disk is the file's allocated bytes. Blocks shared between reflinked copies are
  counted for each copy, so the sum over all disks can exceed what the volume really uses; check `df` on the volume
  for the truth.
- **Stopping:** the storage server and its NBD servers are killed hard (never a graceful NBD shutdown), so the nodes'
  `qemu-nbd` reconnects instead of failing writes. `docker compose restart storage` (or stop/start) pauses disk I/O in
  running VMs until it is back; nothing is lost. A gateway restart or crash does not restart the storage server: both
  run in the network namespace of the compose service `netns`, which outlives them.

## Security model

- **Network:** each attached disk gets its own NBD export: an `nbdkit` process on its own port in TCP 10810–11809,
  listening on `10.198.0.1` inside the gateway's network namespace. The gateway accepts these ports only from the
  WireGuard underlay, so only enrolled nodes can connect; devices and the LAN cannot. Nodes drop traffic from their
  VMs to the underlay, so guests cannot reach the exports either.
- **Key:** each export requires TLS with a pre-shared key that the hub generates at attach (32 random bytes, sealed
  with `OBSERVATORY_SECRETS_KEY` in Postgres). The key goes only to the storage server and to the node that runs the
  VM, in that VM's desired state; a node of another org (or another node of the same org) never receives it, and a
  connection without it fails the TLS handshake. On the node the key is in `vm.json` and in
  `<vm dir>/nbd-<disk id>-tls/keys.psk` (both 0600), never in process arguments, logs, node status, API responses or
  audit rows.
- **Fencing:** release confirmation or force release deletes the export; its `nbdkit` is killed and the key is gone.
  A new attach uses a new key; the port may repeat, since the lowest free one is taken.
- **Encryption at rest is not built in.** Disk files are plain on the storage volume. If you need it, put the `disks`
  volume on an encrypted block device (LUKS). Disk traffic is encrypted in transit twice (WireGuard and TLS).

## Known limits

- The node that runs a VM can read and write that VM's attached disks (root on the node host sees the NBD device and
  the key). Org admins who control a node are therefore trusted with the disks attached to VMs on it, as with root
  disks.
- The storage server is trusted like the gateway: it holds every disk file and every export key.
- 1 000 exports (attached disks) per hub, one storage server per hub. Attach returns `409` "no free storage port"
  when the range is used up.
- A storage or gateway restart pauses disk I/O in running VMs. A VM that starts while its storage is unreachable
  shows `error` and retries with the restart backoff, then every 30 s for as long as storage stays unreachable; it
  boots once storage is back.
- Disk errors on a VM (e.g. "NBD connection lost") are shown once the node's NBD device drops; a `qemu-nbd` that is
  still trying to reconnect does not show up as an error.
- Restarting `observatory-node.service` hibernates the node's VMs (disks flushed first) and resumes them on start
  ([vm-lifecycle.md](vm-lifecycle.md#node-restart)); their `qemu-nbd` processes are stopped and reconnected.
- Every VM drive (root and attached disks) uses Firecracker's `Writeback` cache mode, so a guest flush (`fsync`)
  reaches the host and, for disks, the storage server before it returns. This is what makes writes durable across a
  storage restart or a node crash, at the cost of slower `fsync`-heavy workloads.
- No shrinking, shared or read-only attachments, disk-only snapshots (snapshots are always of a whole VM), or
  encryption at rest yet.

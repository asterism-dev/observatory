# Logs

The *Logs* page of the web app browses and searches all logs of your organization in one place, including the logs of
VMs, nodes and image builds that no longer exist. Telemetry must be on ([Telemetry](telemetry.md)); while it is off, the
page's endpoints answer 404 `telemetry is off`. The page is shown to people with the `logs:read` permission: org admins
and owners, or members through a custom role ([Permissions](permissions.md)). Everyone else gets no navigation entry and a
"no access" page on the direct URL.

## Groups and streams

Logs are grouped by the object that produced them, and a group has streams:

| Group | Shown as | Streams |
|---|---|---|
| VM | `vms/<name>` | `service`, `console`, `app`, `system`, `egress` |
| Node | `nodes/<name>` | `observatory-node`, `firecracker` |
| Image build | `builds/<image name>` | one per build (the build id) |

The *Groups* tab lists them with their kind, number of streams and the time of the last entry; filter by kind or name.
Open a group to see its streams, with the time of the last entry. For image builds the start time and result are added
while the build still exists. Below, the entries of the group are listed with the same controls as on the VM and node
pages: free text, level, one stream or all, click a field to filter, time range presets and live tail. The VM and node
*Metrics* tabs link to their group.

## Deleted objects

A group whose VM, node or image no longer exists is marked *deleted*. It stays until the store drops its logs (see
[retention](#limits-and-retention)). The name shown is the last one found in the logs. A name reused later is a separate
group, because a group is identified by the object, not by its name. Build logs of deleted images only exist here: the
build log in the database goes with the image. Builds of private (user) images are never copied to the store and so
never appear.

## Query

The *Query* tab runs a [LogsQL](https://docs.victoriametrics.com/victorialogs/logsql/) query over the groups you tick
(none ticked means the whole organization) and a time range. A result with entries is shown like the entry list; a result
from a `stats` pipe is shown as a table. Examples:

- `error` shows entries containing the word.
- `* | stats by (observatory.vm_name) count()` counts entries per VM.

The hub adds the organization to every query, so a query can never reach another organization's logs. A few LogsQL
constructs that could widen a query are refused with a 400.

## Limits and retention

- Logs are kept for `OBSERVATORY_LOGS_RETENTION` (14 days by default; [deploy.md](deploy.md#configuration)). Older
  ranges are cut to it.
- A query returns at most 1 000 rows, runs at most 30 seconds, and its request is at most 64 KiB.
- Per person: 10 requests per second (burst 40), 3 queries at the same time, 5 live tails. The hub runs at most 4
  explorer queries at once; further ones wait up to 5 seconds, then get a 429. The detail pages and dashboards are not
  affected.
- The group list is cached for 30 seconds per organization.
- Reading logs is not audited.

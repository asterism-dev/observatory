# Telemetry

Nodes send host metrics and logs to the hub, which stores them and shows usage graphs and logs on the node page. The
same pipeline carries each VM's usage, logs and app telemetry to the VM page ([VMs](#vms)).

## What nodes send

Metrics, every 10 seconds:

- `system.cpu.time`, `system.cpu.load_average.1m`, `.5m` and `.15m`
- `system.memory.usage` and `system.paging.usage`
- `system.filesystem.usage` and `system.filesystem.limit` for `/` and the VM directory (once if they share a filesystem)
- `system.network.io` per interface, without the VMs' tap devices, OVS internal devices (`ovs-system`, `br-int`, `genev_sys_*`, `vxlan_sys_*`, `gre_sys`) and `lo`
- `observatory.node.vms.running`

Logs, from two sources:

- `observatory-node`: the node agent's own log, with structured fields such as `vm`, `disk`, `image` and `err`.
- `firecracker`: Firecracker's own log per VM, at level Warning and above.

The guest's serial console (`console.log`) never appears on the node page. It is VM-internal output and is shown only on
the VM page under `vm:metrics` (stream `console`, see below).

## VMs

The VM page has a Metrics tab with usage graphs, the live process list and log streams. VMs on the new guest init send
telemetry; a VM started before this feature sends none until it reboots. Builder VMs send it too, but the hub rejects
it, so it is never stored or shown (the node logs this once per VM).

Upgrading: update the hub before the nodes. An older hub ignores `X-Observatory-VM` and stores a newer node's VM data as
the node's own.

Usage, every 10 seconds, with the node's `system.*` names minus paging: `system.cpu.time`, `system.cpu.load_average.1m`,
`.5m` and `.15m`, `system.memory.usage`, `system.filesystem.usage` and `system.filesystem.limit`, and `system.network.io`.

Log streams:

- `service`: the VM's main process (`cmd.log`), or the image's journal for systemd images.
- `system`: the guest init's own messages after it connects.
- `app`: logs sent by apps to the local endpoint.
- `console`: the serial console, added by the node.
- `egress`: the egress proxy's requests for the VM, added by the node, with `vars=` and `account=` values removed.

Apps send metrics and logs to the guest's local endpoint ([guest.md](guest.md#sending-telemetry-from-apps)).

Limits, per VM and enforced on the node. Over a limit the data is dropped and the VM page shows what was dropped:

| Limit | Value |
|---|---|
| Logs | 100 lines/s, burst 500 |
| App metric series | 2 000 per hour; a histogram point counts one per bucket plus `_sum` and `_count` |
| Buckets | 64 bounds, exponential buckets or quantiles per point |

A new unit, scope or bucket layout under the same name and attributes counts as new series, as it does in the store.
| Usage | only the 8 `system.*` names, 2 000 series |
| Request body | 1 MiB, at most 128 attributes per resource, scope, point or record |
| Connections | 2 concurrent |
| Bytes | 256 KiB/s, burst 2 MiB |

The process list is live only (not stored), at most 500 entries. `cpuPercent` is percent of one core, like `top`, so it
can exceed 100.

## Access

`node:metrics` gates the usage graphs and logs on the node page. Admins and owners have it; members only through a
custom role ([permissions.md](permissions.md)). `vm:metrics` gates a VM's usage, processes and logs: the owner, team
members of a team VM, team admins and org admins and owners have it. Admins read VM telemetry but never connect. VictoriaMetrics leaves out the newest 30 seconds by default (its latency offset), so the charts end slightly before now.

## Configuration

Hub settings are listed in [deploy.md](deploy.md#configuration); the compose file runs the stores
([deploy.md](deploy.md#telemetry-stores)).

- `OBSERVATORY_METRICS_URL` and `OBSERVATORY_LOGS_URL`: both or neither. Unset turns telemetry off: the OTLP endpoints
  answer 404 and the node page hides the graphs and logs. Nodes then drop VM telemetry and check again every 5 minutes.
- `OBSERVATORY_METRICS_RETENTION` (default `30d`) and `OBSERVATORY_LOGS_RETENTION` (`14d`).
- `OBSERVATORY_METRICS_MAX_HOURLY_SERIES` (`500000`) and `OBSERVATORY_METRICS_MAX_DAILY_SERIES` (`2000000`); series
  beyond the limits are dropped.

For development, `make telemetry-up` and `make telemetry-down` start and stop the stores (see the README).

## Endpoints

`POST /otlp/v1/metrics` and `POST /otlp/v1/logs` take OTLP/HTTP protobuf (`application/x-protobuf`, gzip allowed) with
a bearer node credential; in this version nothing else is accepted. The body limit is 4 MiB. The hub sets
`observatory.org/node/node_name` and drops any sender-supplied attributes starting with `observatory`.

A node sends a VM's data with `X-Observatory-VM: <vm id>` and `X-Observatory-Stream`: `service`, `console`, `app`,
`system` or `egress` for logs, `usage` or `app` for metrics (else 400). The VM must exist in the node's org and be
assigned to that node, and must not be a builder VM (else 403). The hub then also sets `observatory.vm` and
`observatory.vm_name`, and on logs `observatory.stream`; VM logs are stored under the stream fields
`observatory.org,observatory.vm,observatory.stream`, apart from the node's own logs.

`GET /api/orgs/{org}/vms/{id}/processes` (needs `vm:metrics`) returns the running VM's live process list, at most 500
entries. `cpuPercent` is percent of one core over a 200 ms sample, like `top`, so it can exceed 100 on a multi-core VM.

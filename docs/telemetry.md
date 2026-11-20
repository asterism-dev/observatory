# Telemetry

Nodes send host metrics and logs to the hub, which stores them and shows usage graphs and logs on the node page.

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

The guest's serial console (`console.log`) is not shipped. It is VM-internal output, and org admins must not see inside
VMs.

## Access

`node:metrics` gates the usage graphs and logs on the node page. Admins and owners have it; members only through a
custom role ([permissions.md](permissions.md)). VictoriaMetrics leaves out the newest 30 seconds by default (its latency offset), so the charts end slightly before now.

## Configuration

Hub settings are listed in [deploy.md](deploy.md#configuration); the compose file runs the stores
([deploy.md](deploy.md#telemetry-stores)).

- `OBSERVATORY_METRICS_URL` and `OBSERVATORY_LOGS_URL`: both or neither. Unset turns telemetry off: the OTLP endpoints
  answer 404 and the node page hides the graphs and logs.
- `OBSERVATORY_METRICS_RETENTION` (default `30d`) and `OBSERVATORY_LOGS_RETENTION` (`14d`).
- `OBSERVATORY_METRICS_MAX_HOURLY_SERIES` (`500000`) and `OBSERVATORY_METRICS_MAX_DAILY_SERIES` (`2000000`); series
  beyond the limits are dropped.

For development, `make telemetry-up` and `make telemetry-down` start and stop the stores (see the README).

## Endpoints

`POST /otlp/v1/metrics` and `POST /otlp/v1/logs` take OTLP/HTTP protobuf (`application/x-protobuf`, gzip allowed) with
a bearer node credential; in this version nothing else is accepted. The body limit is 4 MiB. The hub sets
`observatory.org/node/node_name` and drops any sender-supplied attributes starting with `observatory`.

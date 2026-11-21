# Telemetry, part 2: VMs

## Intent

Follow-up to `2026-11-20-telemetry-nodes-design.md` (part 1: backbone + nodes, PR #38). Every VM delivers the same kind of telemetry the nodes do — CPU, memory, filesystem usage, network, logs — plus a live process list, and users can send their own metrics and logs from their apps through a standard local OTLP endpoint. Everything travels the same way (OTLP) into VictoriaMetrics/VictoriaLogs and is shown on the VM detail page. The in-memory VM samples on the hub (`fleet.Metrics`, measured by the node from outside) go away.

Success: the owner of `vm01` opens the VM, tab "Metrics", sees CPU/memory/disk/network over the last 6 h with the same interactions as the node page, a live process list refreshing every 5 s, and logs filtered to stream `service` showing their app's stdout. Their Node.js app, started with the stock OTel SDK and no configuration, sends `orders_total` and its logs; the logs appear under stream `app`, the metric is queryable in the store with `observatory_vm=<id>`. An org admin sees the same for every VM of the org but still cannot connect. A VM that floods logs is capped and the page says how many lines were dropped; other VMs are unaffected.

## Decided with the user

- **Who sees what:** a new permission **`vm:metrics`** opens VM telemetry (usage graphs, process list, all log streams). Owner of a user VM, members of the owning team for team VMs, and org admins/owners for every VM. Admins still never get `vm:connect`. This is a deliberate exception to "admins never look inside a VM": they may read its telemetry, including logs.
- **Separate log streams**, assigned at ingest: `service` (CMD/ENTRYPOINT output), `console` (serial console: kernel + init), `app` (logs the app sends via OTLP), `system` (the guest init's own messages), `egress` (the egress proxy's per-request access log). All under `vm:metrics` for now; the separation allows per-stream permissions, retention or forwarding later.
- **Process list: live only** (like `top`): fetched on demand while the page is open, refreshed every 5 s, never stored. A stored top-N history may come later.
- **Own metrics/logs:** the guest init listens on dedicated ports `localhost:14318` (OTLP/HTTP) and `localhost:14317` (OTLP/gRPC) — not the standard 4317/4318, so an image shipping its own collector never collides — and points `OTEL_EXPORTER_OTLP_ENDPOINT` at them for CMD, SSH sessions and, for systemd images, the units' environment, so a stock SDK works without configuration. Metrics and logs only; traces come with part 6.
- **Egress stream redaction:** the node removes secret names (`vars=…`) and integration account names (`account=…`) from egress lines before they enter the stream, keeping the promise in `docs/permissions.md` that admins never see secret names.
- **systemd images:** when the image runs its own systemd, the guest init follows the image's journal and puts it into stream `service` (unit name as a field); without systemd, `cmd.log` as today.
- **VMs on an old guest init** (still running after a node update) deliver no telemetry until their next boot; their usage box and list meters stay empty meanwhile. Accepted and documented; no fallback to outside sampling.
- **Limits per VM:** at most 2 000 own time series and 100 log lines per second (short bursts allowed); excess is dropped and the VM page shows how much was dropped.
- **Guest-measured usage** every 10 s: CPU (total and per core), memory, filesystem usage per VM filesystem, network per interface, load — replaces the node's outside measurements.
- **VM page:** like the node page — a "Metrics" tab (only with `vm:metrics`) with four graphs (same chart component and interactions), the live process list, and logs with a stream selector (all / service / console / app / system / egress), collapsed rows = message only, expandable JSON, field chips, live tail. The compact usage box in the overview and the meters in the VM list show the latest values from the new source. Users' own metrics get no graphs in part 2 (that is part 5).
- **Transport (approach A):** guest → node over vsock, node → hub over OTLP with its credential.
- Node page stays as in part 1 (no guest console, no egress log).

## Goals / non-goals / anti-goals

- Goals: guest collector + local OTLP receiver in the guest init; vsock forwarding through the node with VM identity; console and egress streams from the node; per-VM limits on the node; hub ingest of VM data with VM stamping; `vm:metrics`; VM usage/logs/tail/processes API; VM Metrics tab; VM list/overview usage from the store; removal of `fleet.Metrics` and the node's outside VM sampling.
- Non-goals: traces; stored process history; graphs/dashboards for users' own metrics (part 5); Grafana access (part 3); per-stream permissions; per-org series limits; telemetry from the legacy (non-guest-init) runtime.
- Anti-goals: a VM can write telemetry as another VM, node or org; a VM can bypass its limits by talking to vsock directly; one VM's volume degrades the store or the node for others; telemetry blocks or breaks the VM's app, its boot, SSH, or the node agent; admins gain a way to execute or connect into VMs.

## Approaches

- **A — vsock to the node, node forwards (chosen).** The guest init sends OTLP over a guest-initiated vsock connection (new port 1052, same pattern as the token service on 1051). The node knows the VM from the per-VM socket in its jail, adds console and egress streams, enforces the limits, and forwards to the hub with its credential; the hub checks the VM runs on that node. Process list: hub → node over the existing connection → guest agent (port 1050, new op). No tokens or network route in the VM; works for VMs without internet.
- B — VM sends directly to the hub over the network with a per-VM token: tokens readable by the VM's root, needs a VM→hub route that doesn't exist on purpose, limits only on the hub. Rejected.
- C — node pulls from the guest periodically: the guest would have to buffer pushed app data until polled; more state and latency, no gain over A. Rejected.

## Design

### Guest (internal/guestinit)

- **Collector**: every 10 s reads `/proc/stat`, `/proc/meminfo`, `/proc/loadavg`, `/proc/net/dev` and statfs of the image root and attached disks' mount points (the same metric names/units as the node's host metrics in part 1, so queries and charts are shared: `system.cpu.time{cpu,state}`, `system.memory.usage{state}`, `system.filesystem.usage/limit{mountpoint}`, `system.network.io{device,direction}`, `system.cpu.load_average.*`). Exclude `lo`. *(Our call: reuse the part-1 parsers by moving them to a shared package if that doesn't drag node code into the init; otherwise copy the small parsers.)*
- **Service log**: images without systemd — the supervisor (`supervise.go`) keeps writing `cmd.log` as today and additionally emits each line as a log record, stream `service`. Images with systemd (`StartImage` runs the image's systemd as PID 1) — the guest init follows the image's journal (e.g. `journalctl -f -o json` inside the image's namespaces, or reading the journal files) and emits each entry as stream `service` with the unit (`_SYSTEMD_UNIT`) as a field and the journal priority mapped to the level; kernel messages (already in `console`) are skipped. Lines over 16 KiB are truncated. *(Our call: truncation; mechanism left to the builder.)*
- **System log**: the guest init's own messages (currently `fmt.Fprintf(os.Stderr, …)` to the console) become records, stream `system`. Until the vsock forwarding is connected (early boot) they go to the console as today; once connected they go only to `system`, so they are not duplicated in `console` and not counted twice against the limit. *(Our call.)*
- **Local OTLP receiver**: `127.0.0.1:14318` (HTTP, protobuf and JSON as the OTLP spec allows) and `127.0.0.1:14317` (gRPC) in the VM's network namespace (the image shares it). Accepts metrics and logs; traces answer "unimplemented"/404. Records from here are stream `app`. If a port is nevertheless taken, that listener is skipped with a `system` log line; the app is never blocked. `OTEL_EXPORTER_OTLP_ENDPOINT=http://localhost:14318` and `OTEL_EXPORTER_OTLP_PROTOCOL=http/protobuf` are added to the CMD environment, SSH sessions and (systemd images) the default unit environment, unless the image/env already sets them.
- **Forwarding**: one OTLP/HTTP-like exchange over vsock to the node, port 1052 (guest-initiated, reconnect with backoff). Bounded in-memory buffer in the guest (drop oldest), never blocks the app or the init. The guest marks each request with its stream class (`service`, `system`, `app`, or `usage` for metrics); it never sets `observatory.*`. *(Builder: framing — plain HTTP/1.1 over the vsock connection is acceptable.)*
- **Process list**: new agent op `processes` on port 1050: returns PID, PPID, user, command line (truncated to 512 bytes), state, CPU % since the previous call (or over a short sample), RSS. Processes of the image's PID namespace and the init's own helpers (SSH sessions, integrations) are included; kernel threads excluded. Max 500 entries, sorted by CPU then RSS.

### Node

- **vsock listener 1052** per VM, created like the token listener (`listenTokens`), closed with the VM. Each connection is attributed to that VM.
- **Limits** (the node is the trust boundary; the guest's root could bypass the init): per VM, log records 100/s with a burst of 500 (token bucket); metric series: at most 2 000 distinct series (metric name + attribute set) per VM per hour window, new series beyond that dropped, existing ones still accepted; request body ≤ 1 MiB. Drops are counted per VM and reason and exported as node metrics `observatory.vm.telemetry.dropped{vm,kind}` (kind ∈ logs, series, body). Only stream classes `service`, `system`, `app`, `usage` are accepted from the guest; anything else is rejected.
- **Console stream**: the node tails each running VM's `console.log` (same tail approach as the Firecracker log in part 1) and emits records stream `console`. Subject to the same per-VM log limit.
- **Egress stream**: the egress proxy's per-request lines (part 1 keeps them on stderr only) become records stream `egress` for that VM, sent through the VM path (not the node's own logs), subject to the per-VM limit; they stay on stderr as well. Before emitting, the node strips the secret names (`vars=…`, `internal/egress/terminate.go`) and integration account names (`account=…`, `retry.go`) from the record; the stderr line is unchanged.
- **Forwarding to the hub**: VM data goes to the existing `POST /otlp/v1/metrics|logs` with the node credential and a header `X-Observatory-VM: <vm id>` plus `X-Observatory-Stream: <stream>`; one request per VM and stream class. Uses the same bounded, retry-on-retryable buffering as the node's own logs (part 1), but separately per VM so one VM's backlog can't evict another's — *(our call: a shared bound of 10 000 records across VMs with fair per-VM caps is acceptable if simpler).*
- **Process list**: new control message from the hub (`processes` request/reply with the VM id) → guest agent call → reply. Timeout 3 s.
- **Node queries exclude VM data**: VM metrics carry `observatory.node` too and share metric names with the node's host metrics, so the part-1 node usage queries add `observatory_vm=""` (and node log queries exclude records with `observatory.vm`). *(Our call.)*
- **Removed**: the node's outside VM sampling (`metricsLoop`/`sendMetrics`, `proto.VMMetrics`, message type `metrics`) once the guest path exists. Keep the hub tolerant of old nodes that still send `metrics` (ignore it).

### Hub

- **Ingest**: when `X-Observatory-VM` is present, the hub checks the VM exists, belongs to the node's org and is assigned to that node (else 403); `X-Observatory-Stream` must be one of `service, console, app, system, egress` for logs or `usage, app` for metrics (else 400). Stamping adds resource attributes `observatory.vm`, `observatory.vm_name`, and for logs `observatory.stream`; the existing strip of sender `observatory*` attributes applies unchanged. VictoriaLogs stream fields for VM logs: `observatory.org, observatory.vm, observatory.stream`.
- **Permission**: `vm:metrics` on `org/vms/*`: granted wherever `vm:read` is granted to owners and team members today, plus org admins/owners for all VMs (explicit, like the other admin VM actions — never `vm:*`). Not in the plain member rule. `docs/permissions.md` updated.
- **API** (all `vm:metrics`):
  - `GET /api/orgs/{org}/vms/{id}/usage?from&to` — same shape as the node usage (`t`, `charts` cpu/mem/disk/net); memory chart auto-scales.
  - `GET /api/orgs/{org}/vms/{id}/logs?from&to&q&level&stream&field=k:v&limit` and `…/logs/tail?…` (SSE) — like the node endpoints, `stream` instead of `source` (validated against the five names; empty = all).
  - `GET /api/orgs/{org}/vms/{id}/processes` — proxies the live list from the node; 409 when the VM isn't running, 504 on timeout.
  - `GET /api/orgs/{org}/vms/{id}/telemetry-drops?from&to` — dropped counts per kind for the hint (or fold into `usage` — *our call*).
  - VM JSON `usage` (overview box and VM list meters) comes from one instant query per request for the listed VMs (latest CPU %, memory used/total, root filesystem used/total), only for VMs the caller may `vm:metrics`; `canTelemetry` on VM JSON.
- **Removed**: `fleet.Metrics`, `DropMetrics`, `GET /api/orgs/{org}/vms/{id}/metrics`, `VMUsage.vue`'s polling of it.

### Web

- VM detail: new tab "Metrics"/"Metriken" after "Overview", only with `canTelemetry`: `NodeUsage`-style usage (reuse/generalise `NodeUsage`, `TimeSeriesChart`), process table (PID, command, CPU %, memory, user; pause button; refresh 5 s while visible), logs (generalise `NodeLogs`: stream selector instead of source filter), dropped-data hint when > 0 in the range.
- Overview usage box and VM list meters keep their look, fed by the new `usage` JSON (disk becomes filesystem usage instead of allocated image size; the "disk hint" text changes accordingly).
- de + en strings.

### Docs

`docs/telemetry.md`: VM section (what the guest sends, streams, limits, the local endpoint and env vars, `vm:metrics`, admins may read VM telemetry); `docs/guest.md`: local OTLP endpoint for app developers with a 5-line example; `docs/permissions.md`.

## Constraints

- Telemetry must never block or break the VM's app, boot, SSH or the guest agent; all guest-side buffers are bounded and drop.
- `observatory.*` and the stream are trusted only when set by the node/hub; the guest's stream class is validated by the node against the guest-allowed set.
- Admins get no new way to execute in or connect to VMs.
- Works for VMs without network access.
- Retention from part 1 applies. The global series limit default rises to `OBSERVATORY_METRICS_MAX_HOURLY_SERIES=500000` (daily 2 000 000) because each VM may add up to 2 000 own series; docs state that many busy VMs can still exhaust it (per-org limits remain a non-goal). *(Our call.)*
- VMs keep working on an old guest init (no telemetry until reboot); docs mention it.

## Left to the builder

vsock framing details; the gRPC receiver implementation (grpc-go is already a module dependency through the OTLP packages; keep the init binary size in mind — if it grows by more than a few MB, report back); exact process CPU % sampling; tail implementation reuse; query details; how the old `metrics` message is retired across a node update; test approach (fake vsock with unix sockets as in existing guest/node tests).

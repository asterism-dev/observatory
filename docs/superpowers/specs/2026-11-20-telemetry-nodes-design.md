# Telemetry, part 1: backbone and nodes

## Intent

The hub becomes the single place where telemetry arrives, is stored and is shown. Everything comes in the same way (OTLP): node logs and metrics now; VM metrics, processes, logs and users' own metrics next; builder logs, and later per-run logs and events for functions and automations. The hub shows it on its own pages; an external Grafana can be connected as a second view onto the same data.

The whole effort is split into sub-projects (agreed with the user):

1. **Backbone + nodes** (this document)
2. VMs: collector in the guest init (CPU, memory, filesystem usage, processes, service log), local OTLP endpoint for users' own metrics, vsock → node → hub, VM detail page; replaces the in-memory `fleet.Metrics`
3. Grafana: org and platform tokens, Prometheus- and VictoriaLogs-compatible endpoints, docs
4. Builder logs move to VictoriaLogs; `image_build_log_chunks` goes away
5. Users' own dashboard pages
6. Later: OTLP forwarding to an external target; traces and per-run logs for functions/automations

Success for part 1: an org admin opens node-107, picks "6 h", hovers over the CPU graph and reads the exact values at 14:32 in all four graphs at once, drags a range to zoom in, sees the node's log lines of that range below, opens one line as JSON, clicks `"vm": "vm01"` and sees only that VM's lines. With "live" on, new lines appear within seconds. After a hub restart the history is still there.

## Decided with the user

- The hub stores and shows telemetry itself; external Grafana is optional (sub-project 3); forwarding (B) comes later.
- Storage: **VictoriaMetrics + VictoriaLogs** (Prometheus + Loki considered and rejected, see Approaches).
- Ingest is OTLP for every sender.
- Attributes: senders' attributes are kept as they are (needed for users' own metrics); only the reserved `observatory.*` namespace is set by the hub and overwritten if a sender sets it.
- A limit on the number of time series protects the store; global first, per org only if needed.
- Node logs: **only observatory's own logs** — the `observatory-node` service and Firecracker's own messages — not the host's whole journal.
- A **new permission `node:metrics`** on `org/nodes/*` gates the node's usage graphs and logs. *(Our reading: one permission for both.)* Admins and owners get it through `node:*`; members don't by default and can be granted it through a custom role.
- The **guest serial console is not shown on the node page** (it is VM-internal; admins hold `node:*` but deliberately never get a way to look inside a VM, see `docs/permissions.md`). It belongs to the VM page in part 2.
- Logs are structured (JSON-like records with fields). The UI shows a collapsed line as time, level, source and **message only**; clicking expands the full JSON; clicking a field in the JSON adds it as a filter chip.
- Graphs: shared hover across all graphs on a page with a tooltip (exact time + every series value), axes with values, legend click hides a series, drag to zoom (logs below follow the zoomed range), double-click resets. Range picker 1 h / 6 h / 24 h / 7 d. Approved mockup: `.superpowers/brainstorm/51499-1795189183/content/node-page-v3.html` (local, not committed).
- Technical picks the user approved: uPlot for graphs; OTel Go SDK over OTLP/HTTP from the node with its existing credential (not over the yamux connection); host metrics read from `/proc` building on `internal/node/procfs.go`; the hub parses OTLP, stamps `observatory.*` and forwards to the stores' OTLP endpoints; org scoping on queries by label/field filters (not VictoriaLogs tenants); live logs via the store's tail endpoint, passed on as SSE like build logs; the node buffers a bounded amount while the hub is unreachable and drops the oldest beyond that.

## Goals / non-goals / anti-goals

- Goals: VictoriaMetrics and VictoriaLogs in prod compose and dev; OTLP ingest on the hub; node sends host metrics and structured logs; node detail page with graphs and logs; shared graph component; retention and series limit configurable.
- Non-goals (this part): hub's own logs, the host journal, VM-internal data, Grafana access, traces, alerting, forwarding, migrating build logs, replacing `VMUsage.vue`/`fleet.Metrics` (part 2).
- Anti-goals: a VM or node can write data into another org or impersonate another node/VM; one noisy sender (or a high-cardinality label) degrades the store for everyone; telemetry outages break node operation (sending must never block or crash the agent).

## Approaches

- **VictoriaMetrics + VictoriaLogs (chosen):** two single binaries as containers beside the hub, like the registry. Both accept OTLP natively, small RAM/disk footprint, retention is one flag. Grafana later uses its stock Prometheus datasource plus the official VictoriaLogs plugin (not Loki-compatible).
- Prometheus + Loki (+Tempo): Grafana-native without a plugin, Loki has tenants built in — but heavier and more configuration to operate. Rejected for a deployment of "a few nodes, a few dozen VMs".
- Everything in Postgres: the hub would have to implement PromQL/LogQL itself for Grafana. Rejected.

Transport: OTLP/HTTP from the node (chosen) vs. a stream on the existing yamux connection — rejected because every later sender (builder, functions) should use the same standard path and SDK.

## Design

### Components

- **Compose (prod, `deploy/compose.yaml`):** services `victoriametrics` and `victorialogs`, pinned versions with digest (like the registry), own volumes, not published on the host; only the hub talks to them. Retention from `.env`: `OBSERVATORY_METRICS_RETENTION` (default `30d`) and `OBSERVATORY_LOGS_RETENTION` (default `14d`), passed as the stores' retention flags. Series limit via VictoriaMetrics' `-storage.maxHourlySeries`/`-storage.maxDailySeries` from `.env` with a sensible default. *(Our call: exact defaults for the series limit.)*
- **Dev (`deploy/dev/compose.yaml`):** same two services under a profile (`make telemetry-up` / `telemetry-down`, the pattern of `registry-up`), published on localhost ports; a make target appends the hub URLs to `deploy/dev/hub.env` if missing.
- **Hub config:** `OBSERVATORY_METRICS_URL` and `OBSERVATORY_LOGS_URL` (the stores' base URLs). Both unset → telemetry is off: OTLP endpoints answer 404, the node page hides the usage and log sections. Setting only one is a config error.

### Ingest (hub)

- `POST /otlp/v1/metrics` and `POST /otlp/v1/logs`, OTLP/HTTP protobuf (gzip accepted), `Authorization: Bearer <node credential>` resolved with `store.NodeByCredential`. Body size limit (e.g. 4 MB after decompression). *(Our call.)*
- The hub unmarshals with `go.opentelemetry.io/proto/otlp`, removes every resource and data-point/record attribute starting with `observatory.`, sets resource attributes `observatory.org` (node's org id), `observatory.node` (node id) and `observatory.node_name`, re-marshals and forwards to VictoriaMetrics `/opentelemetry/v1/metrics` and VictoriaLogs `/insert/opentelemetry/v1/logs` (stream fields: `observatory.org`, `observatory.node`, `service.name`).
- The forwarding answer is passed back (2xx / 5xx); the hub does not buffer. The node keeps log records that failed retryably (no response, 429, 5xx) and resends them with the next batch, bounded at 10 000 records (oldest dropped); batches rejected with any other status are dropped. Metrics are not resent, a gap is acceptable.
- The credential-based identity is designed so part 2 can add a VM identity (the node forwards guest data and states the VM id, the hub checks the VM is on that node) without changing the endpoints.

### Node

- OTel Go SDK: `MeterProvider` with a periodic reader every 10 s over `otlpmetrichttp`; `LoggerProvider` with a batch processor over `otlploghttp`; both with a bounded queue (drop oldest when full, never block). Resource: `service.name=observatory-node`, `service.version`.
- Endpoint: the http(s) origin of the node's WebSocket connect URL (`cfg.Hub.URL`, e.g. `ws://hub:8090/api/nodes/connect` → `http://hub:8090`) + `/otlp`; the node already reaches that address, so no protocol or config change. `OBSERVATORY_NODE_URL` (hub-side, for image pulls) is not used. *(Our call.)*
- **Logging:** the agent moves from `log.Printf` (≈72 call sites in `internal/node`, `internal/provider`, `cmd/node`) to `log/slog` with fields (`vm`, `disk`, `image`, `err`, …). The default handler fans out to stderr (journald keeps working, text format) and to the OTel slog bridge (`otelslog`). Message stays short and constant (`"image pull failed, retrying"`), details go into fields.
- **Firecracker output:** today Firecracker's stdout/stderr and the guest's serial console share each VM's `console.log`. Firecracker's own log is moved to a separate file per VM through its logger configuration (`log_path`, level `Warning` or `Info`); the node tails that file and emits one record per line: `source=firecracker`, `vm=<id>`, `vm_name`, level from Firecracker's level field. `console.log` stays as it is and is not shipped in part 1. Records from the agent itself carry `source=observatory-node`.
- **Host metrics** (OTel semantic convention names, read from `/proc` and `statfs`, extending `procfs.go`):
  - `system.cpu.time` per state (user, system, iowait, idle, …) and per cpu; the UI derives utilisation
  - `system.cpu.load_average.1m/5m/15m`
  - `system.memory.usage` per state (used, cached, free), swap
  - `system.filesystem.usage`/`limit` per mount for `/` and the VM directory's filesystem (`cfg.Paths.VMs`)
  - `system.network.io` per interface and direction (physical, `wg*`, `vmm0`; tap devices excluded)
  - `observatory.node.vms.running`
- Metric naming in the store follows VictoriaMetrics' Prometheus naming mode so later Grafana queries look conventional. *(Our call; builder may adjust the exact flag.)*

### Query (hub → web)

Part 1 exposes narrow, node-scoped endpoints, not a raw query proxy (a raw proxy only arrives with Grafana in part 3):

- `GET /api/orgs/{org}/nodes/{id}/usage?from&to` — the hub runs fixed MetricsQL queries (`query_range`, step chosen to give ~300 points), always with `extra_label=observatory_org=<org>` and the node filter, and returns `{t: [...], series: {cpu: {...}, mem: {...}, disk: {...}, net: {...}}}` ready for uPlot.
- `GET /api/orgs/{org}/nodes/{id}/logs?from&to&q&level&source&field=k:v…&limit` — LogsQL built server-side (user input only as quoted values, never raw query text), `extra_filters` on `observatory.org` and the node; newest first, default limit 500.
- `GET /api/orgs/{org}/nodes/{id}/logs/tail?…` — same filters, VictoriaLogs tail endpoint passed on as SSE (same pattern as `followBuildLog`).
- Authorization: `node:metrics` on the node (new action in `internal/authz/catalog.go` on `ObjNodes + "/*"`, covered by the admin `node:*` rule, not in the member rule; `docs/permissions.md` updated). The node JSON tells the web app whether the caller holds it, so the sections are hidden otherwise.

### Web

- New shared `TimeSeriesChart` component on uPlot (`uplot` npm dependency): synced cursor across charts on a page (uPlot cursor sync), tooltip with time + all values, axes, legend toggle, drag-to-zoom emitting the selected range, double-click reset; colours from lumen tokens; works in light and dark.
- `NodeDetailView.vue` gets a "Usage" section (range picker 1 h / 6 h / 24 h / 7 d, four charts: CPU user/system/iowait %, memory used/cache, disk % per filesystem, network per interface) and a "Logs" section: search, level filter, source filter (`observatory-node` / `firecracker`), field-filter chips, live/pause toggle, collapsed line = time · level · source · message, click expands pretty JSON with clickable attribute lines. Zoom on the charts sets the log range. Auto-refresh every 10 s while the tab is visible and the range ends at "now".
- de + en strings.

### Docs

`docs/telemetry.md` (what is collected, retention, limits, ports, turning it on), `docs/deploy.md` (new services and `.env` vars, upgrade note), README dev section (`make telemetry-up`).

## Constraints

- Sending telemetry must never block or crash the node agent; a missing/unreachable hub endpoint only drops data.
- No new secrets: nodes reuse their credential; the stores are reachable only by the hub.
- `observatory.*` attributes are trusted only when set by the hub.
- Existing behaviour (journald logs on the node, `fleet.Metrics` for VMs, build logs) stays as it is.

## Notes for later parts

- Part 2 must decide who sees a VM's serial console and in-guest logs: `vm:read` alone would let org admins look inside every VM, which the permission model avoids today.
- Agent log lines name VMs (`vm`, `vm_name`); holders of `node:metrics` see the names of all VMs on that node. Accepted, since it is granted to admins by default.

## Left to the builder

Exact SDK/exporter configuration and queue sizes; the console tail mechanism (offset handling across restarts, rotation); LogsQL/MetricsQL details; the step/point calculation; chart styling within the approved mockup; test approach (fake store server for the hub proxy, the OTLP stamping as a unit test).

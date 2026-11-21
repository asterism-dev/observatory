# Dashboards

A dashboard is a page of charts built from the metrics in the hub's store: the built-in VM and node usage
([Telemetry](telemetry.md)) and whatever your apps send to their VM's local endpoint
([guest.md](guest.md#sending-telemetry-from-apps)). Open *Dashboards* in the web app's navigation. Telemetry must be on;
without a metrics store the panels show an error.

## Scopes: who sees and edits a dashboard

A dashboard is in one of three scopes, chosen when you create it and changeable later under *Edit*, *Visibility*:

| Scope | Who sees it | Who edits and deletes it |
|---|---|---|
| Private | only you | only you |
| Team | members of the team | team admins, org admins and owners, and its creator while they are a member of the team |
| Org | every member | org admins and owners, and its creator |

- Any member creates private dashboards and dashboards of the teams they belong to; only org admins and owners create
  org dashboards.
- Moving a dashboard to another scope ("sharing") needs edit rights on it and the right to create in the target scope.
  Only its creator can make a dashboard private, so an admin cannot take a shared dashboard away from its team.
- No built-in role reaches another member's private dashboards, not even an owner's. They are deleted when their member
  leaves the organization.

The permission rules behind this are in [Permissions](permissions.md#dashboards).

## Whose data a panel shows

A dashboard holds queries, not data. Every panel runs as the person viewing it, and the hub restricts each query to
the data that person may read:

- Org admins and owners (org-wide `vm:metrics` and `node:metrics`) see all metrics of the organization, including those of
  deleted VMs.
- Everyone else sees only series of VMs they hold `vm:metrics` on: their own VMs and their teams' VMs. Node series are
  included only for viewers with `node:metrics` on all nodes (a custom role). A viewer with neither sees empty panels.

The restriction is applied by the store to every series selector in the query, so query text such as
`{__name__=~".+"}`, `label_replace`, subqueries or `absent()` cannot widen it. The metric-name suggestions in the
editor are restricted the same way. So when Alice shares a dashboard with the org, Carol sees the same panels but only
with data of the VMs Carol may read.

## Panels

Each panel has a title, a MetricsQL query (a superset of PromQL), a type, a unit and a width.

- **Time series**: one line per series over the selected range. Hovering one chart shows the same moment on every
  chart; drag across a chart to zoom all of them, double-click to reset. Click a legend entry to hide its line.
- **Stat**: the latest value at the end of the range. If the query returns several series, the panel shows the first
  one and says how many more there are; wrap the query in `sum(...)` for a total.
- **Unit**: none, percent (values 0–100), bytes, bytes/s, seconds or requests/s; it formats axis and values.
- **Width**: half or full. On narrow screens every panel is full width.

The range presets are 1 hour, 6 hours, 24 hours and 7 days; an unzoomed range moves with the current time and
refreshes every 30 seconds. In the editor, type part of a metric name for suggestions, and choose *Run* (or press
Ctrl+Enter) for a preview before you apply the panel. Reorder panels with the arrow buttons.

Series are labelled by their VM name (`observatory_vm_name`) and their remaining labels. Metric names follow the
Prometheus conventions of the store: the OpenTelemetry metric `system.cpu.time` becomes
`system_cpu_time_seconds_total`, an app counter `orders` with unit `1` becomes `orders_total`.

## Examples

CPU cores in use per VM (works without custom metrics, and is the example a new dashboard offers):

```
sum(rate(system_cpu_time_seconds_total{state!="idle"}[5m])) by (observatory_vm_name)
```

Memory in use per VM, unit bytes:

```
sum(system_memory_usage_bytes{state="used"}) by (observatory_vm_name)
```

Orders per second by status from an app counter, unit requests/s:

```
sum by (status) (rate(orders_total[5m]))
```

95th percentile latency from an app histogram, unit seconds:

```
histogram_quantile(0.95, sum by (le) (rate(http_request_duration_seconds_bucket[5m])))
```

The same per VM, as a stat panel with the slowest VM first:

```
sort_desc(histogram_quantile(0.95, sum by (le, observatory_vm_name) (rate(http_request_duration_seconds_bucket[5m]))))
```

## Limits

| Limit | Value |
|---|---|
| Dashboards per organization | 50 |
| Panels per dashboard | 20 |
| Name and panel title | 100 characters |
| Query | 4 KiB |
| Range | 31 days |
| Points per series | 11 000; the hub raises the step to fit |
| Query time | 30 seconds |
| Queries | 10 per second per person, bursts of 40 (enough for a full dashboard at once) |

Two people editing the same dashboard do not overwrite each other: if it changed since you opened the editor, saving
fails with "changed elsewhere" and you reload to see the other version. Creating, changing and deleting dashboards is
recorded in the audit log (`dashboard:create`, `dashboard:update`, `dashboard:delete`).

## API

| Method and path | Purpose |
|---|---|
| `GET /api/orgs/{org}/dashboards` | dashboards you can see, each with `canEdit` |
| `POST /api/orgs/{org}/dashboards` | create: `{name, scope, teamId?, panels}` |
| `GET`, `PUT`, `DELETE /api/orgs/{org}/dashboards/{id}` | read, update (send the `updatedAt` you read; a stale one gives 409), delete |
| `POST /api/orgs/{org}/dashboards/query` | `{query, from, to, step?}` for a range or `{query, time?}` for one instant |
| `GET /api/orgs/{org}/dashboards/metric-names?match=` | metric names you can see, optionally limited by a series selector |

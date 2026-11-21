# Logs explorer — design

## Intent

Today logs are only reachable per object: the Metrics tab of one node or one VM. Finding a line means clicking through
every page, and once a VM or image is deleted its logs are unreachable in the hub although they are still in
VictoriaLogs (build logs of deleted images are gone from Postgres entirely, `ON DELETE CASCADE`). The org needs one
place to browse and search all of its logs, including those of deleted objects, for as long as the store keeps them.

## Goals

- An org page **Logs** with three levels: log groups → streams → entries.
- Log groups come from the log store, so deleted VMs, nodes and images appear (marked "deleted") until retention drops
  their logs.
- A **Query** tab: LogsQL across chosen groups, results as rows or, for `stats` pipes, as a table.
- New org permission `logs:read`, held by the built-in admin and owner roles only.

## Non-goals

- Longer retention. It stays at `OBSERVATORY_LOGS_RETENTION` (14 days); keeping build logs of deleted images in
  Postgres is not part of this.
- Per-team or per-object scoping inside the explorer (all or nothing per org; can be added later).
- Saved queries, alerts, metric filters, export.
- Builds of private (user-scoped) images: they are never copied to the store and so never appear.
- Platform-wide view: platform admin status grants nothing here, as elsewhere.

## Anti-goals

- Any path by which a query sees another org's logs.
- Slowing the store for everyone: one heavy query must not starve ingest or the detail pages.

## Approaches

- **A — browse only** (groups → streams → entries with filters and live tail). Simple, but no searching across groups
  and no aggregation.
- **B — A plus a Query tab with LogsQL.** Chosen by the user. Reuses the forced `extra_filters` scoping and the LogsQL
  deny list of the Grafana logs proxy (the probe found no leaking constructs on VictoriaLogs v1.54.0, so the list is
  empty but stays enforced).
- **C — point users at Grafana.** Rejected: the user wants it in the hub, without extra tooling.

## Decided

- **Permission:** action `logs:read` on a new org object `org/logs`; built-in rules for admin and owner; grantable via
  custom roles; listed in `docs/permissions.md`. Detail-page logs stay under `vm:metrics` / `node:metrics` unchanged.
- **Scope:** every explorer request goes to VictoriaLogs with `extra_filters=observatory.org:="<org>"` set by the hub;
  client-supplied `extra_filters`/`extra_stream_filters` and tenant headers are impossible (the hub builds the request).
- **No audit:** reading logs is not audited, like the detail pages and dashboards (user decision).
- **Retention:** whatever the store holds (default 14 days); the UI says so.

### Log groups

| Kind | Group key | Name field | Streams |
|---|---|---|---|
| VM | `observatory.vm` | `observatory.vm_name` | `observatory.stream`: service, console, app, system, egress |
| Node | `observatory.node` | `observatory.node_name` | `source`: observatory-node, firecracker |
| Image | `observatory.image` | `observatory.image_name` | one per `observatory.build` |

Display names are `vms/<name>`, `nodes/<name>`, `builds/<image name>`; the id is the identity (a name reused after
deletion is a separate group). A group is "deleted" when its id no longer exists in Postgres. The list shows kind,
stream count and time of the last entry, filterable by kind and name.

### Pages

1. **Groups** (`/orgs/<org>/logs`): table as above.
2. **Group**: its streams with last entry time; for images each build stream shows build start and result when the
   build still exists in Postgres, else just its id and time.
3. **Stream / group entries**: the existing log list (collapsed message, expand to JSON, click a field to filter),
   free text, level, time range up to the retention, live tail. "All streams" of a group is selectable.
4. **Query tab**: pick groups (multi-select, empty = whole org), time range, LogsQL text. Rows render like the entry
   list; a result with a `stats` pipe renders as a table.

The VM and node Metrics tabs get a link "open in Logs" to their group.

## API (my calls, builder may refine names)

- `GET /api/orgs/{org}/logs/groups?kind=` → `[{kind, id, name, deleted, streams, lastAt}]`
- `GET /api/orgs/{org}/logs/groups/{kind}/{id}/streams`
- `GET /api/orgs/{org}/logs/entries?kind=&id=&stream=&q=&level=&from=&to=&field.<k>=` and `/entries/tail` (same
  filter building as the detail pages, `buildLogsQL`)
- `POST /api/orgs/{org}/logs/query` `{query, groups:[{kind,id}], from, to, limit}` → `{rows}` or `{columns, rows}`

All need `logs:read`; telemetry off → 404 like the other telemetry endpoints.

## Limits (my calls)

- Time range ≤ store retention; query result ≤ 1 000 rows; upstream timeout 30 s; request body ≤ 64 KiB.
- Per user: 10 queries/s, burst 40 (like dashboard queries); at most 3 concurrent queries and 5 live tails per user.
- Group listing (my call): names, last entry time and builds are plain fields, not stream fields, so the list is a
  bounded `stats by (<group key>) max(_time), count_uniq(<stream field>)` plus the name field over the retention,
  cached per org for 30 s with one in-flight computation per org. Names of live objects come from Postgres; deleted ones
  use the last name found in the logs. No ingest change.
- Store protection (my call): besides the per-user limits, a hub-wide cap of 4 concurrent explorer queries (others wait
  up to 5 s, then 429), separate from the detail pages and dashboards; VictoriaLogs gets `-search.maxQueryDuration=60s`
  in `deploy/compose.yaml`.
- The hub reads `OBSERVATORY_LOGS_RETENTION` (default `14d`, same variable as the store) to bound the time range.

## Left to the builder

- Exact VictoriaLogs calls for groups and streams within the limits above.
- How a `stats` result is detected (response shape vs. query text).
- UI details within the existing design system (tables, badges, `TelemetryLogs` reuse).

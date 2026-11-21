# Telemetry, part 5: user dashboards

## Intent

Part 5 of the telemetry roadmap. Users send their own metrics from their apps (part 2, stream `app`, local OTLP endpoint) and want to see them in the hub without running Grafana: "eigene Seiten des Users, wenn er seine Metrics anzeigen will". A dashboard is a page with a few charts built from the user's metrics (and the built-in VM/node usage metrics), visible inside the org according to the same permissions as the data.

Success: Alice, owner of `vm01` running an app that sends `orders_total{status}` and `http_request_duration_seconds` histograms, creates a dashboard "Shop", adds a panel "Orders/min" with query `sum by (status) (rate(orders_total[5m]))` and a panel "p95 latency" with `histogram_quantile(0.95, sum by (le) (rate(http_request_duration_seconds_bucket[5m])))`, picks the time range, shares it with her team. Bob (same team) sees it with data. Carol (member, not in the team, no `vm:metrics` on `vm01`) sees the dashboard only if shared org-wide, and then panels show no data from `vm01`. An org admin sees all dashboards and all data.

## Decided

*(Controller calls, user away — goal mode.)*

- **Queries are MetricsQL**, entered per panel (with a metric-name picker and autocomplete from the user's visible series as help). No visual query builder in this part.
- **Data scoping is enforced by the hub, per viewer, at query time** (refined after review): holders of org-wide `vm:metrics` and `node:metrics` (org admins) get only `extra_label=observatory_org=<org>` — no VM list, so history of deleted VMs stays visible. Everyone else gets `extra_filters[]={observatory_vm=~"<allowed ids>"}` (OR, per VictoriaMetrics, with `{observatory_vm=""}` only if they hold `node:metrics` org-wide); **if the allowed list is empty and they have no node access, the hub does not query at all and returns an empty result** (an empty regex `=~""` would match node series). Original text for reference: every panel query runs with `extra_label=observatory_org=<org>` and `extra_filters[]={observatory_vm=~"<ids of VMs the viewer may vm:metrics>"}` plus, if the viewer may `node:metrics` (org admins), an additional alternative for node series; viewers without any access get empty results. VictoriaMetrics applies `extra_filters[]` to every series selector in the query, so query text cannot widen it. Node series (no `observatory_vm`) are only included for viewers with `node:metrics` on the org's nodes (`extra_filters[]={observatory_vm=""}` as a second OR filter).
- **Dashboard ownership/sharing**: a dashboard belongs to a user (private), a team (visible to team members), or the org (visible to all members).
  - Private dashboards follow the existing private-scope rule (`ObjUserImages`/`ObjUserEnv` style): **no built-in role reaches them, not even org admins** — only their user.
  - Team dashboards: read for team members; edit for the creator (while still a team member) and team admins; org admins read/edit via their team-wide rights like other team resources.
  - Org dashboards: read for all members; create/edit for org admins and the creator.
  - Create: any member may create private and (for own teams) team dashboards; org-wide ones only org admins. "Sharing" = moving a dashboard to a team or the org scope, allowed if the actor may create in the target scope; `created_by` keeps edit rights for the creator as stated above. New permission actions `dashboard:read`/`dashboard:edit` modelled like other org resources (follow images/env scoping patterns in `internal/authz`).
- **Panel types**: time series (lines, uses the existing `TimeSeriesChart` incl. synced hover/zoom), stat (single latest value with unit). Unit per panel: none, %, bytes, bytes/s, seconds, req/s. Max 20 panels per dashboard, max 50 dashboards per org.
- **Layout**: a responsive grid, panel width 1/2 or full; order by drag or up/down buttons (simplest that works on mobile too).
- **Storage**: Postgres tables `dashboards(id, org_id, owner_kind {user,team,org}, owner_id, name, created_by, created_at, updated_at)` and panels as JSONB array on the dashboard (title, type, query, unit, width); updated_at for optimistic concurrency (409 on stale save).
- **API**: `GET/POST /api/orgs/{org}/dashboards`, `GET/PUT/DELETE /api/orgs/{org}/dashboards/{id}`, `POST /api/orgs/{org}/dashboards/query` (body: query, from, to, step or instant) — the hub computes the viewer's allowed VM ids and runs `query_range`/`query` with the enforced filters; `GET /api/orgs/{org}/dashboards/metric-names?match=` for the picker (series/label values API with the same filters). Query timeout 30 s, max 11 000 points per series, per-user rate limit 10 queries/s with a burst of 40 (a 20-panel dashboard loads at once).
- **UI**: org nav "Dashboards" list (scope badges), dashboard page with range picker (presets like the node page + custom), edit mode (add/edit/remove panels, query editor with "Run" preview), empty state with an example query using `system_cpu_time_seconds_total` so it works without custom metrics.
- Audit events for create/update/delete.

## Goals / non-goals / anti-goals

- Goals: as above; docs `docs/dashboards.md`.
- Non-goals: logs panels, alerting, templating variables, import/export of Grafana JSON, public/anonymous dashboards.
- Anti-goals: a viewer sees series from VMs they can't `vm:metrics` (by query text, regex, `label_replace`, subqueries or the metric-name picker); a dashboard query can starve the store (timeouts, point caps, rate limit); editing someone else's private dashboard.

## Approaches

- **MetricsQL panels with hub-enforced `extra_filters[]` per viewer (chosen):** flexible enough for users' own metrics, safe because the store applies the filter to every selector.
- Visual builder only (metric + label filters + aggregation): safer UX for novices but too limited for histograms/rates users already know from Grafana. Possible later as an assistant on top.
- Embedding Grafana: separate component, separate auth. Rejected (part 3 covers external Grafana).

## Left to the builder

Picker/autocomplete details, grid component, chart reuse, migration numbering, test layout. Tests must include a scoping test where a member's query `{__name__=~".+"}` and `label_replace`-tricks return no series from another team's VM.

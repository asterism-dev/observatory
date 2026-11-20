# Telemetry, part 3: Grafana access

## Intent

Part 3 of the telemetry roadmap (parts 1 and 2: `2026-11-20-telemetry-nodes-design.md`, `2026-11-20-telemetry-vms-design.md`). The user wants admins to be able to connect an external Grafana as a second view onto the hub's telemetry (decision "A": Grafana reads from the hub; forwarding to an external backend is part 6, later). Grafana uses its stock Prometheus datasource for metrics and the official VictoriaLogs datasource plugin for logs, pointed at the hub with a token; every query is confined to the token's org.

Success: an org admin creates a token "grafana-prod" in org settings → Telemetry, sees it once, adds two datasources in Grafana (Prometheus URL `https://hub/grafana/prometheus`, VictoriaLogs URL `https://hub/grafana/logs`, header `Authorization: Bearer <token>`), builds a panel `rate(system_cpu_time_seconds_total{observatory_vm="…"}[5m])` and a logs panel `observatory.stream:="service"`; another org's data never appears, even with a query that names the other org's labels. Revoking the token stops Grafana within seconds.

## Decided

- With the user (earlier): Grafana reads from the hub (A); hub stays the single store; access per org; platform/org admins may connect Grafana.
- *(Controller calls, user away — goal mode)*:
  - **Tokens are org-scoped**, created and revoked by holders of a new org permission `telemetry:token` (org admins and owners via the admin role; not members). There is **no platform-wide token**: platform admins deliberately get no tenant data (`requirePlatformAdmin` "grants no access to tenant data"); a platform admin who is also an org admin can create that org's token. Cost if wrong: a later "platform token" feature.
  - **A token never reaches further than its creator's org-wide access, and dies with it.** Creating a token requires `telemetry:token` *and* org-wide `vm:metrics` (on `org/vms/*`) *and* `node:metrics` (on `org/nodes/*`) — so a custom role granting only `telemetry:token` cannot mint an org-wide token. On use, the hub re-checks (cached ≤ 60 s) that the creator is still a member holding those three permissions; otherwise the token is treated as revoked (401) and marked revoked. Deny rules that exclude single VMs from the creator are not modelled for tokens: a creator with any deny on `vm:metrics`/`node:metrics` objects cannot create tokens (checked at creation and on use).
  - An org token sees **all** telemetry of its org (all nodes, all VMs incl. logs) — the same as an org admin's view (admins hold `node:metrics` and `vm:metrics` for everything). Documented as such.
  - Token format: `obs_grafana_` + 32 random bytes base32; stored as SHA-256 hash; shown once; name (unique per org), created_by, created_at, last_used_at (updated at most once per minute), revoked_at. Max 20 active tokens per org. Audit events for create/revoke.
  - Endpoints (bearer token only, no session cookies, CORS not enabled):
    - `/grafana/prometheus/api/v1/{query,query_range,series,labels,label/{name}/values,metadata,status/buildinfo}` (GET and POST form) → VictoriaMetrics with `extra_label=observatory_org=<org>` forced; any client-supplied `extra_label`, `extra_filters`, `extra_filters[]` parameters are removed first. Other VictoriaMetrics paths (write, import, export, delete, admin, snapshot, `/api/v1/status/tsdb`) → 404.
    - `/grafana/logs/select/logsql/{query,tail,hits,field_names,field_values,stream_field_names,stream_field_values,streams,stream_ids,stats_query,stats_query_range,facets}` → VictoriaLogs with `extra_filters=observatory.org:="<org>"` forced; client `extra_filters`/`extra_stream_filters` removed; insert/delete/admin paths 404. Tenant headers (`AccountID`, `ProjectID`) stripped.
    - `/grafana/prometheus/api/v1/metadata` answers a local empty success stub (`{"status":"success","data":{}}`): VictoriaMetrics keeps metadata per metric family, not per series, so it may not honour `extra_label`.
    - **User-written LogsQL is new territory** (parts 1–2 only sent hub-built queries): before relying on `extra_filters`, the builder probes VictoriaLogs v1.54.0 with subqueries and pipes (`in(...)`, `union`, `join`, `stream_context`, `facets`, `field_values`, `hits`, `stats_query_range`, `stream_ids`) across two orgs. If any leaks, the proxy rejects (400) queries containing the leaking constructs by a conservative token scan — not a full parser — and the docs list the restriction. The probe results are recorded in the test file as cases.
    - Response bodies passed through; upstream errors mapped to 502 without leaking upstream URLs.
  - Per-token rate limit: 20 requests/s, burst 100 (429 beyond); query timeout 60 s; `tail` max 15 min per connection.
  - Telemetry off → 404; revoked/unknown token → 401; token of a deleted org → 401.
  - UI: org settings gets a "Telemetry" page: list tokens (name, created, last used, revoke), create dialog showing the token once plus copy-ready datasource URLs and a short Grafana how-to.
  - CLI: not in this part.

## Goals / non-goals / anti-goals

- Goals: token model + store + migration; `telemetry:token` permission; the two proxies with enforced scoping and path allowlists; rate limiting; org settings UI; docs (`docs/grafana.md`) incl. datasource setup and the VictoriaLogs plugin name (`victoriametrics-logs-datasource`).
- Non-goals: platform-wide tokens; per-team/per-VM tokens; dashboards shipped for Grafana (a sample dashboard JSON in docs is optional); OTLP forwarding (part 6); Loki-compatible API.
- Anti-goals: a token reads another org's data by any query/param/header trick; a token can write, delete or administer the stores; tokens readable after creation; Grafana traffic degrading the hub for users.

## Approaches

- **Hub reverse proxy with enforced scope parameters (chosen):** small allowlisted proxy in the hub; relies on VictoriaMetrics `extra_label` and VictoriaLogs `extra_filters`, which apply to every selector/filter in the query and cannot be widened by query text. Already proven in parts 1–2.
- Query rewriting/parsing in the hub (parse PromQL/LogsQL and inject label matchers): more code, easy to get wrong. Rejected.
- Expose the stores directly with their own auth (vmauth/multi-tenant cluster): new component and tenant mapping; single-node stores don't have tenants for metrics. Rejected.

## Design notes for the builder

- Store: migration `000NN_telemetry_tokens.sql` (next free number), table `telemetry_tokens(id uuid pk, org_id fk, name, token_hash bytea unique, created_by, created_at, last_used_at, revoked_at)`, unique `(org_id, name) where revoked_at is null`.
- API: `GET/POST /api/orgs/{org}/telemetry-tokens`, `DELETE /api/orgs/{org}/telemetry-tokens/{id}`; permission `telemetry:token` on the org's settings object (follow how `integration:*`/`env:*` org actions are modelled in `internal/authz/catalog.go`).
- Proxy package `internal/api/grafana.go` (or `internal/telemetry/proxy.go` + thin API wiring); uses the existing `telemetry.Client` URLs; streaming for `tail` with flush and cleared write deadline (like the SSE handlers).
- Tests must include: param smuggling (`extra_label` in query string AND POST form, repeated, different case), path traversal (`/grafana/prometheus/api/v1/../admin`), `match[]` for another org's labels returns nothing, POST bodies > 1 MiB rejected, token revoked mid-tail → stream ends within the limiter interval or on next request.
- Docs: `docs/grafana.md`, link from `docs/telemetry.md`; `docs/permissions.md` for `telemetry:token`.

## Left to the builder

Exact rate limiter implementation; how `last_used_at` is batched; UI layout within the existing settings pages.

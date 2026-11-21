# Grafana

A Grafana you run can read the telemetry of an organization ([Telemetry](telemetry.md)) through two read-only
endpoints of the hub: `/grafana/prometheus` (metrics) and `/grafana/logs` (logs). Access uses an org token, not a
login.

## Create a token

In the web app, open *Organization settings*, *Telemetry*, and choose *Create token*. The token starts with
`obs_grafana_` and is shown once; the hub stores only a hash. An organization has at most 20 active tokens, with
unique names.

To create a token you need `telemetry.tokens.create` on the organization settings and org-wide `compute.instances.getMetrics` and
`compute.nodes.getMetrics`, with no deny rule on them ([Permissions](permissions.md)). Owners and admins have all three. Platform
admin status grants nothing here, and there are no platform-wide tokens.

## Prometheus datasource

In Grafana, add a *Prometheus* datasource:

- URL: `<hub>/grafana/prometheus`
- Custom HTTP header: `Authorization` with value `Bearer <token>`

`<hub>` is the public address of your hub. Use server-side access (Grafana's default *Server*
mode): the hub sends no CORS headers, so browser access cannot reach it. Queries, `query_range`, series, label and build info calls work. The
metadata endpoint answers with an empty result.

## VictoriaLogs datasource

Install the plugin `victoriametrics-logs-datasource` in Grafana and add a datasource:

- URL: `<hub>/grafana/logs`
- Custom HTTP header: `Authorization` with value `Bearer <token>`

As with Prometheus, the datasource must use server-side access.

## What a token sees

All telemetry of the organization, including its VM logs and the build logs of its org and team images ([Build logs](telemetry.md#build-logs)). Nothing of other organizations: the hub adds the
organization filter to every query and drops any filter the client sends to change it.

## Lifecycle

Revoke a token in *Telemetry* settings; it stops working immediately. When its creator leaves or is removed from the
organization, the hub revokes the creator's tokens at once. A token also dies when its creator loses
`telemetry.tokens.create`, `compute.instances.getMetrics` or `compute.nodes.getMetrics`, for example through a role change or a deny rule. That is checked
on every use against an access state that may be up to 60 seconds old, so such a change takes effect on the token's
next use after that; the hub then revokes the token and answers 401.

## Limits

- 20 requests per second per token, with bursts of 100 (else 429).
- At most 20 concurrent requests per token, of which at most 5 live tails (else 429).
- Queries run for at most 60 seconds.
- A live tail ends after 15 minutes at most. If no data has arrived yet when a tail is quiet too long or is revoked,
  it ends with a 502; once rows are streaming, the connection simply closes.
- Request bodies up to 1 MiB.
- Only the query endpoints are available; everything else answers 404, and LogsQL constructs that could reach other
  organizations answer 400.

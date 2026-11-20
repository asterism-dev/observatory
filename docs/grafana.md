# Grafana

A Grafana you run can read the telemetry of an organization ([Telemetry](telemetry.md)) through two read-only
endpoints of the hub: `/grafana/prometheus` (metrics) and `/grafana/logs` (logs). Access uses an org token, not a
login.

## Create a token

In the web app, open *Organization settings*, *Telemetry*, and choose *Create token*. The token starts with
`obs_grafana_` and is shown once; the hub stores only a hash. An organization has at most 20 active tokens, with
unique names.

To create a token you need `telemetry:token` on the organization settings and org-wide `vm:metrics` and
`node:metrics`, with no deny rule on them ([Permissions](permissions.md)). Owners and admins have all three. Platform
admin status grants nothing here, and there are no platform-wide tokens.

## Prometheus datasource

In Grafana, add a *Prometheus* datasource:

- URL: `<hub>/grafana/prometheus`
- Custom HTTP header: `Authorization` with value `Bearer <token>`

`<hub>` is the public address of your hub. Queries, `query_range`, series, label and build info calls work. The
metadata endpoint answers with an empty result.

## VictoriaLogs datasource

Install the plugin `victoriametrics-logs-datasource` in Grafana and add a datasource:

- URL: `<hub>/grafana/logs`
- Custom HTTP header: `Authorization` with value `Bearer <token>`

## What a token sees

All telemetry of the organization, including its VM logs. Nothing of other organizations: the hub adds the
organization filter to every query and drops any filter the client sends to change it.

## Lifecycle

Revoke a token in *Telemetry* settings; it stops working immediately. A token also dies when its creator leaves the
organization or loses `telemetry:token`, `vm:metrics` or `node:metrics`. The hub rechecks this at least every 60
seconds and then answers 401.

## Limits

- 20 requests per second per token, with bursts of 100 (else 429).
- Queries run for at most 60 seconds.
- A live tail ends after 15 minutes at most. A tail that is quiet for a long time, or whose token was revoked, ends
  with a 502.
- Request bodies up to 1 MiB.
- Only the query endpoints are available; everything else answers 404, and LogsQL constructs that could reach other
  organizations answer 400.

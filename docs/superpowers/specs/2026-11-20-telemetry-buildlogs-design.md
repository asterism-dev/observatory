# Telemetry, part 4: build logs in the log store

## Intent

Part 4 of the telemetry roadmap. The roadmap item was "builder logs move to VictoriaLogs; `image_build_log_chunks` goes away", so that build output shares the one log path (search across builds, Grafana via part 3, later per-run logs for functions/automations follow the same pattern).

Success: an org member with access to image `acme/api` opens a build's log as today (unchanged UI, live follow works); the same lines are searchable in the store as stream `build` with `observatory.image` / `observatory.build` fields, so an org admin's Grafana (part 3) can query "all failed `npm ci` lines across builds last week". With telemetry off, builds and their logs work exactly as before.

## Decided

*(Controller calls, user away — goal mode; reasons given because they deviate from the roadmap line.)*

- **Postgres stays the record for build logs; the log store gets a copy.** Today a finished build's log is compacted into `image_builds.log` and kept with the build history indefinitely, and telemetry is optional. Moving the record into VictoriaLogs (14-day default retention, global) would silently delete older build logs and break builds on hubs without telemetry. So `image_build_log_chunks` and `image_builds.log` stay; the roadmap's "chunks go away" is dropped. Cost if wrong: a later migration that removes the Postgres path once retention per stream exists.
- When telemetry is configured, every appended build log chunk is split into lines and written to VictoriaLogs as OTLP log records: stream `build`, resource attrs `observatory.org`, `observatory.image`, `observatory.image_name`, `observatory.build`, `service.name=observatory-builder`; body = the line (truncated at 16 KiB, ANSI escapes kept). Partial lines across chunks are carried until the next newline or the build's finish. Stream fields: `observatory.org,observatory.image,observatory.stream`.
- **Only builds of org- and team-scoped images are copied.** Builds of user-private images are not written to the store: their logs are readable only by that user today (`org/user-images/<user>` is out of reach for every built-in role), and an org's Grafana token (part 3) sees everything with its `observatory.org`. Adds attribute `observatory.image_scope` (`org` | `team`). Cost if wrong: private builds missing from Grafana.
- Written asynchronously from the hub (bounded in-memory queue, 10 000 lines, drop oldest; store errors logged, never fail the build or the append API).
- The existing build log API/UI is unchanged (reads from Postgres).
- Who can query them: only through stores the org already exposes — the org's Grafana tokens (part 3) see stream `build`; no new UI in this part. VM/node log views never show stream `build` (they filter by node/VM).

## Goals / non-goals / anti-goals

- Goals: hub-side line splitter + async writer into VictoriaLogs; stamping; tests; docs (`docs/telemetry.md`, `docs/images.md` one paragraph).
- Non-goals: moving the record off Postgres; a new build-log UI; builder VM usage metrics (builder VMs are refused at ingest, part 2 ruling); per-stream retention.
- Anti-goals: a build fails or slows because the log store is down; build logs of one org visible via another org's token; secrets that the build log redaction already removes appearing in the copy (the copy is taken after any existing redaction).

## Approaches

- **Hub copies on append (chosen):** single place (`buildAgent.log`), no builder/agent changes, works for old builder images.
- Builder agent sends OTLP itself through the node: needs node forwarding for builder VMs (refused by part 2) and agent changes. Rejected.
- Move the record (roadmap wording): loses history with retention, breaks telemetry-off hubs. Rejected (see Decided).

## Left to the builder

Queue/worker placement (a small `telemetry.LogWriter` reusable by later parts is welcome), batching (e.g. per 500 lines or 1 s), tests with a fake VictoriaLogs.

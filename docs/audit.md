# Audit

The audit trail is append-only and served by the audit service. Admins read it in the web app (org settings → Audit) or
with `observatory org audit`.

## What is recorded

- Every write and every authorization denial of a verified caller.
- Reads only where the org turned on data events for that service (see below).
- The hub records its actions through its outbox. Currently only the audit service records through the shared
  interceptor; other services follow as they are extracted.
- Calls without a verified identity are not recorded. A handler error (including not found) is `failed`; only
  authorization denials are `denied`, and they carry the same action name as the allowed call, so filter on `result`
  when counting what happened.

Event fields: `id`, `time`, `type` (the action, e.g. `compute.instances.create`), `org` (empty for platform scope),
`actor` (kind `user`, `service`, `node` or `system`, and id), `resource`, `result` (`allowed`, `denied`, `failed`),
`clientIp`, `userAgent`, `requestId` and `details` (action-specific).

## Data events

Org admins with `audit.settings.update` choose per org and service whether reads are recorded. The list only offers
services that record through the interceptor (today `audit`). A change is recorded as `audit.settings.update`, and
services pick it up without a restart.

## Retention

Platform admins set "Audit retention" from 30 to 3650 days (default 365). Older events are deleted by a daily job, so a
change applies at the next run. Nothing else deletes events.

## API

- `ListEvents` (org) and `ListAllEvents` (platform admins) at `/api/audit/v1/`, filterable by action, actor, result
  (`allowed`, `denied`, `failed`) and time, paged with `nextPageToken`.
- CLI: `observatory org audit [--limit N] [--action A] [--actor ID] [--since 24h]`, see [cli.md](cli.md).

## Guarantees

- Transactional outbox: an event is written in the transaction of the change, so a rolled-back change leaves no event
  and a committed change is eventually recorded, also when the event bus or the audit service is down meanwhile.
- The bus keeps events for 7 days. An audit service down for longer loses the events older than that; the bus being
  down only delays them, since they wait in the outbox.
- Text is stored without NUL characters, which Postgres cannot hold; only an event that cannot be decoded is dropped.
- Delivery is at least once; storage is idempotent by event id, so a redelivery is stored once.
- Delivery order is not causal order. Consumers must not rely on it; sort by `time`.
- Storage is append-only: a database trigger rejects `UPDATE`, `DELETE` and `TRUNCATE`, except in the retention job.
  This guards against bugs in application code, not against a database user who sets the retention flag too.
- Client IP: the edge sets it and a service records it only for a verified identity token. A leaked token could forge
  the IP for at most 5.5 minutes.

Deployment: [deploy.md](deploy.md#audit-service). Permissions: [permissions.md](permissions.md#audit).

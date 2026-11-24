# Telemetry as the first independent service

## Intent

observatory moves step by step on `main` towards independent services around one Postgres instance and NATS (see
[node control over a message bus](2026-11-24-node-control-bus-design.md) for the overall direction). Telemetry (logs
and metrics) goes first. It is off the VMs' critical path and stateless (the data sits in VictoriaMetrics/Logs), yet it
forces every foundational question of a distributed setup:

- what a service looks like (core, HTTP and NATS adapters, own database);
- how nodes and services authenticate to NATS;
- how a request from the GUI reaches a service that is not the hub, and how that service checks permissions;
- how a service gets data another service owns.

Answering these once on a low-risk service is the point; the node control project and storage build on the result.

Roadmap after this: node control over NATS (its spec stays valid; NATS auth for nodes and the service skeleton come
from here), then several storage servers, then the gateway and the rest.

## Goals

- A `telemetry` service, own binary and image, that can run as several instances and be updated independently of the
  hub.
- Nodes deliver logs and metrics over NATS, not through the hub; a hub restart is no telemetry gap.
- Telemetry has its own database in the shared Postgres instance and reads nothing from other services' databases.
- The service checks permissions itself, from a token the hub issues, without querying anyone for the user's rights.
- The GUI, CLI and Grafana keep working unchanged.

## Non-goals

- Node control over NATS (next project); the node keeps its WebSocket control connection here.
- An edge/front proxy separate from the hub. The hub forwards.
- Splitting IAM or compute out of the hub. The hub stays the owner of users, orgs, sessions, policy, nodes and VMs and
  answers for them on the bus.
- A NATS cluster, JetStream, or anything persisted in NATS.
- Changes to what is collected, stored or shown.

## Anti-goals

- A service reading another service's database "just this once".
- A per-request call to the hub to check a user's rights.
- A central IAM service project hiding inside this one: only the token and what it needs.

## Today

- **Ingest:** nodes send OTLP over HTTP to `{hub}/otlp/v1/{metrics,logs}` (`internal/node/telemetry.go:46`, VM data via
  the node's forwarder `internal/node/vmtelemetry/forward.go` with `X-Observatory-VM`). The hub authenticates the node,
  stamps org/node/VM, rejects builder VMs and writes to VictoriaMetrics/Logs (`internal/api/otlp.go`,
  `internal/telemetry/stamp.go`).
- **Query:** hub routes `nodes/{id}/usage|logs|logs/tail`, `vms/{id}/usage|logs|logs/tail`, `logs/groups/…`,
  `logs/query`, `dashboards` (+ `query`, `metric-names`), `telemetry-tokens`, and the Grafana proxies `/grafana/` and
  `/grafana/logs/` (token-authenticated, rate limited) in `internal/api/api.go`.
- **Build logs:** the hub writes them itself (`telemetry.LogWriter`, `cmd/hub/main.go:105`).
- **Tables:** `telemetry_tokens` (migration 00030), `dashboards` (00031) in the hub database, with foreign keys and
  `ON DELETE CASCADE` into `orgs`, `users`, `teams`; `RemoveMember` also deletes the user's private dashboards in its
  transaction (`internal/store/orgs.go:177`).
- **Grafana tokens:** each use also checks that the creator is still a member and still holds the token's rules
  (`CanHold`), re-checked on an interval per token; otherwise the token is revoked automatically with an audit entry
  (`internal/api/telemetry_tokens.go:140-176`).
- **Audit:** dashboard and token create/update/delete, token auto-revoke and every denied `authorize()`
  (`internal/api/orgs.go:62`) write to the hub's `audit_log`.
- **More hub data the queries read:** the logs explorer and dashboard scope list all VMs of an org, builder VMs,
  nodes, images and image builds (`BuilderVMs`, `ListImages`, `ImageBuilds`, `UserTeamIDs`, `Membership`;
  `internal/api/dashboard_query.go:46`, `liveLogGroupNames`, `listLogStreams`).
- **Telemetry inside staying routes:** `listVMs` / `getVM` embed the latest usage via `telemetry.VMLatest`
  (`internal/api/vms.go:195,257`); `CanTelemetry` on nodes and VMs comes from `h.telemetry != nil`.
- **Ingest identity:** besides stamping, ingest requires the VM to be on the sending node (`internal/api/otlp.go:83`).
- **Permissions:** `internal/authz` builds a Casbin enforcer in memory from `PolicySnapshot`
  (`internal/store/roles.go:19`), which reads memberships, teams, role bindings, custom role rules **and the owner of
  every VM and disk**, from which per-resource owner rules are generated. The hub reloads it in-process after each
  change (`internal/api/orgs.go:168`).

## Decisions

### Service shape

- `cmd/telemetry`, own image, own version; `deploy/compose.yaml` gains a `telemetry` service and a `nats` service.
- One core (ingest, queries, dashboards, telemetry tokens, Grafana proxy); HTTP and NATS adapters call it in-process.
  Permission checks live in the core.
- Own database `telemetry` in the shared Postgres instance, own migrations. `telemetry_tokens` and `dashboards` move
  there; on first start the data is copied once from the hub database and dropped there afterwards (the hub release
  that drops them requires the telemetry release before it).
- Stateless: any number of instances; NATS subscriptions use the queue group `telemetry`.

### NATS

- One NATS server, plain core NATS, no persistence. Nodes reach it directly on port 4222; TLS follows the hub's TLS
  setting.
- Authentication by auth callout answered by the hub: a node presents its existing node credential, a service its
  service token. Node permissions: publish/subscribe `*.v1.node.{its id}.>` plus `_INBOX.>`.
- Subjects are service first, version second, literal `node` before a node ID; org never before the node.

### Ingest over NATS

- The node publishes OTLP protobuf (gzip) as requests on `telemetry.v1.node.{node}.metrics|logs` and, for VM data,
  `telemetry.v1.node.{node}.vm.{vm}.metrics|logs`. VMs keep delivering locally to the node; no guest talks to NATS.
- Node and VM identity come from the subject (enforced by NATS permissions); `X-Observatory-VM` goes away.
- The telemetry service replies after writing to the store. **Without a reply the node keeps the batch in a bounded
  buffer and resends; a full buffer drops the oldest and counts the drop**, as the forwarder does today (user
  decision). No JetStream.
- Builder-VM rejection and org stamping use owner data (below).
- Build logs: the copier (`internal/api/buildlog_copy.go`) stays in the hub and keeps deciding what to copy and how to
  stamp it; only the write moves: the hub publishes on `telemetry.v1.buildlogs` (request with reply), the telemetry
  service writes. The hub keeps today's in-memory queue as its buffer *(design call)*.
- The hub's own use of telemetry data (latest usage in `listVMs` / `getVM`) goes through the telemetry service:
  `telemetry.v1.vm.latest` with the page's VM IDs as one request; `CanTelemetry` comes from "telemetry service
  configured". The GUI does not change *(design call)*.

### Data owned by another service

- User decision: one Postgres instance, a database per service; data owned by another service is asked for over NATS.
- The telemetry service asks the owner over NATS: `compute.v1.node.get`, `compute.v1.vm.get` (org, node, owner
  user/team, builder flag, name), and the list operations the queries need: `compute.v1.vm.list`, `compute.v1.node.list`
  (per org, with owners), `images.v1.image.list`, `images.v1.build.list`. Lists are paged so a reply stays below the NATS
  message limit. The hub answers them for now *(design call)*.
- Ingest checks that the VM's node (from `vm.get`) is the sending node, as today.
- Answers are cached; the hub publishes `compute.v1.node.changed` / `compute.v1.vm.changed` on change and the cache
  drops the entry; every entry also expires after 60 s in case an event is lost.
- Writes always go to the owner; telemetry never writes compute data.

### GUI requests and permissions

- The hub stays the entry point: it forwards the telemetry routes (listed under Today, plus `/grafana/…`) to the
  telemetry service as an HTTP reverse proxy. Streaming tails pass through unchanged. User decision: HTTP forwarding,
  not NATS request/reply (tails stream, answers can exceed NATS's message limit).
- For a request with a user session, the hub validates the session as today and attaches a **caller token** it signs
  (user decision):
  - lifetime 60 s; a removed permission can therefore still apply for up to 60 s (accepted by the user); logout is
    immediate because the hub validates the session before issuing;
  - contents: user ID, session ID, and everything about the user's rights: org memberships with role, team
    memberships (incl. team admin), and the resolved rules of their built-in and custom roles per org;
  - **not** in the token: who owns which resource. Ownership is a property of the resource and comes from its owner
    (the cached `compute.v1.vm.get` above). This keeps the token's size bound by orgs and rules, not by resources.
- The telemetry service verifies the signature and checks permissions in its core from token + resource owner. No
  query for the user's rights.
- Grafana requests carry a telemetry token instead of a session; telemetry owns those tokens and checks them itself.
  The hub forwards `/grafana/…` without a caller token.
- The hub's signing key: Ed25519, generated and kept by the hub; the telemetry service fetches the public key over
  `iam.v1.keys.get` at start and on `iam.v1.keys.changed` *(design call)*.

### Evaluating rules without ownership rules

Today's enforcer turns ownership into generated rules. The token-based check needs the same answers from rules in the
token plus the resource's owner *(design call)*:

- The token carries every rule reaching the user (via user, teams, built-in and custom roles), including deny rules on
  specific objects such as a single VM ID.
- Owner-generated rules (user-owned, team-owned, team admin) are rebuilt at check time from the resource's owner.
- Both `Allowed` and `CanHold` (with its escalation rule: org-wide grants count only without a deny under them) must
  give the same answer as today's enforcer for every telemetry route; tests compare both on the same fixtures.

## Delivery

All on `main`, each step releasable, in roughly this order (the builder may reorder within it):

1. NATS in compose and dev setup; auth callout in the hub; nothing uses it yet.
2. Hub answers `compute.v1.node.get` / `vm.get` and publishes `.changed` events.
3. Caller token: issuing in the hub, verification and rule evaluation as a library, tested against today's enforcer.
4. `cmd/telemetry` with its own database and the moved tables (data copy), queries over HTTP; the hub forwards the
   routes.
5. Ingest over NATS behind a node switch (`OBSERVATORY_NATS_URL`), HTTP ingest at the hub stays during the switch.
6. Build logs over NATS.
7. Cut: HTTP ingest and the telemetry code removed from the hub; nodes require NATS for telemetry.

## Failure behaviour

| Event | Effect |
|---|---|
| Hub restart | Ingest continues (NATS → telemetry). Queries fail while no hub forwards (the GUI is down then anyway). Caches keep answering until they expire; new VMs' data waits in the node buffer until the hub answers `vm.get`. |
| Telemetry instance dies | Others in the queue group take over; with none left, nodes buffer until one is back; queries fail. |
| NATS restart | Nodes buffer and resend after reconnect. |
| VictoriaMetrics/Logs down | No reply, nodes buffer, as today. |

## Success criteria (end to end on the pve01 test VMs)

- Node and VM metrics and logs, tails, logs explorer, dashboards and Grafana work as before, served by the telemetry
  service.
- Hub restart: no gap in node/VM metrics.
- Two telemetry instances: both take ingest and queries; stopping one loses nothing.
- Stopping all telemetry instances for a few minutes: data arrives afterwards; with a longer outage the node reports
  drops.
- Removing a user from an org: their telemetry access ends within 60 s.
- The telemetry service has no credentials for the hub database.

## Left to the builder

- Payload formats of `vm.get` / `node.get` and events; token encoding (JWT or other) and exact claims layout.
- Buffer size on the node; reply timeouts.
- How the hub reaches the telemetry service over HTTP (service name / config) and how several instances are balanced.
- Which of today's routes move: a route moves if its data comes from the telemetry stores or the telemetry database
  (e.g. check `vms/history`, which reads hub tables).
- Migration mechanics for the per-service database.

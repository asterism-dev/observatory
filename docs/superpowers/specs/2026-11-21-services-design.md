# Services with central IAM

## Intent

Project 2 of the 1.0 rework, after the mesh underlay (`2026-11-21-mesh-underlay-design.md`). The hub, one Go process
with one Postgres holding every table, becomes a set of services. Each service has one job, its own API, its own
actions in the permission catalog and its own data. One central IAM, one shared GUI, the way large clouds are built.

Why (user): deployments across several hosts get easier, and every service can be tested on its own because it only
does its job. The system must stay extensible: S3 buckets and functions come later.

Success:

- Every feature of 0.x works on 1.0, with the same permission semantics, through the same GUI.
- A homelab runs it with `docker compose up` on one machine; an operator can move single services to other hosts
  (over the underlay of project 1) by changing configuration only.
- Each service's tests run against that service alone, with the others replaced by their generated clients' fakes or
  not needed at all.

## Decided with the user

- **Central IAM**, not permission handling per service: services register their action catalog and check against
  central policy.
- **Several containers are fine; one Postgres instance.**
- **A quota service** joins the cut.
- **Subservices:** storage later gets S3, compute later gets functions. Categories group services in the GUI only.
- **Quota now enforces limits only; usage is recorded later** from lifecycle events every service sends from day 1.
- **Event bus** for internal reactions and, later, user subscriptions (webhooks, function triggers).
- **1.0 is a fresh install**, no migration from 0.x (mesh spec).
- All recommendations below were accepted: one binary/image (1A), NATS JetStream (2A), protobuf with ConnectRPC and REST
  transcoding (3A), compute orchestrates (4A), disks belong to compute (5A).

## Goals

- Every 0.x feature rebuilt as services: orgs, members, teams, roles, invites, SSH keys, audit, limits, nodes, VMs,
  snapshots, auto-suspend, delete-after, egress, disks, devices, networks, gateway, images, recipes, builds, auto
  rebuilds, registry, env vars, integrations (incl. Claude multi-account, gcloud), telemetry, dashboards, logs
  explorer, node updates, version checks, CLI.
- Permission semantics unchanged: subjects, object patterns, deny wins, built-in roles, escalation guard, connect check
  (`docs/permissions.md`).
- Running services on several hosts, enrolled over the underlay (see Several hosts).
- Event bus in place with outbox delivery, events modelled so user subscriptions can be added without reworking
  services.
- Extensibility: adding a service (buckets, functions) means a new package, proto, catalog and GUI section, with no
  change to existing services beyond what it consumes.

## Non-goals

- S3 buckets, functions, metering/billing, user event subscriptions (designed for, not built).
- Separate binaries, images or repos per service (possible later, see Rules).
- Migration from 0.x.
- HA or multiple instances of a service (must not be ruled out, see Rules).

## Anti-goals

- A service reading another service's tables, or importing another service's internals.
- `serve all` behaving differently from separate processes.
- A permission change that never takes effect, or a request admitted when the policy cannot be loaded.
- A lost event after a committed change, or an event for a rolled-back change.
- Homelab setup getting meaningfully heavier than today's compose.

## Approaches

1. **Packaging.** **A) One repo, one binary, one image, one process per service (picked):** `observatory serve <svc>`,
   compose starts the same image several times; `serve all` runs everything in one process for development and
   integration tests. B) One binary and image per service: clean, but many images to version and publish.
2. **Event bus.** **A) NATS JetStream (picked):** one small extra container, durable streams and consumers, subject
   permissions that fit later user subscriptions, clusterable. B) Postgres only (outbox, LISTEN/NOTIFY, own consumer
   offsets): no new component, but fan-out, retention and user subscriptions self-built. C) Kafka/Redpanda: too heavy
   for homelabs.
3. **API style.** **A) Protobuf contracts with ConnectRPC (picked):** one `.proto` per service; gRPC between services
   and for the CLI and Go clients, Connect protocol or gRPC-Web for the GUI with generated TypeScript, REST transcoding
   (`google.api.http` annotations, e.g. via `vanguard`) for curl and third parties; `buf breaking` in CI; aip.dev as
   style guide. This is the pattern Google uses (protobuf, gRPC clients, REST transcoding). B) OpenAPI-first with
   generated code: stays REST, weaker generators and breaking-change detection. C) Everything over NATS request/reply:
   no public API.
4. **Data a VM needs.** **A) Compute orchestrates (picked):** on create/update, compute calls artifacts, network and
   secrets with the user's token, so each checks its own permissions; at boot compute fetches current values with its
   service account and hands them to the node. B) Each service delivers its part to the node: desired state scattered,
   node talks to five services. C) Compute keeps event-fed local copies: resilient, more code; can be added later.
5. **Snapshots across VM and disks.** **A) Disks belong to compute (picked)**, as block volumes belong to the VM API at
   large clouds: attach, grow with restart, `releasing` with node confirmation, snapshots and fork stay local
   transactions. The storage server stays a separate data-plane process controlled by compute. B) Disks as their own
   service with sagas for snapshot, attach, grow, fork: clean split, the most failure-prone code in the system.

## Design

### Services

| Service | Owns (from today's tables and features) | GUI category |
|---|---|---|
| **iam** | users, orgs, memberships, sessions, invites, teams, roles, rules, bindings, SSH keys, platform admin, action catalog registry, OIDC login, CLI tokens, service accounts | Management |
| **audit** | audit trail of every service (see Audit), audit settings, retention | Management |
| **quota** | limits per user/team/org, current usage counters, reservations | Management |
| **compute** | nodes as VM hosts (capacity, hypervisor, control channel from the mesh spec), node updates, VMs, VM history, snapshots, auto-suspend, delete-after, egress CAs, disks, disk exports, storage server control | Compute |
| **network** | underlay hosts (enrollment, roles, addresses, peer sets, endpoints, quarantine; mesh spec), devices, networks, VM network attachments, OVN state, gateway state *(underlay ownership: our call; quarantine stays inside one service, and compute refers to a node by its host id)* | Networking |
| **artifacts** | images, versions, tags, layers, image defaults, registry credentials, token auth for the distribution registry; knows nothing about builds, externally pushed images land the same way | Artifacts |
| **build** | recipes (revisions, files), builds, build logs (written to VictoriaLogs), auto rebuilds (reacting to artifacts' "base image has a new version" event); runs builder VMs through compute and pushes results through artifacts' API. An image refers to the recipe it was built from but does not own it. | Artifacts |
| **secrets** | env vars, org keys, hub secrets (sealing), integrations, integration accounts | Management |
| **telemetry** | dashboards, telemetry tokens, metrics and logs queries (proxy to VictoriaMetrics/VictoriaLogs), logs explorer, OTLP intake | Management |
| **edge** | serves the Vue GUI, session check, JWT minting, routing `/api/<svc>/…`, version checks; the only externally reachable process | – |

Later: **buckets** (Storage), **functions** (Compute), **metering** (Management); git triggers and other build kinds
fit under build.

### Data

- One Postgres instance; one database and one DB role per service. No cross-database access, no foreign keys across
  services; references to another service's objects are ids, validated through its API when created.
- Configuration per service (own env vars and DB URL), so a service can move to its own Postgres by config.
- Each service owns its migrations.

### IAM and request flow

- The edge validates the session (cookie) or CLI bearer token with iam and attaches a short-lived signed JWT (user id,
  session). Services verify it offline against iam's JWKS.
- Services register their action catalog with iam at start. iam streams each org's effective policy (rules, roles,
  teams, bindings) to the services; each service evaluates `Allowed(sub, org, obj, act)` locally (today's in-memory
  enforcer). Changes take effect within seconds. A service that has no current policy for an org refuses its requests.
- Connect check, escalation guard and "404 for non-members" keep working as today.
- **Owner-dependent grants are computed by the owning service** *(our call)*, the way attribute conditions work at
  large clouds: iam streams principals' rules, roles, team memberships and bindings; the service adds the resource's
  attributes (owner user, owner team) from its own tables at check time. Today's `Policy.VMs`/`Policy.Disks` and the
  generated per-member/per-team grants move out of iam. A creator can use a new resource at once, because the owner
  attribute is local.
- **A service's catalog registration carries its built-in role defaults and owner-grant templates** *(our call)*, so a
  new service (buckets, functions) needs no change in iam.
- **iam asks the owning service when it needs resource knowledge** *(our call)*: the escalation guard for grants on a
  specific object, and the "owns VMs or disks → 409" checks on member removal, leave and team delete, call an internal
  check API on each registered service. These are calls on write paths only; the runtime dependency iam → service and
  service → iam (policy stream) is accepted.
- **iam issues every principal's credentials** *(our call)*: users, service accounts (gateway, storage server,
  distribution registry, each service), hosts (issued at enrollment, replacing the `node.json` bearer) and VM identities
  (requested by compute for guests, e.g. the integration token refresh). Credentials are JWTs verifiable offline, so
  telemetry and artifacts check node tokens without calling compute.
- Service-to-service calls carry either the user's JWT (on behalf of, so the callee checks its own permissions) or a
  service account's token issued by iam. Shared static tokens (`OBSERVATORY_GATEWAY_TOKEN`, `OBSERVATORY_STORAGE_TOKEN`,
  registry events secret) are replaced by service accounts.

### Quota

- quota holds the limits (today's `limits`: vCPU, memory, disk GB per user/team/org) and current usage.
- Services reserve before creating or growing (`reserve(owner, team, org, resource, amount)` → id), commit on success,
  release on delete or failure. Root disks (compute) and disks (compute) count together, as today.
- Reservations not committed within a timeout are released, so a crashed service does not leak quota.

### Event bus

- NATS JetStream, one container.
- Each service writes events to an outbox table in the same transaction as the change; a relay publishes them.
  Delivery is at least once; consumers are idempotent (event id).
- Events are protobuf messages with the same `buf breaking` rules as the APIs.
- Subjects carry org and type, e.g. `org.<id>.compute.vm.stopped`; every event names the action and object a reader
  would need to see it, so user subscriptions can later be permission-checked against the catalog.
- From day 1: audit events (consumed by audit), lifecycle events for every resource with owner, team, org, size and
  timestamp (consumed by quota for reconciliation, later metering), and cleanup reactions (e.g. VM deleted → network
  ports released). GUI live updates via edge are allowed but not required.

### Action catalog

- **Permission names are `service.resource.verb`** (user decision), e.g. `logging.logEntries.list`,
  `compute.instances.start`, `compute.disks.attach`, `iam.roles.update`, `audit.settings.update`: service as in the
  service table, resource in lowerCamelCase plural, verb in lowerCamelCase (`get`, `list`, `create`, `update`,
  `delete`, plus domain verbs such as `start`, `stop`, `connect`, `attach`). Built-in role defaults, custom rules,
  object patterns in grants and the audit trail all use these names; the 0.x `noun:verb` actions (`vm:power`, …) are
  replaced, no aliases (fresh install).
- Each service defines its actions next to its API: every RPC carries a proto annotation with its action, object
  pattern and kind (write or read). The same annotation drives the permission check, the audit interceptor and the
  catalog *(our call)*.
- At start a service registers its catalog with iam: actions, object patterns, descriptions, kind, built-in role
  defaults and owner-grant templates (user decision: actions live in the service and are published to iam).
- iam validates custom rules against the registered catalogs, and the GUI role editor lists actions from there, so a
  new service's actions appear without changes to iam or the GUI.
- An action that disappears from a catalog while custom rules still use it is kept and marked as unknown, not deleted
  *(our call)*.

### Audit

Like a cloud audit trail (user decision):

- A shared Connect interceptor in every service records automatically every write call and every denied call:
  principal, org, action, object, result, source IP, user agent, request id. Services may attach domain details (e.g.
  old and new role). Events go through the outbox and the bus to the audit service, which stores them append-only in
  its own Postgres database.
- **Data events** (read calls) are off by default and can be switched on per org and per service by org admins with a
    new permission `audit.settings.update` on `org/audit`. The platform admin sets retention and may cap what orgs can
  enable
  *(our call)*.
- `audit.events.list` per org and platform-wide audit for platform admins stay as today; the GUI filters by principal,
  action,
  object, time and result.
- Later, over the bus: streaming to a SIEM (user subscription), export into buckets.

### Settings other services need

General rule *(our call)*: a setting belongs to the service whose topic it is. Other services load it through its API
at start, keep it in memory, update it on a `<svc>.settings.changed` event, and reload everything after a lost NATS
connection. Audit settings are the first case; no general settings service.

### Interfaces

- One `.proto` package per service and version (`compute.v1`), resource-oriented per aip.dev.
- External paths through REST transcoding: `/api/<svc>/v1/…`, e.g. `GET /api/compute/v1/orgs/{org}/vms/{vm}`.
- GUI: one Vue app with generated TypeScript clients; navigation by category (Compute, Storage, Artifacts,
  Networking, Management). The CLI uses generated Go clients over gRPC.
- Node, gateway, storage server and distribution registry talk to their owning service (compute, network, compute,
  artifacts) over the underlay.

### Rules that keep services separable

- No imports of another service's internals; enforced by lint in CI.
- `serve all` takes no shortcuts: services talk through their Connect clients over loopback, never through direct
  function calls.
- `buf breaking` for APIs and events from the first release.
- Nothing may assume a single instance per service (no in-process locks as the only coordination for cross-request
  state); HA itself is out of scope.

### Deployment

- One image `observatory`; compose starts edge, iam, audit, quota, compute, network, artifacts, build, secrets, telemetry,
  NATS, Postgres, plus today's gateway, storage server, distribution registry and VictoriaMetrics/VictoriaLogs.
- Single host: services reach each other locally by name.

### Several hosts (deliverable, user decision)

- A service host enrolls like a node, as an underlay host with the role "service host" (mesh spec, hosts with roles),
  through network's enrollment with a single-use token.
- At enrollment the host receives, besides its underlay identity, the iam service-account credentials for the services
  it is configured to run.
- Names resolve across hosts over the underlay (`<svc>.internal`-style names served by the gateway DNS or an equivalent;
  builder's call), on a single host locally.
- NATS and Postgres are reachable over the underlay for services on other hosts.
- Between services: plain gRPC plus tokens; the underlay (WireGuard) is the encryption and network boundary, on a
  single host the docker network *(our call)*.
- Tests cover at least two hosts: edge/iam/postgres/NATS on the hub host, one or more services on a second host.

### Order of work *(our call)*

1. Foundation: proto/buf tooling, `serve` command, edge, iam (incl. catalog registry, policy streaming, service
   accounts), NATS + outbox library, audit (interceptor and service), quota.
2. compute (nodes, VMs, disks, snapshots) on top of the mesh control channel.
3. network.
4. secrets/integrations.
5. artifacts and build.
6. telemetry, dashboards, logs.

The GUI and CLI move with each service.

## Left to the builder

- Exact proto shapes and resource names (following aip.dev), and the JWT claims and lifetime.
- Policy streaming mechanism (gRPC stream, NATS subject, etc.).
- Outbox relay implementation and NATS stream layout and retention.
- Whether the REST transcoding uses `vanguard` or another library (verify fit first).
- How each 0.x feature's behaviour is split across service APIs, as long as it stays equivalent.

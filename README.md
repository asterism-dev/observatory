# observatory

observatory is a hub for managing organisations, users and (later) VMs. It
keeps users, orgs, membership, sessions and an append-only audit log in
Postgres, serves a Vue web app embedded in the Go binary, and ships a small CLI
(`observatory login`, `observatory whoami`).

It needs an external OIDC identity provider for login; see
[docs/idp.md](docs/idp.md) for requirements and client setup.

To run it on a server with Docker Compose, see [docs/deploy.md](docs/deploy.md); releases are described in
[docs/releasing.md](docs/releasing.md).

## Development

Needs Go, Node 22, Docker and `jq`.

```sh
make dev-up idp-up idp-bootstrap   # Postgres, Zitadel (deploy/idp), deploy/dev/hub.env
make run                           # edge on :8090, hub on :8091
npm --prefix web ci                # first time only
npm --prefix web run dev           # Vite on :5173, proxies /api and /auth
```

An existing `deploy/dev/hub.env` needs `OBSERVATORY_SECRETS_KEY=$(openssl rand -base64 32)` appended, and
`OBSERVATORY_GATEWAY_TOKEN=$(openssl rand -hex 32)` and `OBSERVATORY_STORAGE_TOKEN=$(openssl rand -hex 32)` (at least
32 characters each; the gateway and the storage server authenticate to the hub with them; `make gateway-token
storage-token` appends whichever is missing).

`make run` starts `observatory serve all`: the edge (public entry point, web app) on `OBSERVATORY_EDGE_LISTEN`
(default `:8090`) in front of the hub, which listens on `OBSERVATORY_HUB_LISTEN` (`:8091` with `make run`) for the
gateway, the storage server and the registry; the edge answers `/internal/*` with 404. `make edge-token` appends
`OBSERVATORY_EDGE_TOKEN` to `deploy/dev/hub.env` if missing (`make run` does it): the token the edge proves itself
with to the hub when the two run as separate processes (`observatory serve edge` and `observatory serve hub`); `serve all`
generates its own per start. `make service-token` likewise appends `OBSERVATORY_SERVICE_TOKEN` (edge) and the matching
`OBSERVATORY_SERVICE_TOKENS=edge=…` (hub) for separate processes.
`OBSERVATORY_GATEWAY_HOST` is the address nodes and clients reach the gateway on; it defaults to the host of `OBSERVATORY_PUBLIC_URL`.

Nodes need the WireGuard/OVN gateway: they reach the hub's control endpoint only through it, over the underlay.
Start it, and the storage server for disks, in Docker (they need
`OBSERVATORY_GATEWAY_TOKEN` and `OBSERVATORY_STORAGE_TOKEN`, added if missing, and reach the hub at `HUB_URL`, default
`http://host.docker.internal:8091`):

```sh
make gateway-up      # gateway-down stops both
```

Nodes and devices reach the gateway on UDP 51820/51821 at `OBSERVATORY_GATEWAY_HOST`, so use a LAN address rather
than `localhost` in `OBSERVATORY_PUBLIC_URL` then. [docs/network.md](docs/network.md) covers the address plan, the
gateway, devices, networks and the isolation model; [docs/disks.md](docs/disks.md) covers disks and the storage server.

For images, start the registry (`make registry-up`; `registry-down` stops it; the dev hub does not collect its garbage). It
appends `OBSERVATORY_REGISTRY_URL`, `OBSERVATORY_REGISTRY_KEY_DIR`, `OBSERVATORY_REGISTRY_CERT_DIR` and
`OBSERVATORY_REGISTRY_EVENTS_SECRET` to
`deploy/dev/hub.env` if missing, listens on `localhost:55000` and reports pushes to the hub at `HUB_URL`. It restarts
until the hub has written its token certificate to `deploy/dev/registry-certs`. The registry integration test runs
against it: `HUB_URL=http://host.docker.internal:55090 make registry-up`, then with `deploy/dev/hub.env` sourced
`OBSERVATORY_TEST_REGISTRY_URL=http://localhost:55000 go test ./internal/api -run TestRegistryPushRoundTrip`.

For telemetry, start the stores with `make telemetry-up` (`telemetry-down` stops them). It appends
`OBSERVATORY_METRICS_URL` and `OBSERVATORY_LOGS_URL` to `deploy/dev/hub.env` if missing and listens on
`localhost:58428` and `localhost:59428`.

Open <http://localhost:5173>. The first user to log in becomes platform admin.
With only the built binary (`make observatory`, then `bin/observatory serve all`), set `OBSERVATORY_PUBLIC_URL=http://localhost:8090`.

## Nodes

A node is a Linux amd64 host that runs the VMs. Create one in the GUI (or `POST /api/orgs/{org}/nodes`) and run the
returned install command on the host. The hub serves the installer and the node binary itself: `make node` builds
`bin/observatory-node-linux-amd64`, or point `OBSERVATORY_NODE_BINARY` at another file. Without the binary, `/node/observatory-node`
and `/node/SHA256SUMS` return 404. [docs/nodes.md](docs/nodes.md) covers requirements, what the installer changes on
the host and re-enrolling.

## CLI

```sh
make cli
bin/observatory login [--hub http://localhost:8090]
bin/observatory whoami
```

## Tests

```sh
make dev-up
make test
```

Tests need the dev Postgres (`OBSERVATORY_TEST_DATABASE_URL`, set by `make test`).

## Permissions

[docs/permissions.md](docs/permissions.md) describes roles, rules, teams, limits and platform admins.

## Design

[docs/superpowers/specs/2026-11-17-iam-foundation-design.md](docs/superpowers/specs/2026-11-17-iam-foundation-design.md)

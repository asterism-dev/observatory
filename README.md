# observatory

observatory is a hub for managing organisations, users and (later) VMs. It
keeps users, orgs, membership, sessions and an append-only audit log in
Postgres, serves a Vue web app embedded in the Go binary, and ships a small CLI
(`observatory login`, `observatory whoami`).

It needs an external OIDC identity provider for login; see
[docs/idp.md](docs/idp.md) for requirements and client setup.

## Development

Needs Go, Node 22, Docker and `jq`.

```sh
make dev-up idp-up idp-bootstrap   # Postgres, dev Zitadel, deploy/dev/hub.env
make run                           # hub on :8090
npm --prefix web ci                # first time only
npm --prefix web run dev           # Vite on :5173, proxies /api and /auth
```

An existing `deploy/dev/hub.env` needs `OBSERVATORY_SECRETS_KEY=$(openssl rand -base64 32)` appended, and
`OBSERVATORY_GATEWAY_TOKEN=$(openssl rand -hex 32)` (at least 32 characters; the gateway authenticates to the hub with it).
`OBSERVATORY_GATEWAY_HOST` is the address nodes and clients reach the gateway on; it defaults to the host of `OBSERVATORY_PUBLIC_URL`.

Open <http://localhost:5173>. The first user to log in becomes platform admin.
With only the built hub (`make hub`), set `OBSERVATORY_PUBLIC_URL=http://localhost:8090`.

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

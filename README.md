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

Open <http://localhost:5173>. The first user to log in becomes platform admin.
With only the built hub (`make hub`), set `OBSERVATORY_PUBLIC_URL=http://localhost:8090`.

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

## Design

[docs/superpowers/specs/2026-11-17-iam-foundation-design.md](docs/superpowers/specs/2026-11-17-iam-foundation-design.md)

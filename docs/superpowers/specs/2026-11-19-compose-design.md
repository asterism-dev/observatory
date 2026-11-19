# Hub as a Docker Compose stack

## Intent

The hub should be runnable, later in production, with Docker Compose alone: optionally start a login service, edit one
`.env`, start the hub stack. Today the hub runs as a host binary next to dev-only compose files with insecure defaults.

## Goals

- `deploy/compose.yaml`: hub, Postgres, gateway and storage, configured only through `deploy/.env`.
- Three ways to serve it: plain HTTP on 8090 (no TLS; local, tailnet or behind your own proxy), or with the `tls`
  profile Caddy on 80/443 with either an ACME certificate or a self-signed one from Caddy's internal CA.
- The login service as a separate, optional stack the operator can replace with any OIDC provider.
- A hub image built in the repo: web GUI, hub binary and the node binary the installer serves.

## Non-goals

- Production TLS for the bundled login service (operators with real deployments use their own OIDC provider or
  Zitadel's own TLS setup; our call, the user wanted the login service "outside").
- Changing the dev workflow: `deploy/dev/compose.yaml`, `make dev-up`, `make gateway-up`, `make run` stay.
- Image publishing to a registry, upgrades/migrations tooling, backups.

## Approaches

TLS (user picked C):

- A: Caddy always in the stack (ACME); B: HTTP only, TLS left to the operator's proxy; **C: Caddy as an optional
  `tls` profile**, plain HTTP without it. The user added a self-signed option, so the profile has two modes.

Login service: kept as its own compose project rather than a profile of the hub stack, so `docker compose up` of the
hub never starts or touches it and any OIDC provider fits (user: "docker compose up login-dienst, dann .env, dann hub").

## Design

Operator flow:

```
docker compose -f deploy/idp/compose.yaml --env-file deploy/idp/.env up -d   # optional login service
sh deploy/idp/bootstrap.sh                                                   # prints issuer and client IDs
cp deploy/.env.example deploy/.env && $EDITOR deploy/.env
docker compose -f deploy/compose.yaml up -d                                  # HTTP on :8090
docker compose -f deploy/compose.yaml --profile tls up -d                    # or Caddy on 80/443
```

Hub stack (`deploy/compose.yaml`, project name `observatory-hub`, so it never collides with the dev project
`observatory`):

| Service | Notes |
| --- | --- |
| `hub` | built from `deploy/hub.Dockerfile`; reads the `OBSERVATORY_*` variables; publishes `${OBSERVATORY_HTTP_PORT:-8090}` |
| `postgres` | `postgres:17.10-alpine`, password from `.env`, data volume, not published |
| `modules`, `gateway`, `storage` | as in the dev compose, but `HUB_URL=http://hub:8090`; UDP 51820/51821 published |
| `caddy` | profile `tls`; 80/443; `OBSERVATORY_TLS=acme` (default) or `internal`; proxies to `hub:8090`; data volume holds certificates and, for `internal`, the root CA |

- The hub's `OBSERVATORY_DATABASE_URL` is composed in `compose.yaml` from `POSTGRES_PASSWORD`; the operator sets the
  password, not the URL.
- `deploy/.env.example` lists every variable with a one-line explanation: public URL, gateway host, OIDC issuer and
  client IDs, secrets key, gateway and storage tokens (each with its `openssl rand` command), Postgres password,
  domain and TLS mode, optional docs URL and HTTP port. No working defaults for secrets: the hub refuses to start
  without them, as today.
- `OBSERVATORY_PUBLIC_URL` must match how users reach the hub (`https://<domain>` with the `tls` profile).
- `internal` TLS: the docs show how to copy Caddy's root CA from its volume and trust it on node hosts
  (`/usr/local/share/ca-certificates/`, `update-ca-certificates`) before running the installer, and in browsers;
  devices (WireGuard) need nothing.

Hub image (`deploy/hub.Dockerfile`): stage 1 Node builds `web/dist`; stage 2 Go builds the hub (with the embedded GUI)
and the linux/amd64 node binary; runtime image without toolchains, non-root, with the node binary at a fixed path set
via `OBSERVATORY_NODE_BINARY`.

Login service: `deploy/dev/zitadel/` moves to `deploy/idp/` (the Makefile `idp-*` targets and `docs/idp.md` follow).
Its insecure defaults move to `deploy/idp/.env.example`; `.env` is gitignored and `make idp-up` creates it from the
example when missing, so the dev flow keeps working. `bootstrap.sh` keeps writing `deploy/dev/hub.env` for dev and,
with `--print`, prints the issuer and client IDs for the operator to paste instead (our call).

## Verification

On a free test VM (9001 or 9002, Debian, Docker installed there): start the login service, bootstrap, fill `.env`,
start the hub stack without TLS and with `--profile tls` in `internal` mode; check the hub answers, `/auth/login`
redirects to the issuer, the gateway connects to the hub, and a node can download the installer. ACME needs a public
domain and is checked only for config validity (`caddy validate`).

## Left to the builder

Runtime base image choice, Caddyfile layout, exact `.env.example` wording, health checks and `depends_on` conditions.

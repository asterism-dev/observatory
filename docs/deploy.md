# Deploying the hub with Docker Compose

`deploy/compose.yaml` runs the hub with its Postgres, the WireGuard/OVN gateway, the storage server and the image
registry, configured
only through `deploy/.env`. A login service is not part of it: use any OIDC provider (see [idp.md](idp.md)) or the
Zitadel stack in `deploy/idp/`.

## Requirements

- A Linux amd64 host with Docker and the Compose plugin; the kernel must provide the `openvswitch` and `wireguard`
  modules (the stack loads them).
- Open ports: TCP 8090 (plain HTTP) or 80 and 443 (`tls` profile), and UDP 51820 and 51821 (nodes and devices);
  with the bundled login service also TCP 8080.

## Steps

```sh
# 1. Optional: the bundled login service. Edit deploy/idp/.env first (see below).
cp deploy/idp/.env.example deploy/idp/.env
docker compose -f deploy/idp/compose.yaml --env-file deploy/idp/.env up -d --wait
ZITADEL_URL=http://<idp host>:8080 sh deploy/idp/bootstrap.sh --print <hub public URL>

# 2. Configure the hub: paste the three printed OIDC lines, generate the secrets as the comments say.
cp deploy/.env.example deploy/.env    # then edit it

# 3. Start it, either plain HTTP on OBSERVATORY_HTTP_PORT …
docker compose -f deploy/compose.yaml up -d
# … or with TLS through Caddy on 80/443.
docker compose -f deploy/compose.yaml --profile tls up -d
```

`up` pulls the published images `ghcr.io/asterism-dev/observatory-{hub,gateway,storage}` at `OBSERVATORY_VERSION`
(`latest` by default, or a release tag such as `v0.1.0`); `up --build` builds them from the checkout instead.

The bundled Zitadel serves plain HTTP; its `.env.example` holds insecure defaults that must change before the host is
reachable from a network: `ZITADEL_DOMAIN` (the host name browsers use), `ZITADEL_MASTERKEY` (exactly 32 characters,
`openssl rand -hex 16`), `LOGIN_SESSION_COOKIE_SECRET` (`openssl rand -base64 32`) and `POSTGRES_ADMIN_PASSWORD`
together with the same password inside `ZITADEL_DATABASE_POSTGRES_DSN`. `ZITADEL_URL` for `bootstrap.sh` is
`http://<ZITADEL_DOMAIN>:8080` and becomes the hub's issuer. `bootstrap.sh` needs `curl` and `jq`, reads the admin
token Zitadel writes to `deploy/idp/.bootstrap/`, and creates a new project on every run.

After `up`, check that every service runs: `docker compose -f deploy/compose.yaml ps` and, if the hub restarts,
`docker compose -f deploy/compose.yaml logs hub` (a short token or a malformed key stops it there).

The first user to log in to the hub becomes platform admin: log in right after the first start, and turn off
self-registration in the login service (or keep it unreachable) until then.

## Serving modes

| Command | `deploy/.env` | Result |
| --- | --- | --- |
| `up` | `OBSERVATORY_PUBLIC_URL=http://<host>:8090` | plain HTTP, for a trusted network, a tailnet or your own reverse proxy |
| `--profile tls up` | `OBSERVATORY_TLS=acme`, `OBSERVATORY_DOMAIN`, `OBSERVATORY_PUBLIC_URL=https://<domain>` | public certificate; the domain must resolve to the host and 80/443 must be reachable |
| `--profile tls up` | `OBSERVATORY_TLS=internal`, same domain and URL | certificate from Caddy's own CA, no public DNS needed |

With the `tls` profile set `OBSERVATORY_HTTP_PORT=127.0.0.1:8090`: Docker's published ports bypass host firewalls
such as ufw, so port 8090 would otherwise stay reachable without TLS. Behind your own proxy, forward to port 8090 and
keep websockets (`/api/nodes/connect`) working.

## Trusting the internal CA

With `OBSERVATORY_TLS=internal` everything that talks HTTPS to the hub must trust Caddy's root certificate; WireGuard
devices need nothing.

```sh
docker compose -f deploy/compose.yaml cp caddy:/data/caddy/pki/authorities/local/root.crt observatory-hub-ca.crt
```

Run it as a user in the `docker` group: with `sudo` the copy is owned by root with mode 0600 and `curl --cacert`
cannot read it.

- Node hosts, before running the installer: copy the file to `/usr/local/share/ca-certificates/observatory-hub-ca.crt`
  and run `sudo update-ca-certificates`; `curl` and the node then trust the hub.
- Linux machines running the CLI: the same.
- macOS: `sudo security add-trusted-cert -d -r trustRoot -k /Library/Keychains/System.keychain observatory-hub-ca.crt`.
- Browsers use the system store on macOS and Windows; Firefox needs the certificate imported in its own settings.

## Configuration

`deploy/.env.example` lists every variable with a comment. Points that are easy to miss:

- `OBSERVATORY_PUBLIC_URL` must be exactly what users open; the login redirect is registered for it.
- `OBSERVATORY_GATEWAY_HOST` is what nodes and devices dial on UDP 51820/51821; it defaults to the host of the public
  URL.
- `OBSERVATORY_SECRETS_KEY` encrypts every stored secret. Back it up; a new key makes them unreadable.
- `OBSERVATORY_OIDC_ISSUER` must be reachable from the hub container, so not `localhost`.
- `OBSERVATORY_REGISTRY_EVENTS_SECRET` authenticates the registry's push notifications to the hub. They arrive at
  `/internal/registry/events`, which the `tls` profile's Caddy answers with 404 from outside; behind your own proxy,
  block `/internal/*` as well.
- `OBSERVATORY_REGISTRY_URL` (the compose file sets `http://registry:5000`), `OBSERVATORY_REGISTRY_KEY_DIR` and
  `OBSERVATORY_REGISTRY_CERT_DIR` turn the registry on; without a URL the hub has no images. The events secret must be
  at least 32 characters.
- `OBSERVATORY_BUILDER_IMAGE` overrides the builder image repository and `OBSERVATORY_BUILDER_HUB_URL` the hub URL
  builder VMs use (default: the public URL); set the latter when VMs cannot reach the public URL.

## Image registry

The `registry` service stores image versions and publishes no port: Docker clients and nodes reach it through the
hub's `/v2/` proxy, and the hub issues the registry tokens. The registry reports every push to the hub, which turns it
into an image version or deletes it (not linux/amd64, over the org's storage limit) and shows the reason on the image.
The hub's token key lives in the volume `registry-keys`, its certificate (all the registry mounts) in `registry-certs`,
the images in `registry-data`. Only tagged pushes count: a push by digest alone (`…/i/<id>@sha256:…`) becomes no
version, and its layers do not count towards the storage limit.

The registry notifies the hub in memory only: a push whose notification the hub has not yet received when the registry
restarts is lost (the layers stay until garbage collection, push again). Docker clients need the hub over https;
nodes and builder VMs may use the plain internal URL. See [images.md](images.md) for using it.

Deleting versions or images removes their manifests; the disk space comes back with garbage collection, which the hub
does not run. Garbage collection must not run next to a push, so stop the registry for it, daily from the host's crontab
at a quiet hour (pushes and pulls fail for the minute it takes):

```sh
0 4 * * * cd /path/to/observatory && docker compose -f deploy/compose.yaml stop registry && docker compose -f deploy/compose.yaml --profile maintenance run --rm registry-gc; docker compose -f deploy/compose.yaml start registry
```

## Updating

```sh
git fetch --tags && git checkout v0.2.0          # compose file and docs of that release
# set OBSERVATORY_VERSION=v0.2.0 in deploy/.env (with latest: docker compose -f deploy/compose.yaml pull)
docker compose -f deploy/compose.yaml up -d
```

The hub checks GitHub for a newer release every 6 hours and shows platform admins a banner when one exists; nodes on
an older version than the hub are marked on the node pages. `OBSERVATORY_NO_UPDATE_CHECK=1` in `deploy/.env` turns the
check off (air-gapped hosts). Builds that are not a release tag never check.

The hub migrates its database on start. Data lives in the volumes `postgres-data`, `disks`, `ovn`, `ovs`,
`registry-data`, `registry-keys`, `registry-certs` and, with TLS, `caddy-data`.

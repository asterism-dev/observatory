# Deploying the hub with Docker Compose

`deploy/compose.yaml` runs the hub with its Postgres, the WireGuard/OVN gateway, the storage server and the image
registry, configured
only through `deploy/.env`. A login service is not part of it: use any OIDC provider (see [idp.md](idp.md)) or the
Zitadel stack in `deploy/idp/`.

## Requirements

- A Linux amd64 host with Docker and the Compose plugin; the kernel must provide the `openvswitch`, `wireguard` and
  `xt_connlimit` modules (the stack loads them).
- Open ports: TCP 8090 (plain HTTP) or 80 and 443 (`tls` profile), and UDP 51820 and 51821 (nodes and devices; nodes may optionally accept UDP 51822 from each other for direct paths);
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

# 4. Required: start the stack in order whenever Docker starts (see below).
sudo cp deploy/observatory-hub.service /etc/systemd/system/
sudo systemctl daemon-reload && sudo systemctl enable observatory-hub.service
```

`up` pulls the published images `ghcr.io/asterism-dev/observatory-{hub,gateway,storage}` at `OBSERVATORY_VERSION`
(`latest` by default, or a release tag such as `v0.1.0`); `up --build` builds them from the checkout instead.

The bundled Zitadel serves plain HTTP; its `.env.example` holds insecure defaults that must change before the host is
reachable from a network: `ZITADEL_DOMAIN` (the host name browsers use), `ZITADEL_MASTERKEY` (exactly 32 characters,
`openssl rand -hex 16`), `LOGIN_SESSION_COOKIE_SECRET` (`openssl rand -base64 32`) and `POSTGRES_ADMIN_PASSWORD`
together with the same password inside `ZITADEL_DATABASE_POSTGRES_DSN`. `ZITADEL_URL` for `bootstrap.sh` is
`http://<ZITADEL_DOMAIN>:8080` and becomes the hub's issuer. `bootstrap.sh` needs `curl` and `jq`, reads the admin
token Zitadel writes to `deploy/idp/.bootstrap/`, and creates a new project on every run.

Step 4 is not optional. Gateway and storage join the network namespace of the `netns` service. When Docker itself
restarts (a reboot, a Docker upgrade) it starts containers in no particular order, and one that comes up before
`netns` fails with `cannot join network namespace of a non running container` and is not retried; on test hosts this
happened on every Docker restart. Without the unit, devices, nodes and disks stay cut off until someone runs
`docker compose -f deploy/compose.yaml start`. `journalctl -u observatory-hub` shows each run.

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
such as ufw, so port 8090 would otherwise stay reachable without TLS. Behind your own proxy, forward to port 8090. Nodes
open their control connection through the gateway, not through the proxy (it is refused there); they use the public
URL only for enrollment, their underlay config and the emergency channel.

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
- `OBSERVATORY_GATEWAY_NODE` (optional) is the id of a node installed on the hub host itself. The gateway then uses
  that node's OVS instead of its own. It needs the override `deploy/compose.shared.yaml`
  (`-f deploy/compose.yaml -f deploy/compose.shared.yaml`); see [network.md](network.md#hub-and-node-on-one-host).
- `OBSERVATORY_SECRETS_KEY` encrypts every stored secret. Back it up; a new key makes them unreadable.
- `OBSERVATORY_OIDC_ISSUER` must be reachable from the hub container, so not `localhost`.
- `OBSERVATORY_REGISTRY_EVENTS_SECRET` authenticates the registry's push notifications to the hub. They arrive at
  `/internal/registry/events`, which the `tls` profile's Caddy answers with 404 from outside; behind your own proxy,
  block `/internal/*` as well.
- `OBSERVATORY_REGISTRY_URL` (the compose file sets `http://registry:5000`), `OBSERVATORY_REGISTRY_KEY_DIR` and
  `OBSERVATORY_REGISTRY_CERT_DIR` turn the registry on; without a URL the hub has no images. The events secret must be
  at least 32 characters.
- `OBSERVATORY_METRICS_URL` and `OBSERVATORY_LOGS_URL` (the compose file sets both) point at the telemetry stores;
  both unset turns telemetry off, setting only one is an error. `OBSERVATORY_METRICS_RETENTION` (default `30d`),
  `OBSERVATORY_LOGS_RETENTION` (`14d`), `OBSERVATORY_METRICS_MAX_HOURLY_SERIES` (`500000`) and
  `OBSERVATORY_METRICS_MAX_DAILY_SERIES` (`2000000`) are optional; series beyond the limits are dropped. The hub also
  reads `OBSERVATORY_LOGS_RETENTION` to bound the time range of the [Logs](logs.md) page, so set it the same for both. It
  accepts only `<n>h`, `<n>d` or `<n>w`; other syntaxes the store knows, such as `1y`, stop the hub at startup. The
  compose file runs VictoriaLogs with `-search.maxQueryDuration=60s`; keep it if you run your own store.
- `OBSERVATORY_BUILDER_IMAGE` overrides the builder image repository and `OBSERVATORY_BUILDER_HUB_URL` the hub URL
  builder VMs use (default: the public URL); set the latter when VMs cannot reach the public URL.
- `OBSERVATORY_BUILDER_CPUS`, `OBSERVATORY_BUILDER_MEMORY_MB` and `OBSERVATORY_BUILDER_DISK_GB` size builder VMs
  (default: 2, 4096, 40; at least 1, 1024, 10). A build only starts on a node with that much memory free, and the
  builder VM counts against the VM limits of the image's owner. Large builds may run out of memory below 4096.
- `OBSERVATORY_NODE_URL` is the hub URL nodes pull user images (through the hub's `/v2/` proxy) from (default: the public URL). Set it when browsers and
  nodes reach the hub at different addresses, e.g. `http://localhost:8090` for you and `http://100.64.0.1:8090` over an
  overlay network for nodes. Use `https` for public hubs; plain `http` (for nodes and `OBSERVATORY_BUILDER_HUB_URL`)
  only on trusted private networks such as an overlay or VPN.

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

Deleting versions or images removes their manifests; the hub gets the disk space back with the registry's garbage
collection, daily at `OBSERVATORY_REGISTRY_GC_TIME` (default `04:00`, `off` for manual runs only) in the hub's time
zone (`TZ`, default UTC), and when a platform admin starts it under Platform settings → Registry. For it the hub mounts
`registry-data` and the registry's config (`OBSERVATORY_REGISTRY_CONFIG`), and the registry runs as the hub's user; the
`registry-data-owner` service hands data the registry wrote as root over once. Collection must not run next to a push:
new builds wait while running builds finish, then the hub stops issuing push tokens, waits out the ones already issued
(7 minutes) and collects. Until it ends, pushes fail (Docker shows `failed to fetch oauth token` with status 503);
pulls and VMs are not affected. Versions without tags are kept.

## Telemetry stores

The `victoriametrics` and `victorialogs` services hold node metrics and logs in the volumes `metrics-data` and
`logs-data`. They publish no port; only the hub reaches them. Upgrading an existing deployment: `docker compose up -d`
starts the stores, and telemetry is on from then. VictoriaMetrics runs with `-opentelemetry.promoteScopeMetadata=false`:
scope names, versions and attributes from VM apps would otherwise become labels, and so series that the per-VM series
limit does not count ([telemetry.md](telemetry.md#vms)). Keep the flag if you run your own VictoriaMetrics.

## Updating

```sh
git fetch --tags && git checkout v0.2.0          # compose file and docs of that release
# set OBSERVATORY_VERSION=v0.2.0 in deploy/.env (with latest: docker compose -f deploy/compose.yaml pull)
docker compose -f deploy/compose.yaml up -d
systemctl is-enabled observatory-hub.service     # not "enabled": install the unit as in step 4
```

The hub checks GitHub for a newer release every 6 hours and shows platform admins a banner when one exists; nodes on
an older version than the hub are marked on the node pages. `OBSERVATORY_NO_UPDATE_CHECK=1` in `deploy/.env` turns the
check off (air-gapped hosts). Builds that are not a release tag never check.

The hub migrates its database on start. Data lives in the volumes `postgres-data`, `disks`, `ovn`, `ovs`,
`registry-data`, `registry-keys`, `registry-certs`, `metrics-data`, `logs-data` and, with TLS, `caddy-data`.

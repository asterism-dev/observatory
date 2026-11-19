# Deploying the hub with Docker Compose

`deploy/compose.yaml` runs the hub with its Postgres, the WireGuard/OVN gateway and the storage server, configured
only through `deploy/.env`. A login service is not part of it: use any OIDC provider (see [idp.md](idp.md)) or the
Zitadel stack in `deploy/idp/`.

## Requirements

- A Linux amd64 host with Docker and the Compose plugin; the kernel must provide the `openvswitch` and `wireguard`
  modules (the stack loads them).
- Open ports: TCP 8090 (plain HTTP) or 80 and 443 (`tls` profile), and UDP 51820 and 51821 (nodes and devices).

## Steps

```sh
# 1. Optional: the bundled login service. Set ZITADEL_DOMAIN and its secrets in deploy/idp/.env first.
cp deploy/idp/.env.example deploy/idp/.env && $EDITOR deploy/idp/.env
docker compose -f deploy/idp/compose.yaml --env-file deploy/idp/.env up -d --wait
ZITADEL_URL=http://<idp host>:8080 sh deploy/idp/bootstrap.sh --print <hub public URL>

# 2. Configure the hub: paste the three printed OIDC lines, generate the secrets as the comments say.
cp deploy/.env.example deploy/.env && $EDITOR deploy/.env

# 3. Start it, either plain HTTP on OBSERVATORY_HTTP_PORT …
docker compose -f deploy/compose.yaml up -d --build
# … or with TLS through Caddy on 80/443.
docker compose -f deploy/compose.yaml --profile tls up -d --build
```

The bundled Zitadel serves plain HTTP; its `.env.example` holds insecure defaults that must change before the host is
reachable from a network. `bootstrap.sh` needs `curl` and `jq` and reads the admin token Zitadel writes to
`deploy/idp/.bootstrap/`. The first user to log in to the hub becomes platform admin.

## Serving modes

| Command | `deploy/.env` | Result |
| --- | --- | --- |
| `up` | `OBSERVATORY_PUBLIC_URL=http://<host>:8090` | plain HTTP, for a trusted network, a tailnet or your own reverse proxy |
| `--profile tls up` | `OBSERVATORY_TLS=acme`, `OBSERVATORY_DOMAIN`, `OBSERVATORY_PUBLIC_URL=https://<domain>` | public certificate; the domain must resolve to the host and 80/443 must be reachable |
| `--profile tls up` | `OBSERVATORY_TLS=internal`, same domain and URL | certificate from Caddy's own CA, no public DNS needed |

Behind your own proxy, forward to port 8090 and keep websockets (`/api/nodes/connect`) working.

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

## Updating

```sh
git pull
docker compose -f deploy/compose.yaml up -d --build
```

The hub migrates its database on start. Data lives in the volumes `postgres-data`, `disks`, `ovn`, `ovs` and, with
TLS, `caddy-data`.

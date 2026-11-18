# Host-bound secrets and the egress proxy

A secret variable can be bound to a few host names. A VM then never receives the value: it gets a placeholder, and a
per-VM egress proxy on the node puts the real value into HTTPS requests to exactly those hosts and removes it from the
responses. Everything else the VM does on the internet is left alone (no allowlist).

```
GH_TOKEN=obsph_rhhfld5sx222jbgys6izpeqdyegykest     # what the VM sees
gh api user                                         # works: the proxy sends the real token to api.github.com
curl -u x:$GH_TOKEN https://example.com             # example.com gets the placeholder
```

## Binding a secret

On the Environment page, check "secret" and fill in "Bind to hosts", or send `hosts` with `POST`/`PATCH
/api/orgs/{org}/env[/{id}]`:

- 1–10 exact DNS names, no wildcard, no IP literal, no port. Input is case-insensitive; names are stored lower-cased. Only secrets can be bound, and the value must
  be at least 12 bytes (shorter values would mask unrelated response bytes).
- Scopes and resolution are unchanged: a user VM gets its owner's private scope, a team VM never a private one, the
  narrowest scope wins per name ([permissions.md](permissions.md#environment-variables)).
- A VM can have at most 100 bound host names across all its secrets. Above that the hub cannot mint the VM's CA and
  logs an error; the VM runs without a proxy and its bound hosts get the placeholder and fail authentication.

The placeholder is `obsph_` followed by 32 random base32 characters. It is generated when hosts are first set and
stays the same for the variable's life (value changes, renames, moves, host edits), so hibernate, restore and fork
with memory keep working and a value change needs no reboot. It is not secret, but no endpoint returns it; a
placeholder copied into another VM is worthless unless that VM is entitled to the same secret, because each proxy
knows only its own VM's secrets.

| Change | Effect |
|---|---|
| New value, delete | Next request. |
| Bind an existing secret | The placeholder works at once; the VM keeps the real value it already has until its next boot. |
| Add a host | Next request, once the VM trusts the new CA (see [Trust](#trust-in-the-guest)). |
| Unbind (hosts emptied) | The placeholder stops working at once; the real value reaches the VM at its next boot. A running guest keeps the placeholder in its environment until it reboots. |

## What the proxy does

The node redirects every TCP connection to port 443 that a VM with bound secrets opens to the internet to that VM's
proxy (IPv4 only: the VM NAT network has no IPv6). A guest with IPv6 egress of its own reaches bound hosts directly
with the placeholder: no substitution, so nothing leaks, but those requests fail. The proxy reads the TLS ClientHello
(up to 16 KiB; nothing within 1 s or not TLS: pass through) and then:

- **Unbound name, no SNI, or ALPN other than `h2`/`http/1.1`/none:** splices the connection to the original
  destination IP and port, replaying the ClientHello byte for byte. The VM sees the server's real certificate;
  `/etc/hosts` and the VM's own DNS keep working. Nothing is logged.
- **Bound name:** terminates TLS with a leaf certificate for that name and forwards each request **by name** (the
  node's resolver, port 443, TLS 1.2+ verified against public roots). The original destination IP is ignored, so
  `curl --resolve api.github.com:443:<other IP>` still lands at GitHub and a placeholder cannot be sent to an
  attacker's server that claims a bound name. A request whose `Host`/`:authority` differs from the SNI gets
  `421 Misdirected Request` without substitution.

HTTP/1.1, HTTP/2 (including gRPC), WebSocket over HTTP/1.1, server-sent events and other streaming bodies work on
bound hosts. A proxy handles up to 1 024 connections; terminated connections close after 5 min idle.

### Substitution

- **Request header values only.** Every placeholder bound to the request's host is replaced by its value.
  `Authorization: Basic …` is decoded, substituted in user and password, and re-encoded, so `curl -u x:$TOKEN`
  and `https://user:$TOKEN@host/` in git work.
- Never in the path, query, body, trailers or WebSocket frames: a value there could be stored and published by the
  bound host (a gist, an issue). Those requests carry the placeholder.
- Requests to bound hosts get `Accept-Encoding: identity` (so responses can be scrubbed), lose
  `Sec-WebSocket-Extensions` (no per-message compression) and `Forwarded`, and get no `X-Forwarded-*`.

### Scrubbing

Every value of the VM's secrets and every header value the proxy rewrote (as sent, e.g. the re-encoded Basic string)
is overwritten with `*` of the same length wherever it appears in a response from a bound host: status headers,
1xx headers, body, trailers and WebSocket data from the server. Headers whose **name** contains a value are dropped.
Masking keeps the length, so `Content-Length`, chunking and WebSocket framing stay valid.

- gzip and deflate responses are decoded and sent on uncompressed. Any other `Content-Encoding` (br, zstd) is
  answered with `502` and never passed on unscrubbed.
- `Alt-Svc` is removed, so clients do not move a bound host to QUIC.

### Logs

The node journal (`journalctl -u observatory-node`) gets one line per request to a bound host:

```
vm <id> egress: egress vm=<id> host=httpbin.org method=GET path=/headers status=200 bytes=229 dur=98ms vars=GH_TOKEN
```

`path` is without the query, with the VM's placeholders masked as `***` (URL-escaped: `%2A%2A%2A`). Values the client
itself puts into a path are masked only while they are this VM's current values; guests normally hold only
placeholders. `vars` lists the names of the substituted variables. Header values, queries and bodies are never logged. Unbound connections are not logged. Audit rows of `env.set` contain the host names, never
values or placeholders.

## Trust in the guest

Each VM with bound secrets has its own CA: ECDSA P-256, valid 30 days, critical name constraints that permit exactly
the VM's bound hosts and exclude all IP addresses. The hub mints it (key sealed with the org key), mints a new one
when the newest has less than 15 days left or does not cover all bound hosts, and drops expired ones. The proxy
issues 24 h leaves from the **oldest** valid CA covering the host, so processes started before a rotation keep working.

- Every non-expired CA of the VM is installed as `/usr/local/share/ca-certificates/observatory-egress-<id>.crt`
  and `update-ca-certificates` runs at every boot, resume and takeoff, and while the VM runs through the guest agent's
  `trust` request. Go, curl, git, OpenSSL and Java (with `ca-certificates-java`) use the system store directly.
- Constant variables, always set in every VM, point the other runtimes at the system bundle
  `/etc/ssl/certs/ca-certificates.crt`: `SSL_CERT_FILE`, `NODE_EXTRA_CA_CERTS`, `REQUESTS_CA_BUNDLE`,
  `CURL_CA_BUNDLE`. They are in the `/etc/environment` block, `/etc/vmm/env.sh` and `DefaultEnvironment=` in
  `/etc/systemd/system.conf.d/` and `user.conf.d/`, so services get them too. They never change, so a VM's first bound
  secret needs no reboot.
- Processes read trust at start. A process started before its VM got a CA for a new host does not trust that host
  until restarted; the VM page says "Restart long-running programs to trust new hosts." until the older CA expires
  (at most 30 days).
- Fork copies the source's CAs; restore keeps the VM's current ones.

## On the node

- One proxy process per VM with bound secrets (`observatory-node egress`), started before the VM boots or resumes and
  stopped when it stops or hibernates. It runs as UID/GID `71000 + n` (n = last octet of the VM's NAT address,
  2–253, without supplementary groups), with no capabilities, an empty environment, no core dumps
  (`PR_SET_DUMPABLE 0`, `RLIMIT_CORE 0`) and `PR_SET_PDEATHSIG`. It listens on `192.168.64.1:15000 + n` and accepts
  only its VM's NAT address. A crashed proxy is restarted with backoff; meanwhile the VM's redirect is removed.
- The node sends the proxy its table (CA certs and keys, secrets) over a pipe on start and on every change; nothing is
  written to disk. Values reach the node only in the desired list of VMs scheduled on it, over the node connection.
  The unit has `MemorySwapMax=0` and `LimitCORE=0`; proxies are in the same cgroup.
- nftables tables `bridge obs_egress` (marks frames by VM tap) and `inet obs_egress` (redirect, access to proxy
  ports, outbound policy) are recreated at node start, which drops redirects left over from a previous run. A VM can
  reach only its own proxy, and only through the redirect.
- Outbound policy for proxy UIDs (`71002–71253`, nftables and the proxy's dialer): no loopback, no node addresses,
  `0.0.0.0/8`, `169.254.0.0/16`, `10.64.0.0/10`, `10.192.0.0/16`, `10.198.0.0/16`, `10.199.0.0/30`,
  `10.210.0.0/16`, `192.168.64.0/24`, `::`, `::1`, `fe80::/10`, `fc00::/7`. DNS (port 53) is allowed only to the
  nameservers in the node's `/etc/resolv.conf`, so a local resolver such as `127.0.0.53` works. The rule is built from
  that file at node start; restart the node after the nameservers change.
- After a node restart the proxies start empty; until the hub's first push, bound hosts are passed through like
  unbound ones and receive the placeholder.

See [nodes.md](nodes.md#egress-proxy) for the host-side details.

## Failures

| Failure | Behaviour |
|---|---|
| Proxy crashes or will not start | Redirect removed until it is back: unbound HTTPS keeps working (connections open through the proxy at that moment break), bound hosts get the placeholder and refuse it. |
| Hub unreachable | Proxy keeps its last table. |
| Hub cannot build a VM's secrets or CA | The VM is pushed without a proxy (logged on the hub): bound hosts get the placeholder, never the value. |
| Bound host does not resolve, refuses, bad upstream certificate | `502` with a short reason; logged. |
| Upstream redirects to another host | Passed to the client; substituted only if that host is bound too. |
| Server sends br/zstd | `502`. |
| CA expired (node clock, hub unreachable for more than 15 days) | TLS to bound hosts fails in the guest; unbound hosts are unaffected. |

## Connect check

Host-bound secrets **do not relax the connect check**. Anyone connected to a VM can make the proxy use the owner's
secrets (open a shell, run `gh`) without seeing them. Placeholders protect against a value being copied out and used
elsewhere or later; they do not make shared use of a credential safe. A user VM keeps admitting only its owner.
Future VM sharing must not reopen this.

## Security

- Root on the node sees bound traffic in clear and holds the values of the VMs on it, as it can read VM memory and
  disks. This is the node trust boundary (one org per node).
- A node only receives values of VMs scheduled on it. A node of another org never receives them.
- Snapshots, hibernate state and forks never contain real host-bound values.
- A leaked CA key can sign only for the hosts bound to its VM when it was minted (and their subdomains), for 30 days at
  most.
- A compromised process in the VM can use a bound secret against its bound hosts while it runs; it cannot take the
  value away.
- `vm:read` holders (e.g. org admins) see a VM's bound host names and how many of the owner's secrets are bound
  (`GET /api/orgs/{org}/vms/{id}/egress`), never names, values or placeholders of the secrets.

## Known limits

- **Pinning, containers, own trust stores:** tools that pin certificates, containers inside the VM and runtimes with
  their own store (e.g. a JVM without `ca-certificates-java`) do not trust the CA; requests to bound hosts fail TLS.
- **QUIC/HTTP-3, Encrypted Client Hello, non-HTTP protocols inside TLS** to bound hosts are not intercepted; they
  reach the host with the placeholder and fail authentication.
- **Plain HTTP, ports other than 443, IPv6** are never substituted.
- **Headers only:** a value needed in the path, query or body is not substituted.
- **Long-running processes** do not trust a CA added after they started (new host); restart them.
- **Services and shells can differ:** if a user overrides `SSL_CERT_FILE` (or the other trust variables) in a shell,
  services still get the constant values from `DefaultEnvironment`.
- **Unbinding** needs a guest reboot to remove the placeholder from the environment (and to deliver the value).
- **Memory snapshots older than 30 days:** processes in the restored memory trust only expired CAs and lose the bound
  hosts until restarted.
- **X.509 name constraints admit subdomains:** a CA for `github.com` is also valid for `api.github.com` in the eyes of
  TLS clients. The proxy only ever issues leaves for the exact bound names.
- **Scrubbing** matches the value byte for byte in what the proxy forwards:
  - values the server returns transformed (base64, JSON- or URL-escaped, hashed, compressed twice) are not masked;
  - a value split across WebSocket frames is not masked;
  - the proxy holds back response bytes that could start a value. When the upstream pauses for 50 ms, a held tail of up
    to 3 bytes is sent unmasked (so up to 3 leading bytes of a value can pass if the upstream pauses right there; the
    rest is masked when it arrives), and a held tail of 4 bytes or more is masked with `*`, even when it turns out not
    to be a value (innocent data that ends a message with a prefix of a secret);
  - HTTP/1.1 trailers are dropped when the upstream also sends `Content-Length`.
- **Node DNS:** the node reads `/etc/resolv.conf` once at start; restart it after the nameservers change.
- **Tailnet range:** `100.64.0.0/10` is not blocked for VMs or proxies (needed by the current test setup); a node on a
  CGNAT or tailnet underlay exposes those addresses to its VMs.
- **nftables:** the node refuses to start if it cannot load the `obs_egress` tables.

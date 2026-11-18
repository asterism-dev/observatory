# Egress proxy with secret placeholders

## Intent

Project 4 of the VM port (roadmap in `2026-11-17-vms-nodes-design.md`; projects 1–3 and 6 are built). Today every secret a VM may use is written into the guest in clear (`/etc/environment`, `/etc/vmm/env.sh`), so any process in the VM, any copy of its memory (hibernate state, snapshots, forks) and anyone who ever connected can read it and take it elsewhere. This project adds **host-bound secrets**: a secret bound to a few host names reaches the VM only as a placeholder; a per-VM egress proxy on the node replaces the placeholder with the real value in HTTPS requests to exactly those hosts and removes the real value from the responses. Everything else the VM does on the internet is left alone. Project 5 (integrations) plugs its tokens into the same mechanism.

Success: a member stores `GH_TOKEN` as a secret bound to `api.github.com` and `github.com`; in their VM `echo $GH_TOKEN` prints `obsph_…`, yet `gh api user` and `git clone https://github.com/<org>/<private repo>` work; `curl -u x:$GH_TOKEN https://example.com` sends the placeholder, not the token; `curl --resolve api.github.com:443:<some other IP>` still lands at GitHub; an endpoint echoing request headers shows a masked value, never the token; `openssl s_client -connect example.com:443` shows example.com's real certificate; the member rotates the token on the Environment page and the running `gh` session uses the new one without a reboot; a VM snapshot contains no token; a node of another org never receives it.

## Decided with the user

- Secrets reach VMs via **placeholders + an egress proxy**. **No allowlist**: VMs keep open internet egress.
- **Transparent nftables redirect** from the VM taps on the node (no `HTTPS_PROXY` in the guest).
- **Own CA** with **name constraints** and a **short lifetime**; TLS is **terminated only for hosts bound to a secret**; everything else is passed through by SNI untouched.
- **Forwarding by SNI/`Host` name, not destination IP**, so a placeholder cannot be exfiltrated to an attacker IP that claims a bound name.
- **Response scrubbing:** the real value never appears in a response back to the VM.
- **CA trust installed system-wide** in the guest: system store, `/etc/environment` and systemd, plus the CA variables of common runtimes (`NODE_EXTRA_CA_CERTS`, `SSL_CERT_FILE`, `REQUESTS_CA_BUNDLE`, …), not only login shells.
- Variables keep their scopes **org / team / private**; a user VM gets its owner's private scope, a team VM never private ones (existing rule, applies unchanged to host-bound secrets).
- **Integrations (project 5)** will support both delivery modes per integration — real secret in the VM (as in the PoC) or placeholder via this proxy, default proxy. This spec provides the hook: a **host-bound secret** = value + allowed hosts + where to substitute.
- The network spec left open whether host-bound secrets relax the connect check; decided below (they do not, *our call*).
- Nodes are not trusted across orgs; where the proxy runs is our call, justified below.

Everything else is marked *(Our call.)*

## Goals

- **Host-bound secrets:** a secret variable can carry a list of hosts (`hosts`). Such a secret reaches the VM as a stable placeholder in the same places as any variable; the real value never enters the guest.
- **Per-VM egress proxy on the node** for every VM that has at least one host-bound secret: TCP 443 from the VM's NAT tap is redirected to it; it reads the ClientHello, terminates TLS only for a bound SNI, substitutes placeholders in request headers, scrubs responses, and splices every other connection through unchanged.
- **Per-VM CA** with name constraints on exactly that VM's bound hosts, 30 days validity, rotated by the hub; trusted system-wide in the guest.
- **Immediate effect:** changing, unbinding or deleting a host-bound secret takes effect on the next request, without a reboot (plain variables still need the next boot).
- **Hook for project 5:** one hub function returns a VM's host-bound secrets from all sources; integrations add entries, the node and proxy need no change.

## Non-goals

- Egress allowlists, blocking, or logging of unbound traffic.
- Substitution over plain HTTP, in the URL (path or query) or in request bodies *(Our call: a value in a body or query can be stored by the bound host and published, e.g. a gist or an issue; every token project 5 needs travels in headers. Revisit per integration if one needs it.)*
- Wildcard hosts (`*.example.com`), IP literals, ports other than 443.
- Intercepting QUIC/HTTP-3, Encrypted Client Hello, non-HTTP protocols inside TLS to bound hosts. These reach the bound host with the placeholder and fail authentication; nothing leaks.
- Trust inside containers in the VM (own trust stores) and in tools that pin certificates; documented.
- A central (hub/gateway) proxy; IPv6 egress for VMs (the NAT is IPv4-only today).
- Relaxing the connect check (see "Connect check").

## Anti-goals

- The real value of a host-bound secret in the guest (env, seed, files, memory, snapshots), on a node's disk or swap, in any log, audit row, status message, `vm.json` or API response.
- The real value sent to any host other than its bound hosts, or to a bound host over anything but TLS verified against public roots for that name.
- A guest-trusted CA that can sign for names outside its VM's bound hosts, or for IP addresses.
- TLS to an unbound host terminated, re-signed or altered.
- A node receiving host-bound values of VMs that are not scheduled on it; one VM's connections handled with another VM's secrets.
- The proxy as a way into the node, the underlay, link-local metadata or other VMs (SSRF).
- An outage of the proxy breaking HTTPS to unbound hosts.

## Approaches

- **A) One proxy process per VM on the node, transparent redirect, terminate only bound SNI (picked).** The node already runs the VM, holds its plain variables in memory and sees all its traffic; the real values a VM is entitled to reach only the node that runs it. One process per VM means a VM that breaks its proxy finds only its own secrets in it. Only VMs with host-bound secrets get a proxy; all others are not touched at all.
- B) Central proxy at the hub or gateway. Real values never reach nodes, but every VM's HTTPS hairpins through one place (latency, bandwidth, single point of failure), and it buys nothing against the node: root on a node can already read VM memory and TLS session keys, and could speak for the VM to a central proxy just as well.
- C) One shared proxy per node, VM identified by packet mark. Less memory, but one parser bug exposes every owner's secrets on that node.
- D) Explicit `HTTPS_PROXY` in the guest. Works for some tools, ignored by many, bypassable; rejected by the transparent-redirect decision.

## Host-bound secrets

- **Model:** `hosts` on a variable of kind `secret`: 1–10 exact DNS names, lower-case, no wildcard, no IP literal, no port *(Our call)*. Empty = an ordinary secret. Env-kind variables cannot have hosts.
- **Value:** at least 12 bytes *(Our call: scrubbing shorter values would mask unrelated response bytes)*.
- **Placeholder:** `obsph_` + 32 random base32 characters, generated when hosts are first set and stable for the variable's life (survives value changes, renames, moves, host edits). Not secret, but never shown in the API either. Stable per variable so hibernate, restore and fork with memory keep working, and so a value change needs no reboot *(Our call)*.
- **Resolution:** exactly as today (`ResolveEnv`: org + own scope, `env:use`, narrowest scope wins per name). For a host-bound winner the VM's `Boot.Env` gets the placeholder and the VM's egress list gets `{placeholder, value, hosts}`. A placeholder in VM X is worthless in VM Y unless Y is entitled to the same secret: each proxy knows only its own VM's list.
- **Changes:** set hosts on an existing secret → the VM keeps the real value until its next boot (it was delivered already), the placeholder works at once. Remove hosts → the real value goes into the VM at the next boot; the placeholder stops working at once. Value change, delete → next request.

## Placement and isolation

- **Proxy process:** same binary as the node (`observatory-node egress`), one child per VM with host-bound secrets, started before the VM boots or resumes and stopped after it stops or hibernates. Runs under its own unprivileged UID (not the VM's jail UID), no capabilities, `PR_SET_PDEATHSIG`, no core dumps; restarted by the node with backoff *(Our call)*.
- **Configuration:** the node sends the VM's table (CA certs and keys, host-bound secrets) over a pipe as JSON on start and on every change; the proxy swaps it atomically. Nothing is written to disk.
- **Redirect:** a `table inet obs_egress` owned by the node. Frames from the VM's NAT tap get a per-VM mark (bridge family, so the tap and not the bridge is the key); marked TCP to port 443 not addressed to the node is redirected to the VM's proxy port on `192.168.64.1`; the input chain accepts a proxy port only with that VM's mark. A VM can therefore reach only its own proxy, and only through the redirect.
- **Outbound policy:** the proxy's upstream sockets are matched by its UID in an output chain that drops loopback, the node's own addresses, `169.254.0.0/16`, the underlay `10.198.0.0/16`, management, device and network ranges and `192.168.64.0/24` — at least everything the existing `from_vms` chain drops for forwarded VM traffic, so splicing through the node opens no new path. The proxy checks the same list in its dialer, so it fails with a clear error instead of a timeout.
- **Why the node** *(Our call)*: see Approach B. The node of org A only ever receives values of VMs scheduled on it (all org A), over the node's authenticated connection; this matches how plain variables travel today.

## Connection handling

- **Peek:** read the first TLS record (full ClientHello, also when split across segments; at most 16 KiB). Not TLS → splice to the original destination at once. Nothing received within 1 s → splice (server-speaks-first protocols on 443) *(Our call)*.
- **Unbound SNI, no SNI, or ALPN other than `h2`/`http/1.1`/none:** splice to the **original destination** IP:port, replaying the ClientHello byte-for-byte; the VM sees the real certificate. *(Our call: the decided name-based forwarding protects bound names; for unbound names there is nothing to protect, and keeping the original destination preserves `/etc/hosts`, split-horizon DNS and the VM's own resolver. Upstream for unbound traffic still obeys the outbound policy.)*
- **Bound SNI:** terminate with a leaf for that name; connect upstream **by name** (the node's resolver, Go dialer with Happy Eyeballs, IPv4 and IPv6), port 443, TLS ≥ 1.2 verified against the node's public roots, ALPN `h2`/`http/1.1`. The original destination IP is ignored.
- **Protocols on bound hosts:** HTTP/1.1, HTTP/2 (incl. gRPC with trailers), WebSocket upgrade over HTTP/1.1, server-sent events and other streaming bodies — Go's `httputil.ReverseProxy` with `Rewrite` (no `X-Forwarded-*` added) *(Our call: stdlib covers all four)*.
- **`Host` must match SNI:** a request whose `Host`/`:authority` (port stripped, case-insensitive) is not the SNI gets `421 Misdirected Request` and no substitution (defeats domain fronting; HTTP/2 clients retry on a fresh connection).
- **Limits:** request headers ≤ 1 MiB (Go default); bodies stream in both directions with no size limit (headers only are rewritten, scrubbing is length-preserving); 1 024 concurrent connections per VM proxy, beyond that new connections are refused; idle timeout 5 min for terminated connections, none for spliced ones (TCP keepalive) *(Our call)*.

## Substitution

- **Where:** request header values only. Every occurrence of a placeholder bound to this host is replaced by its value. `Authorization: Basic …` is decoded, substituted in user and password, re-encoded. Path, query, body, trailers and WebSocket frames are never touched.
- **Header restriction:** a host-bound secret may name the headers it may go into (`headers`, e.g. `["Authorization"]`); empty = any header. Variables from the Environment page use "any header"; project 5 narrows its integrations *(Our call)*.
- **Request rewrites on bound hosts:** `Accept-Encoding: identity` (so responses can be scrubbed), `Sec-WebSocket-Extensions` removed (no per-message compression).

## Scrubbing

- **What:** every real value of this VM's table plus every header value the proxy rewrote (as sent, e.g. the re-encoded Basic string), in response headers, body, trailers and WebSocket frames from the server.
- **How:** a streaming matcher with a carry-over window of (longest pattern − 1) bytes; a match is overwritten in place with `*` of the same length. Length-preserving, so `Content-Length`, chunking and WebSocket framing stay valid and nothing is buffered beyond the window *(Our call)*.
- **Encodings:** a response with `Content-Encoding` other than identity (the server ignored the request) is decoded when gzip/deflate and re-sent uncompressed; anything else (br, zstd) is answered with `502` and an explanation, never passed unscrubbed *(Our call: fail closed)*.
- `Alt-Svc` is removed from responses so clients do not move a bound host to QUIC.
- **Known limits, documented:** values the server returns transformed (base64 in a body, split across WebSocket frames, hashed); values < 12 bytes are refused at input.

## CA and guest trust

- **Per-VM CA** *(Our call)*: ECDSA P-256, `CA:TRUE, pathlen:0`, validity 30 days, critical name constraints: permitted DNS names = the VM's bound hosts at issue time, excluded IP ranges `0.0.0.0/0` and `::/0`. Minted by the hub, key sealed with the org key (AAD `egress-ca:<vm id>:<ca id>`), pushed with the VM in the desired list; the node keeps it in memory and in the proxy only.
- **Rotation:** the hub mints a new CA when the newest has < 15 days left or does not cover all bound hosts; expired CAs are dropped. The guest trusts every non-expired CA of its VM. The proxy issues leaves (P-256, SAN = the host, 24 h, cached in memory) from the **oldest** valid CA that covers the host, so processes started before a rotation keep working until that CA expires.
- **Fork** copies the source's CAs (same owner, same secrets; processes in the forked memory trust them). Restore keeps the VM's current CAs; a memory snapshot older than 30 days loses bound hosts in its old processes (documented).
- **Guest:** each CA cert goes to `/usr/local/share/ca-certificates/observatory-egress-<id>.crt` and `update-ca-certificates` runs — at every boot/resume/takeoff from the seed (`vmm-boot.sh`) and, while running, through a new guest agent request `trust {certs[]}` over vsock (certs are public; values never travel over vsock).
- **Variables, constant and always set:** `SSL_CERT_FILE`, `NODE_EXTRA_CA_CERTS`, `REQUESTS_CA_BUNDLE`, `CURL_CA_BUNDLE` all point at the system bundle `/etc/ssl/certs/ca-certificates.crt`, in the `/etc/environment` block, `/etc/vmm/env.sh`, and `DefaultEnvironment=` in `/etc/systemd/system.conf.d/` and `user.conf.d/` *(Our call: pointing at the system bundle means the file always exists and the variables never change, so a VM's first bound secret needs no reboot)*. Go, curl, git, OpenSSL and Java (via the `ca-certificates-java` hook when installed) use the system store directly.
- Processes read trust at start: a process started before a CA was added does not trust it until restarted (documented; the UI says so when a VM gets its first bound secret).

## Values on the node

- Host-bound values and CA keys travel only in the desired list over the node connection (`wss://` in production) and live only in the node's and the proxy's memory: not in `vm.json`, the seed, the console log or any status message.
- The installer adds `MemorySwapMax=0` and `LimitCORE=0` to the node unit; proxies inherit the cgroup *(Our call: guarantees "never on disk in clear" without mlock tricks)*.
- After a node restart the proxies start empty; until the hub's first push bound hosts are spliced like unbound ones (placeholder reaches the provider, which refuses it).

## Connect check

- **Not relaxed** *(Our call)*. Anyone connected to a VM can make the proxy use the owner's secret — open a shell, run `gh` — even without seeing it. Placeholders protect against the value being copied out and used elsewhere or later; they do not make shared use of a credential safe. A user VM keeps admitting only its owner. Recorded in `docs/egress.md` so that VM shares (later) do not reopen it by accident.

## Hook for integrations

- `proto.DesiredVM` gains `Egress *Egress{CAs []EgressCA{ID, CertPEM, KeyPEM}, Secrets []HostBoundSecret}`, with `HostBoundSecret{ID, Name, Placeholder, Value, Hosts []string, Headers []string}`.
- Hub: `EgressSecrets(ctx, org, vm)` builds the list; in this project from host-bound variables. Project 5 appends its integrations in proxy mode (e.g. Claude: `api.anthropic.com`, `Authorization`; GitHub: `github.com`, `api.github.com`, `Authorization`) and puts their placeholders where the guest expects the token. Integrations in "real secret" mode do not use this hook. The CA's host set is computed from the merged list.

## Data model (outline)

- `env_vars`: `hosts text[]` (null = not host-bound; check: only kind `secret`), `placeholder text` (unique, set with hosts).
- `vm_egress_cas`: id, org_id, vm_id (cascade), hosts text[], cert_pem, nonce, key_ciphertext, not_before, not_after.
- Proxy tables, leaf caches, connection state: node memory only.

## Failure semantics

| Failure | Behaviour |
|---|---|
| Proxy crashes or will not start | Node removes the VM's redirect until the proxy is back (unbound HTTPS keeps working, bound hosts get the placeholder and fail auth); restart with backoff. |
| Hub unreachable | Proxy keeps its last table; after a node restart it is empty until the first push (see above). |
| Bound host does not resolve / refuses / bad upstream cert | `502` to the client with a short reason; logged. |
| Upstream redirects to another host | Passed to the client; its next request goes to that host, substituted only if that host is bound too. |
| Server sends br/zstd despite `identity` | `502`, never unscrubbed. |
| Leaf CA expired (node clock, hub gone > 15 days) | TLS to bound hosts fails in the guest; unbound unaffected. |
| VM hibernated, paused, suspended | Proxy follows the VM process; spliced connections die with the taps as today. |

## Logging and audit

- Proxy log line per request to a bound host (node journal, via the node): VM id, host, method, path **without query**, status, bytes, duration, names of substituted variables. Never header values, query, bodies or values; Go's server error log is routed through a filter that drops request contents *(Our call)*.
- Unbound connections are not logged (no allowlist; counters only, if any).
- Audit: `env.set` details gain `hosts` (names, never values or placeholders); `vm_egress_cas` minting is logged, not audited.

## Security

- Root on the node sees bound traffic in clear and holds the values of VMs on it; as with VM memory and disks today, this is the node trust boundary (per org) and documented.
- Snapshots, hibernate state and forks never contain real host-bound values — a side benefit over plain secrets.
- The CA cannot be abused beyond the VM's bound hosts even if its key leaks (name constraints, 30 days, one VM).
- A compromised process in the VM can still use a bound secret against its bound hosts while it runs; it cannot take the value away.

## Permissions

- No new actions. Setting or changing `hosts` needs `env:set` on the variable's scope, like the value; unbinding (which delivers the real value at next boot) too. `env:list` shows hosts. Defaults unchanged.
- Placeholders are not returned by any endpoint *(Our call: not secret, but nobody needs them outside the VM)*.

## UI

- **Environment page:** new **Hosts** column (chips, `—` for none). Add/Edit dialog: when "secret" is checked, "Bind to hosts (VMs get a placeholder; the value is only sent to these hosts over HTTPS)" with a host list input; validation as above. Unbinding asks for confirmation ("VMs will receive the real value at their next boot").
- **VM detail:** "Egress" line: number of host-bound secrets and their hosts, CA expiry; hint when trust changed while running ("restart long-running programs to trust new hosts").
- Actions shown only when permitted (`can()`); de + en.

```
NAME              TYPE     SCOPE          HOSTS                         VALUE
GH_TOKEN          secret   private        github.com  api.github.com   ••••••   Edit  Move  Delete
OPENAI_API_KEY    secret   team Backend   api.openai.com                ••••••   Edit  Move  Delete
DATABASE_URL      secret   team Backend   —                             ••••••   Edit  Move  Delete
```

## Left to the builder

- Exact nftables rules (bridge mark, nat redirect, input and UID output chains), UID allocation for proxy processes, proxy port allocation.
- REST payload change for `hosts`; how `EgressSecrets` shares work with `boot()` in `internal/fleet`.
- One cert per file for `update-ca-certificates`; whether `DefaultEnvironment` needs `daemon-reexec` on first install.
- Test strategy; must include: host-bound value never in `Boot.Env`, seed, `vm.json`, logs or API; placeholder of VM X ignored by VM Y's proxy; Basic and Bearer substitution, header restriction; `Host` ≠ SNI → 421; `--resolve` to another IP still reaches the bound name; unbound passthrough byte-identical (real certificate, ClientHello replay, non-TLS and server-speaks-first); scrubbing across chunk and frame boundaries, length-preserving, of rewritten Basic headers; br/zstd → 502; upstream to loopback/underlay/link-local refused for bound and spliced connections; one VM cannot reach another VM's proxy port; CA name constraints (Go, OpenSSL and Node reject a leaf outside the hosts and an IP SAN); rotation keeps old processes working; value change effective without reboot; proxy crash removes the redirect; org B's node never receives org A's values; permission table incl. deny rules.
- **Real-hardware run on Proxmox** (test VM 108 as node of org A, 107 as node of org B; hub, gateway and storage on the Mac reached over the tailnet; nested KVM): a secret bound to `httpbin.org` → basic-auth endpoint 200 with the placeholder, echo endpoints show the mask, `--resolve` to another IP still reaches httpbin; `curl`, Node, Python `requests`, Go and `git clone` of a private repo with a bound GitHub PAT; Claude Code with `CLAUDE_CODE_OAUTH_TOKEN` as placeholder bound to `api.anthropic.com` (streaming); `curl --http2`, a WebSocket echo; `openssl s_client` to an unbound host shows its real chain; a systemd service (not a login shell) reaches a bound host; value rotated while a loop keeps calling the API; hibernate/resume and `systemctl restart observatory-node` with the loop running; throughput of a 1 GB download from an unbound host with and without proxy (target within 5 %), added latency on a bound host (target < 5 ms); dump the desired messages on 107 to show no org A values.
- Add `docs/egress.md` (how placeholders work, what is substituted where, trust variables, known limits: pinning, containers, QUIC/ECH, transformed values, long-running processes); update `docs/permissions.md` (hosts need `env:set`).

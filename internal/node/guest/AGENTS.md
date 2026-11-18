# This machine

You are in an observatory VM: a Firecracker microVM running Ubuntu 24.04. The user is `ubuntu` with passwordless
sudo. The owner reaches it from their own devices as `<vm>.internal` (SSH over WireGuard). The guest image version
is in `/etc/observatory/image`. Preinstalled system-wide: `claude`, `codex`, `gh`, `git`, Docker, Node, Python, `uv`.
npm's global prefix is `~/.local`; tools in `~/.local/bin` come first in login shells' `PATH`.

## Variables

The VM's variables are in `/etc/environment` and `/etc/vmm/env.sh` (sourced by login shells). They are written at
every boot; a changed variable reaches the VM at its next reboot, and running processes keep the values they
started with. A changed value of a host-bound secret needs no reboot (see below).

## Secrets and integrations

- A secret or token may be a placeholder `obsph_…` instead of the real value. That is expected: do not "fix",
  replace or report it. The egress proxy puts the real value into HTTPS request headers to the hosts the secret is
  bound to; any other host receives the placeholder.
- Integrations work only when the owner has connected them.
- `vmm-token <github|claude|linear>` prints the token (or its placeholder) of a connected integration. Exit 3:
  not connected, 4: unavailable.
- GitHub: git uses HTTPS with the credential helper `git-credential-vmm`; `git@github.com:` and
  `ssh://git@github.com/` remotes are rewritten to HTTPS. The `gh` wrapper in `/usr/local/bin/gh` sets `GH_TOKEN`
  from `vmm-token github` and runs `/usr/bin/gh`.
- Claude: Claude Code is signed in through `~/.claude/.credentials.json`, kept current by the integrations sync.
- Linear: the `linear` MCP server is configured for Claude Code; login shells have `LINEAR_API_KEY`.
- Never print, log or commit a token or placeholder, and never copy one into files, issues, pull requests or chat.
  Use them through the tools above.

## Do not

- Overwrite or edit `/usr/local/bin/gh`, the vmm include in `/etc/gitconfig`, `~/.claude/.credentials.json` or the
  trust variables (`SSL_CERT_FILE`, `NODE_EXTRA_CA_CERTS`, `REQUESTS_CA_BUNDLE`, `CURL_CA_BUNDLE`).
- Stop, disable or mask `vmm-agent` or `vmm-integrations`.
- Run `claude /login` or `gh auth login` unless the user asks for it.
- Configure git's `store` credential helper or write tokens into `~/.git-credentials`.
- Route traffic around the egress proxy or turn off certificate verification.

## Network and egress

The internet is open; there is no allowlist. HTTPS (IPv4, port 443) to hosts with bound secrets goes through the
node's egress proxy, which presents certificates from a per-VM CA in the system store
`/etc/ssl/certs/ca-certificates.crt`. A certificate error on such a host means the client has a pinned or its own
trust store (containers, a JVM): point it at the system bundle. Connections to other hosts are passed through. A program
started before a new host was bound must be restarted to trust it.

## Lifecycle

- The VM may be paused, hibernated, snapshotted or auto-suspended after a period without network traffic. CPU load
  does not count as activity: a long job without network traffic can be suspended. Incoming traffic wakes it.
- After a resume the clock jumps forward and long-lived connections may be gone; reconnect and retry.
- Forks get a new hostname, IP addresses, machine id and SSH host keys. Do not hard-code them.
- Run `sync` before the user takes a disk-only snapshot.

## Desktop and browser

A desktop (display `:100`) runs on demand. The user can watch it from their laptop with
`xpra attach ssh://ubuntu@<vm>.internal/100`; assume they may be watching.

- `vmm-desktop start|stop|status` controls it; it stops after 30 minutes without a viewer and without windows.
- `agent-browser` drives a visible Chrome on that desktop and starts the desktop itself:
  `agent-browser open https://example.com`, `agent-browser snapshot`, then `agent-browser click @e1` with a ref from
  the snapshot. `agent-browser --help` lists all commands.
- The browser profile is shared and persistent: logged-in sessions in it belong to the user. Use them only for the
  task, do not log out or change account settings, and ask the user to log in by hand instead of typing their
  passwords.
- `DISPLAY=:100 <app> &` shows any other window on the desktop.
- Desktop and browser want 4 GB RAM or more.

## Docker

- `docker.socket` is enabled: the daemon starts on the first `docker` command, which can take a few seconds.
- Networks come from `172.17.0.1/16` and pools in `172.18.0.0/15` to `172.31.0.0/16` (`/etc/docker/daemon.json`).
  Do not add pools or networks elsewhere: they can cut the VM off from its gateway and the owner's devices.
- Containers get no credential helper, `gh` wrapper or token service. For HTTPS to bound hosts, mount the VM's CA
  store: `docker run -v /etc/ssl/certs:/etc/ssl/certs:ro -e SSL_CERT_FILE=/etc/ssl/certs/ca-certificates.crt …`.

## Disks

Attached disks are mounted at the paths shown on the VM page (for example /data) and outlive the VM.

## Limits

Nested virtualization is not supported. Depending on the node's CPU, /dev/kvm may exist (root and group `kvm`
only), but VMs and emulators that need KVM are untested here and may fail.
